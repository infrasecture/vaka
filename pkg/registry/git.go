package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/recipe"
)

const (
	maxGitOutputBytes     = 8 << 20
	maxGitRecipes         = 1000
	maxGitRecipeEntries   = 2000
	maxGitRecipeFileBytes = 20 << 20
	maxGitRecipeBytes     = 50 << 20
)

// These are aggregate limits, in addition to the per-recipe extraction limits.
// Vars let focused tests exercise the failure paths without large fixtures.
var (
	maxGitObjectStoreBytes   int64 = 512 << 20
	maxGitArtifactCacheBytes int64 = 512 << 20
	gitArtifactGracePeriod         = 24 * time.Hour
)

var gitCommitRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// fetchGitIndexFromCache deliberately performs no Git operation. A mutable
// preview ref advances only through RefreshIndex, making ordinary get/search
// commands reproducible against the last explicitly selected commit.
func (c *Client) fetchGitIndexFromCache(reg Registry) (*IndexResult, error) {
	dir, err := c.cacheDir()
	if err != nil {
		return nil, err
	}
	if err := hardenExistingGitCache(filepath.Join(dir, reg.Name)); err != nil {
		return nil, fmt.Errorf("registry %q: secure preview cache: %w", reg.Name, err)
	}
	env, age, ok := readIndexCache(indexCachePath(dir, reg.Name), reg.sourceIdentity())
	if !ok {
		return nil, fmt.Errorf("registry %q has no resolved Git preview cache; run `vaka registry refresh %s`", reg.Name, reg.Name)
	}
	idx, err := ParseIndex(env.Index)
	if err != nil {
		return nil, fmt.Errorf("registry %q: cached Git preview index: %w", reg.Name, err)
	}
	if !gitCommitRE.MatchString(env.Revision) {
		return nil, fmt.Errorf("registry %q: cached Git preview has an invalid source revision", reg.Name)
	}
	for name, versions := range idx.Recipes {
		if !nameRE.MatchString(name) || len(versions) != 1 || versions[0].SourceRevision != env.Revision {
			return nil, fmt.Errorf("registry %q: cached Git preview provenance is inconsistent; run `vaka registry refresh %s`", reg.Name, reg.Name)
		}
	}
	return &IndexResult{Index: idx, Revision: env.Revision, Age: age}, nil
}

func (c *Client) refreshGitIndex(ctx context.Context, reg Registry) (*IndexResult, error) {
	if err := validateRegistry(reg); err != nil {
		return nil, err
	}
	cacheDir, err := c.cacheDir()
	if err != nil {
		return nil, err
	}
	regDir := filepath.Join(cacheDir, reg.Name)
	if err := ensurePrivateGitCacheDir(regDir); err != nil {
		return nil, fmt.Errorf("registry %q: create preview cache: %w", reg.Name, err)
	}
	unlock, err := acquireGitRefreshLock(regDir)
	if err != nil {
		return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
	}
	defer unlock()
	if err := hardenGitCache(regDir); err != nil {
		return nil, fmt.Errorf("registry %q: secure preview cache: %w", reg.Name, err)
	}

	res, err := c.buildGitIndex(ctx, reg, regDir)
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Match the published-index offline behavior: a failed refresh never
	// destroys the last complete cache. It remains usable but is marked stale.
	stale, staleErr := c.fetchGitIndexFromCache(reg)
	if staleErr != nil {
		return nil, fmt.Errorf("registry %q: refresh Git preview: %w (and no cached snapshot exists)", reg.Name, err)
	}
	stale.Stale = true
	stale.FallbackReason = err.Error()
	return stale, nil
}

func (c *Client) buildGitIndex(ctx context.Context, reg Registry, regDir string) (*IndexResult, error) {
	c.gitProgressf(reg, "preparing local cache")
	oldDigests := c.cachedArtifactDigests(reg)
	pruneGitArtifacts(regDir, oldDigests, time.Now().Add(-gitArtifactGracePeriod))
	if err := checkGitArtifactCache(regDir); err != nil {
		return nil, err
	}

	c.gitProgressf(reg, "fetching %s#%s", reg.Git.URL, reg.Git.Ref)
	repoDir, commit, commitTime, err := fetchGitCommit(ctx, reg.Git)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(repoDir)
	c.gitProgressf(reg, "resolved ref to commit %s", shortRevision(commit))

	c.gitProgressf(reg, "discovering top-level recipes")
	names, err := discoverGitRecipes(ctx, repoDir, commit)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("commit %s contains no top-level recipe directories", shortRevision(commit))
	}
	c.gitProgressf(reg, "found %d top-level recipe(s)", len(names))

	newDigests := make(map[string]bool, len(names))
	createdDigests := make(map[string]bool, len(names))
	committed := false
	defer func() {
		if committed {
			return
		}
		for digest := range createdDigests {
			_ = os.Remove(filepath.Join(regDir, "artifacts", digest+".tar.gz"))
		}
	}()
	idx := &Index{
		APIVersion: APIVersion,
		Kind:       "RegistryIndex",
		Generated:  commitTime,
		Recipes:    make(map[string][]IndexEntry, len(names)),
	}

	for i, name := range names {
		c.gitProgressf(reg, "packaging recipe %q (%d/%d)", name, i+1, len(names))
		artifact, digest, err := packageGitRecipe(ctx, repoDir, commit, name, regDir)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}
		c.gitProgressf(reg, "validating recipe %q (%d/%d)", name, i+1, len(names))
		manifest, summary, err := validateGitRecipeArtifact(ctx, artifact, name)
		if err != nil {
			os.Remove(artifact)
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}
		if err := manifest.CheckIdentity(recipe.ExpectedIdentity{Name: name}); err != nil {
			os.Remove(artifact)
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}

		artifactPath, created, err := installGitArtifact(regDir, artifact, digest)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: cache artifact: %w", name, err)
		}
		hexDigest := strings.TrimPrefix(digest, "sha256:")
		newDigests[hexDigest] = true
		if created {
			createdDigests[hexDigest] = true
		}
		if err := checkGitArtifactCache(regDir); err != nil {
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}
		idx.Recipes[name] = []IndexEntry{{
			Version:        manifest.Version,
			Description:    manifest.Description,
			Tags:           append([]string(nil), manifest.Tags...),
			Created:        commitTime,
			Digest:         digest,
			URLs:           []string{fileURL(artifactPath)},
			MinVakaVersion: manifest.MinVakaVersion,
			Env:            manifestEnvToIndex(manifest.Env),
			Policy: &PolicySummary{
				DefaultActions: summary.DefaultActions,
				RiskFlags:      summary.RiskFlags,
			},
			SourceRevision: commit,
		}}
	}

	c.gitProgressf(reg, "writing local snapshot")
	data, err := yaml.Marshal(idx)
	if err != nil {
		return nil, fmt.Errorf("encode generated preview index: %w", err)
	}
	if int64(len(data)) > maxIndexBytes {
		return nil, fmt.Errorf("generated preview index exceeds the %d byte limit", maxIndexBytes)
	}
	if _, err := ParseIndex(data); err != nil {
		return nil, fmt.Errorf("validate generated preview index: %w", err)
	}
	// Give every reader of the current index a full grace period to open its
	// artifact, even when several refreshes happen in quick succession.
	if err := touchGitArtifacts(regDir, oldDigests, time.Now()); err != nil {
		return nil, fmt.Errorf("retain previous preview artifacts: %w", err)
	}
	if err := writeIndexCache(indexCachePath(filepath.Dir(regDir), reg.Name), indexCache{
		Source:   reg.sourceIdentity(),
		Revision: commit,
		Index:    data,
	}); err != nil {
		return nil, fmt.Errorf("write generated preview index: %w", err)
	}
	committed = true

	// Unreferenced artifacts remain available for a bounded grace period. This
	// avoids invalidating a reader that loaded an older atomic index while still
	// allowing future refreshes to reclaim storage.
	pruneGitArtifacts(regDir, newDigests, time.Now().Add(-gitArtifactGracePeriod))
	return &IndexResult{Index: idx, Revision: commit}, nil
}

func (c *Client) gitProgressf(reg Registry, format string, args ...any) {
	if c.Progress == nil {
		return
	}
	fmt.Fprintf(c.Progress, "vaka: Git preview %q: %s\n", reg.Name, fmt.Sprintf(format, args...))
}

func acquireGitRefreshLock(regDir string) (func(), error) {
	lockDir := filepath.Join(filepath.Dir(regDir), ".locks")
	if err := ensurePrivateGitCacheDir(lockDir); err != nil {
		return nil, err
	}
	path := gitRefreshLockPath(regDir)
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("Git preview refresh lock is a symlink; refusing it")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another Git preview refresh or removal is already running")
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func gitRefreshLockPath(regDir string) string {
	return filepath.Join(filepath.Dir(regDir), ".locks", filepath.Base(regDir)+".lock")
}

func fetchGitCommit(ctx context.Context, source *GitSource) (repoDir, commit, commitTime string, err error) {
	repoDir, err = os.MkdirTemp("", "vaka-git-registry-*")
	if err != nil {
		return "", "", "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(repoDir)
		}
	}()
	if _, err = runGit(ctx, "", "init", "--quiet", "--bare", "--template=", repoDir); err != nil {
		return "", "", "", fmt.Errorf("initialize temporary Git object store: %w", err)
	}
	if _, err = runGit(ctx, repoDir, "remote", "add", "origin", source.URL); err != nil {
		return "", "", "", fmt.Errorf("configure Git preview remote: %w", err)
	}
	// Persist promisor configuration so later cat-file reads can fetch only the
	// selected recipe blobs on demand. Servers that ignore filtering remain
	// bounded by the aggregate temporary object-store limit.
	if _, err = runGit(ctx, repoDir, "config", "remote.origin.promisor", "true"); err != nil {
		return "", "", "", fmt.Errorf("configure Git promisor remote: %w", err)
	}
	if _, err = runGit(ctx, repoDir, "config", "remote.origin.partialclonefilter", "blob:none"); err != nil {
		return "", "", "", fmt.Errorf("configure Git partial-clone filter: %w", err)
	}
	fetchRef := source.Ref
	if !strings.HasPrefix(fetchRef, "refs/") && !gitCommitRE.MatchString(fetchRef) {
		fetchRef = "refs/heads/" + fetchRef
	}
	if _, err = runGitWithStoreLimit(ctx, repoDir, "fetch", "--quiet", "--no-tags", "--depth=1", "--filter=blob:none", "origin", fetchRef); err != nil {
		return "", "", "", fmt.Errorf("fetch ref %q: %w", source.Ref, err)
	}
	rawCommit, err := runGit(ctx, repoDir, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", "", "", fmt.Errorf("resolve fetched ref %q to a commit: %w", source.Ref, err)
	}
	commit = strings.TrimSpace(string(rawCommit))
	if !gitCommitRE.MatchString(commit) {
		return "", "", "", fmt.Errorf("Git returned invalid commit ID %q", commit)
	}
	rawTime, err := runGit(ctx, repoDir, "show", "-s", "--format=%cI", commit)
	if err != nil {
		return "", "", "", fmt.Errorf("read commit timestamp: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(string(rawTime)))
	if err != nil {
		return "", "", "", fmt.Errorf("parse commit timestamp: %w", err)
	}
	commitTime = parsed.UTC().Format(time.RFC3339)
	ok = true
	return repoDir, commit, commitTime, nil
}

func discoverGitRecipes(ctx context.Context, repoDir, commit string) ([]string, error) {
	raw, err := runGit(ctx, repoDir, "ls-tree", "-z", commit)
	if err != nil {
		return nil, fmt.Errorf("list commit tree: %w", err)
	}
	var names []string
	for _, record := range strings.Split(string(raw), "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "tree" || !nameRE.MatchString(name) {
			continue
		}
		direct, err := runGit(ctx, repoDir, "ls-tree", "-z", commit+":"+name, "--", "recipe.yaml")
		if err != nil {
			return nil, fmt.Errorf("inspect recipe tree %q: %w", name, err)
		}
		if !gitTreeContainsPath(direct, "recipe.yaml") {
			continue
		}
		names = append(names, name)
		if len(names) > maxGitRecipes {
			return nil, fmt.Errorf("commit contains more than %d top-level recipes", maxGitRecipes)
		}
	}
	sort.Strings(names)
	return names, nil
}

func validateGitRecipeArtifact(ctx context.Context, artifact, name string) (*recipe.Manifest, *recipe.LocalPolicySummary, error) {
	dir, err := os.MkdirTemp("", "vaka-git-recipe-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	root, err := recipe.OpenSafeRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(artifact)
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	err = recipe.ExtractRecipe(f, name, root)
	f.Close()
	root.Close()
	if err != nil {
		return nil, nil, err
	}
	if err := recipe.ValidateStaged(ctx, dir, recipe.ExpectedIdentity{Name: name}); err != nil {
		return nil, nil, err
	}
	manifestData, err := os.ReadFile(filepath.Join(dir, "recipe.yaml"))
	if err != nil {
		return nil, nil, err
	}
	manifest, err := recipe.ParseManifest(manifestData)
	if err != nil {
		return nil, nil, err
	}
	summary, err := recipe.LintDir(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	return manifest, summary, nil
}

func installGitArtifact(regDir, tmpPath, digest string) (string, bool, error) {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if len(hexDigest) != 64 {
		os.Remove(tmpPath)
		return "", false, fmt.Errorf("invalid artifact digest %q", digest)
	}
	dir := filepath.Join(regDir, "artifacts")
	if err := ensurePrivateGitCacheDir(dir); err != nil {
		os.Remove(tmpPath)
		return "", false, err
	}
	final := filepath.Join(dir, hexDigest+".tar.gz")
	if info, err := os.Lstat(final); err == nil {
		if !info.Mode().IsRegular() {
			os.Remove(tmpPath)
			return "", false, fmt.Errorf("cached artifact %q is not a regular file", final)
		}
		if got, err := fileDigest(final); err == nil && got == digest {
			if err := os.Chmod(final, 0o600); err != nil {
				os.Remove(tmpPath)
				return "", false, err
			}
			os.Remove(tmpPath)
			return final, false, nil
		}
	} else if !os.IsNotExist(err) {
		os.Remove(tmpPath)
		return "", false, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		os.Remove(tmpPath)
		return "", false, err
	}
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return "", false, err
	}
	return final, true, nil
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Client) cachedArtifactDigests(reg Registry) map[string]bool {
	keep := map[string]bool{}
	dir, err := c.cacheDir()
	if err != nil {
		return keep
	}
	data, _ := cachedIndexBytes(dir, reg)
	idx, err := ParseIndex(data)
	if err != nil {
		return keep
	}
	for _, versions := range idx.Recipes {
		for _, entry := range versions {
			if digestRE.MatchString(entry.Digest) {
				keep[strings.TrimPrefix(entry.Digest, "sha256:")] = true
			}
		}
	}
	return keep
}

func pruneGitArtifacts(regDir string, keep map[string]bool, cutoff time.Time) {
	dir := filepath.Join(regDir, "artifacts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		hexDigest, ok := gitArtifactDigest(name)
		if !ok || keep[hexDigest] {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func gitArtifactDigest(name string) (string, bool) {
	hexDigest, ok := strings.CutSuffix(name, ".tar.gz")
	if !ok || len(hexDigest) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", false
	}
	return hexDigest, true
}

func touchGitArtifacts(regDir string, digests map[string]bool, now time.Time) error {
	for digest := range digests {
		path := filepath.Join(regDir, "artifacts", digest+".tar.gz")
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%q is not a regular file", path)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		if err := os.Chtimes(path, now, now); err != nil {
			return err
		}
	}
	return nil
}

func checkGitArtifactCache(regDir string) error {
	dir := filepath.Join(regDir, "artifacts")
	if _, err := directorySizeAtMost(dir, maxGitArtifactCacheBytes); err != nil {
		return fmt.Errorf("Git preview artifact cache: %w", err)
	}
	return nil
}

func ensurePrivateGitCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is not a real directory", dir)
	}
	return os.Chmod(dir, 0o700)
}

func hardenExistingGitCache(regDir string) error {
	if _, err := os.Lstat(regDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return hardenGitCache(regDir)
}

func hardenGitCache(regDir string) error {
	if err := ensurePrivateGitCacheDir(regDir); err != nil {
		return err
	}
	artifactsDir := filepath.Join(regDir, "artifacts")
	if err := ensurePrivateGitCacheDir(artifactsDir); err != nil {
		return err
	}
	lockPath := gitRefreshLockPath(regDir)
	if err := ensurePrivateGitCacheDir(filepath.Dir(lockPath)); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(regDir, "cache.yaml"), lockPath} {
		if err := hardenGitCacheFile(path); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(artifactsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, ok := gitArtifactDigest(entry.Name()); !ok {
			continue
		}
		if err := hardenGitCacheFile(filepath.Join(artifactsDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func hardenGitCacheFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	return os.Chmod(path, 0o600)
}

func manifestEnvToIndex(in []recipe.ManifestEnv) []EnvVar {
	out := make([]EnvVar, len(in))
	for i, env := range in {
		out[i] = EnvVar{
			Name: env.Name, Required: env.Required, Default: env.Default, Description: env.Description,
		}
	}
	return out
}

func fileURL(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func gitCommand(ctx context.Context, repoDir string, args ...string) *exec.Cmd {
	base := make([]string, 0, len(args)+4)
	if repoDir != "" {
		base = append(base, "-C", repoDir)
	}
	base = append(base, "-c", "core.hooksPath=/dev/null")
	base = append(base, args...)
	cmd := exec.CommandContext(ctx, "git", base...)
	cmd.Env = gitEnvironment()
	return cmd
}

func gitEnvironment() []string {
	// Repository-selection variables would override -C and could redirect a
	// Git operation into an unrelated user repository. Keep authentication
	// inputs (credential helpers, SSH agent/command, askpass) intact, but remove
	// variables that change the object store, worktree, refs, or injected -c
	// configuration of Vaka's Git subprocesses.
	blocked := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_INDEX_FILE": true, "GIT_GRAFT_FILE": true, "GIT_REPLACE_REF_BASE": true,
		"GIT_NAMESPACE": true, "GIT_QUARANTINE_PATH": true, "GIT_SHALLOW_FILE": true,
		"GIT_CONFIG_COUNT": true,
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if blocked[key] || strings.HasPrefix(key, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(key, "GIT_CONFIG_VALUE_") || key == "GIT_TERMINAL_PROMPT" {
			continue
		}
		env = append(env, item)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

func runGit(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cmd := gitCommand(ctx, repoDir, args...)
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stdout.exceeded {
		return nil, fmt.Errorf("Git output exceeds the %d byte limit", maxGitOutputBytes)
	}
	if err != nil {
		return nil, gitError(err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func gitError(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

type cappedBuffer struct {
	data     []byte
	exceeded bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	available := maxGitOutputBytes - len(b.data)
	if available > 0 {
		n := available
		if n > len(p) {
			n = len(p)
		}
		b.data = append(b.data, p[:n]...)
	}
	if len(p) > available {
		b.exceeded = true
		return available, fmt.Errorf("Git output exceeds the %d byte limit", maxGitOutputBytes)
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte  { return b.data }
func (b *cappedBuffer) String() string { return string(b.data) }
