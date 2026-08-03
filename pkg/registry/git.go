package registry

import (
	"archive/tar"
	"compress/gzip"
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

var gitCommitRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// fetchGitIndexFromCache deliberately performs no Git operation. A mutable
// preview ref advances only through RefreshIndex, making ordinary get/search
// commands reproducible against the last explicitly selected commit.
func (c *Client) fetchGitIndexFromCache(reg Registry) (*IndexResult, error) {
	dir, err := c.cacheDir()
	if err != nil {
		return nil, err
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
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		return nil, fmt.Errorf("registry %q: create preview cache: %w", reg.Name, err)
	}
	unlock, err := acquireGitRefreshLock(regDir)
	if err != nil {
		return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
	}
	defer unlock()

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
	repoDir, commit, commitTime, err := fetchGitCommit(ctx, reg.Git)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(repoDir)

	names, err := discoverGitRecipes(ctx, repoDir, commit)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("commit %s contains no top-level recipe directories", shortRevision(commit))
	}

	oldDigests := c.cachedArtifactDigests(reg)
	newDigests := make(map[string]bool, len(names))
	idx := &Index{
		APIVersion: APIVersion,
		Kind:       "RegistryIndex",
		Generated:  commitTime,
		Recipes:    make(map[string][]IndexEntry, len(names)),
	}

	for _, name := range names {
		artifact, digest, err := packageGitRecipe(ctx, repoDir, commit, name, regDir)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}
		manifest, summary, err := validateGitRecipeArtifact(ctx, artifact, name)
		if err != nil {
			os.Remove(artifact)
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}
		if err := manifest.CheckIdentity(recipe.ExpectedIdentity{Name: name}); err != nil {
			os.Remove(artifact)
			return nil, fmt.Errorf("recipe %s: %w", name, err)
		}

		artifactPath, err := installGitArtifact(regDir, artifact, digest)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: cache artifact: %w", name, err)
		}
		newDigests[strings.TrimPrefix(digest, "sha256:")] = true
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
	if err := writeIndexCache(indexCachePath(filepath.Dir(regDir), reg.Name), indexCache{
		Source:   reg.sourceIdentity(),
		Revision: commit,
		Index:    data,
	}); err != nil {
		return nil, fmt.Errorf("write generated preview index: %w", err)
	}

	// Retain the current and immediately previous artifact sets. Readers that
	// loaded the old atomic index just before refresh can still fetch its
	// content, while repeated branch updates do not grow the cache without bound.
	for digest := range oldDigests {
		newDigests[digest] = true
	}
	pruneGitArtifacts(regDir, newDigests)
	return &IndexResult{Index: idx, Revision: commit}, nil
}

func acquireGitRefreshLock(regDir string) (func(), error) {
	path := filepath.Join(regDir, "git-refresh.lock")
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("Git preview refresh lock is a symlink; refusing it")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another Git preview refresh is already running")
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
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
	fetchRef := source.Ref
	if !strings.HasPrefix(fetchRef, "refs/") && !gitCommitRE.MatchString(fetchRef) {
		fetchRef = "refs/heads/" + fetchRef
	}
	if _, err = runGit(ctx, repoDir, "fetch", "--quiet", "--no-tags", "--depth=1", source.URL, fetchRef); err != nil {
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
		if _, err := runGit(ctx, repoDir, "cat-file", "-e", commit+":"+name+"/recipe.yaml"); err != nil {
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

func packageGitRecipe(ctx context.Context, repoDir, commit, name, regDir string) (path, digest string, err error) {
	tmp, err := os.CreateTemp(regDir, ".git-artifact-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	path = tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(path)
		}
	}()

	h := sha256.New()
	limited := &maxWriter{w: io.MultiWriter(tmp, h), remaining: maxTarballBytes}
	gz := gzip.NewWriter(limited)
	tw := tar.NewWriter(gz)
	cmd := gitCommand(ctx, repoDir, "archive", "--format=tar", commit, "--", name)
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		tmp.Close()
		return "", "", err
	}
	if err := cmd.Start(); err != nil {
		tmp.Close()
		return "", "", gitError(err, stderr.String())
	}
	archiveErr := rewriteGitArchive(tar.NewReader(stdout), tw)
	if archiveErr != nil {
		_ = stdout.Close()
		_ = cmd.Process.Kill()
	}
	runErr := cmd.Wait()
	closeTarErr := tw.Close()
	closeGzipErr := gz.Close()
	closeFileErr := tmp.Close()
	if limited.err != nil {
		return "", "", limited.err
	}
	if archiveErr != nil {
		return "", "", archiveErr
	}
	if runErr != nil {
		return "", "", gitError(runErr, stderr.String())
	}
	if closeTarErr != nil {
		return "", "", closeTarErr
	}
	if closeGzipErr != nil {
		return "", "", closeGzipErr
	}
	if closeFileErr != nil {
		return "", "", closeFileErr
	}
	digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	ok = true
	return path, digest, nil
}

// rewriteGitArchive removes Git's commit-ID PAX header and normalizes metadata
// that is irrelevant to recipe behavior. Therefore an unrelated commit does
// not change an existing recipe's digest, while content, executable bits, and
// symlink targets remain digest-bound.
func rewriteGitArchive(tr *tar.Reader, tw *tar.Writer) error {
	entries := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Git archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		entries++
		if entries > maxGitRecipeEntries {
			return fmt.Errorf("Git recipe contains more than %d archive entries", maxGitRecipeEntries)
		}

		mode := int64(0o644)
		switch hdr.Typeflag {
		case tar.TypeDir:
			mode = 0o755
		case tar.TypeReg:
			if hdr.Size > maxGitRecipeFileBytes {
				return fmt.Errorf("Git recipe file %q exceeds the %d byte limit", hdr.Name, maxGitRecipeFileBytes)
			}
			total += hdr.Size
			if total > maxGitRecipeBytes {
				return fmt.Errorf("Git recipe exceeds the %d byte unpacked limit", maxGitRecipeBytes)
			}
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
		case tar.TypeSymlink:
			mode = 0o777
		default:
			return fmt.Errorf("Git recipe entry %q has unsupported archive type %d", hdr.Name, hdr.Typeflag)
		}

		normalized := &tar.Header{
			Name:       hdr.Name,
			Linkname:   hdr.Linkname,
			Size:       hdr.Size,
			Mode:       mode,
			Typeflag:   hdr.Typeflag,
			ModTime:    time.Unix(0, 0).UTC(),
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Format:     tar.FormatPAX,
		}
		if err := tw.WriteHeader(normalized); err != nil {
			return fmt.Errorf("write normalized recipe archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				return fmt.Errorf("copy Git recipe file %q: %w", hdr.Name, err)
			}
		}
	}
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

func installGitArtifact(regDir, tmpPath, digest string) (string, error) {
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if len(hexDigest) != 64 {
		os.Remove(tmpPath)
		return "", fmt.Errorf("invalid artifact digest %q", digest)
	}
	dir := filepath.Join(regDir, "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	final := filepath.Join(dir, hexDigest+".tar.gz")
	if got, err := fileDigest(final); err == nil && got == digest {
		os.Remove(tmpPath)
		return final, nil
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, final); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	return final, nil
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

func pruneGitArtifacts(regDir string, keep map[string]bool) {
	dir := filepath.Join(regDir, "artifacts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasSuffix(name, ".tar.gz") &&
			!keep[strings.TrimSuffix(name, ".tar.gz")] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
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
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
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

type maxWriter struct {
	w         io.Writer
	remaining int64
	err       error
}

func (w *maxWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.err = fmt.Errorf("generated artifact exceeds the %d byte limit", maxTarballBytes)
		return 0, w.err
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	if err != nil {
		w.err = err
	}
	return n, err
}
