package registry

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vaka.dev/vaka/pkg/recipe"
)

const gitFixtureCompose = `services:
  app:
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
`

const gitFixturePolicy = `apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`

type gitRegistryFixture struct {
	t   *testing.T
	dir string
	reg Registry
	ref string
}

func newGitRegistryFixture(t *testing.T) *gitRegistryFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	runFixtureGit(t, dir, "init", "--quiet", "--initial-branch=preview")
	runFixtureGit(t, dir, "config", "uploadpack.allowFilter", "true")
	f := &gitRegistryFixture{t: t, dir: dir, ref: "preview"}
	f.reg = Registry{
		Name: "preview",
		Git: &GitSource{
			URL: (&url.URL{Scheme: "file", Path: filepath.ToSlash(dir)}).String(),
			Ref: f.ref,
		},
	}
	f.write("demo/recipe.yaml", `apiVersion: recipes.vaka/v1alpha1
kind: Recipe
name: demo
version: 1.0.0
description: Git preview fixture
tags: [test, preview]
minVakaVersion: 0.0.1
env:
  - name: DEMO_TOKEN
    required: true
    description: fixture input
`)
	f.write("demo/compose.yaml", gitFixtureCompose)
	f.write("demo/vaka.yaml", gitFixturePolicy)
	f.write("demo/README.md", "# demo\n")
	f.write("demo/run.sh", "#!/bin/sh\nexec true\n")
	f.write("demo/literal.txt", "$Format:%H$\n")
	if err := os.Chmod(filepath.Join(dir, "demo", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("compose.yaml", filepath.Join(dir, "demo", "docker-compose.yaml")); err != nil {
		t.Fatal(err)
	}
	f.write(".gitignore", "/demo/.secrets/\n")
	// Git archive attributes must not affect direct tree/blob packaging.
	f.write(".gitattributes", "demo/run.sh export-ignore\ndemo/literal.txt export-subst\n")
	// A nested manifest is not a top-level recipe and must never be discovered.
	f.write("nested/inner/recipe.yaml", "not a top-level recipe\n")
	f.write("docs/note.txt", "registry documentation\n")
	f.commit("initial recipe")
	f.write("demo/.secrets/token", "must never enter the preview artifact\n")
	return f
}

func (f *gitRegistryFixture) write(name, data string) {
	f.t.Helper()
	path := filepath.Join(f.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gitRegistryFixture) commit(message string) string {
	f.t.Helper()
	runFixtureGit(f.t, f.dir, "add", "-A")
	runFixtureGit(f.t, f.dir, "-c", "user.name=Vaka Test", "-c", "user.email=vaka@example.invalid", "commit", "--quiet", "-m", message)
	return strings.TrimSpace(runFixtureGit(f.t, f.dir, "rev-parse", "HEAD"))
}

func runFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func gitObjectMissing(t *testing.T, repoDir, oid string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repoDir, "cat-file", "-e", oid)
	cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	err := cmd.Run()
	if err == nil {
		return false
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("inspect Git object %s: %v", oid, err)
	}
	return true
}

func TestGitPreviewRefreshUsesOnlyTrackedTopLevelRecipeContent(t *testing.T) {
	f := newGitRegistryFixture(t)
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[tar]\n\tumask = 0777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	client := &Client{CacheDir: t.TempDir()}

	if _, err := client.FetchIndex(f.reg); err == nil || !strings.Contains(err.Error(), "registry refresh") {
		t.Fatalf("cold FetchIndex err = %v, want explicit-refresh guidance", err)
	}
	first, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if first.Stale || first.Revision == "" {
		t.Fatalf("first refresh = %+v", first)
	}
	if len(first.Index.Recipes) != 1 || len(first.Index.Recipes["demo"]) != 1 {
		t.Fatalf("generated recipes = %+v, want only demo", first.Index.Recipes)
	}
	entry := first.Index.Recipes["demo"][0]
	if entry.SourceRevision != first.Revision || entry.Version != "1.0.0" ||
		entry.Policy == nil || entry.Policy.DefaultActions["app"] != "reject" {
		t.Fatalf("generated entry = %+v", entry)
	}
	if got, ok := client.CacheRevision(f.reg); !ok || got != first.Revision {
		t.Fatalf("CacheRevision = %q, %v; want %q", got, ok, first.Revision)
	}
	artifactURL, err := url.Parse(entry.URLs[0])
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(artifactURL.Path); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("cached artifact permissions = %v, %v; want user-only", info, err)
	}

	artifact, err := client.FetchTarball(f.reg, "demo", entry)
	if err != nil {
		t.Fatalf("FetchTarball: %v", err)
	}
	defer os.Remove(artifact)
	extracted := t.TempDir()
	root, err := recipe.OpenSafeRoot(extracted)
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.Open(artifact)
	if err != nil {
		t.Fatal(err)
	}
	err = recipe.ExtractRecipe(a, "demo", root)
	a.Close()
	root.Close()
	if err != nil {
		t.Fatalf("ExtractRecipe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extracted, ".secrets", "token")); !os.IsNotExist(err) {
		t.Fatalf("ignored/untracked secret entered artifact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extracted, "nested")); !os.IsNotExist(err) {
		t.Fatalf("nested registry tree entered recipe artifact: %v", err)
	}
	if info, err := os.Stat(filepath.Join(extracted, "run.sh")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable mode was not preserved: info=%v err=%v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(extracted, "literal.txt")); err != nil || string(got) != "$Format:%H$\n" {
		t.Fatalf("Git archive attributes changed tracked content: %q, %v", got, err)
	}
	if target, err := os.Readlink(filepath.Join(extracted, "docker-compose.yaml")); err != nil || target != "compose.yaml" {
		t.Fatalf("symlink = %q, %v", target, err)
	}

	// Neither an uncommitted edit nor an unrelated committed change alters the
	// content-derived recipe digest. The latter advances source provenance only.
	f.write("demo/README.md", "uncommitted edit\n")
	again, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatalf("same-commit refresh: %v", err)
	}
	if got := again.Index.Recipes["demo"][0].Digest; got != entry.Digest {
		t.Fatalf("uncommitted content changed digest: got %s want %s", got, entry.Digest)
	}
	runFixtureGit(t, f.dir, "restore", "demo/README.md")
	f.write("docs/other.txt", "unrelated committed content\n")
	unrelatedCommit := f.commit("unrelated change")
	unrelated, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatalf("unrelated refresh: %v", err)
	}
	if unrelated.Revision != unrelatedCommit || unrelated.Index.Recipes["demo"][0].Digest != entry.Digest {
		t.Fatalf("unrelated refresh revision/digest = %s/%s, want %s/%s",
			unrelated.Revision, unrelated.Index.Recipes["demo"][0].Digest, unrelatedCommit, entry.Digest)
	}

	// A committed recipe edit remains invisible until explicit refresh, then
	// produces a new digest even when the candidate SemVer is intentionally unchanged.
	f.write("demo/README.md", "# changed candidate\n")
	changedCommit := f.commit("change recipe candidate")
	cached, err := client.FetchIndex(f.reg)
	if err != nil {
		t.Fatalf("cached FetchIndex: %v", err)
	}
	if cached.Revision != unrelatedCommit || cached.Index.Recipes["demo"][0].Digest != entry.Digest {
		t.Fatalf("ordinary fetch advanced mutable ref: %+v", cached)
	}
	changed, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatalf("changed refresh: %v", err)
	}
	if changed.Revision != changedCommit || changed.Index.Recipes["demo"][0].Digest == entry.Digest {
		t.Fatalf("changed refresh revision/digest = %s/%s", changed.Revision, changed.Index.Recipes["demo"][0].Digest)
	}
}

func TestGitPreviewBloblessFetchDoesNotMaterializeUnrelatedBlob(t *testing.T) {
	f := newGitRegistryFixture(t)
	unrelatedOID := f.git("rev-parse", "HEAD:docs/note.txt")
	repoDir, commit, _, err := fetchGitCommit(context.Background(), f.reg.Git)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(repoDir)
	if !gitObjectMissing(t, repoDir, unrelatedOID) {
		t.Fatal("blobless fetch materialized an unrelated repository blob")
	}

	artifact, _, err := packageGitRecipe(context.Background(), repoDir, commit, "demo", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(artifact)
	if !gitObjectMissing(t, repoDir, unrelatedOID) {
		t.Fatal("packaging one recipe materialized an unrelated repository blob")
	}
}

func TestGitPreviewObjectStoreLimitFailsClosed(t *testing.T) {
	f := newGitRegistryFixture(t)
	old := maxGitObjectStoreBytes
	maxGitObjectStoreBytes = 1
	t.Cleanup(func() { maxGitObjectStoreBytes = old })

	_, err := (&Client{CacheDir: t.TempDir()}).RefreshIndex(context.Background(), f.reg)
	if err == nil || !strings.Contains(err.Error(), "aggregate limit") {
		t.Fatalf("object-store limit err = %v", err)
	}
}

func TestGitPreviewRetainsArtifactsAcrossRapidRefreshes(t *testing.T) {
	f := newGitRegistryFixture(t)
	client := &Client{CacheDir: t.TempDir()}
	first, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatal(err)
	}
	firstEntry := first.Index.Recipes["demo"][0]
	firstURL, err := url.Parse(firstEntry.URLs[0])
	if err != nil {
		t.Fatal(err)
	}

	f.write("demo/README.md", "second generation\n")
	f.commit("second generation")
	if _, err := client.RefreshIndex(context.Background(), f.reg); err != nil {
		t.Fatal(err)
	}
	f.write("demo/README.md", "third generation\n")
	f.commit("third generation")
	if _, err := client.RefreshIndex(context.Background(), f.reg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstURL.Path); err != nil {
		t.Fatalf("rapid refresh removed an artifact still covered by the reader grace period: %v", err)
	}
	copyPath, err := client.FetchTarball(f.reg, "demo", firstEntry)
	if err != nil {
		t.Fatalf("fetch artifact from older atomic index: %v", err)
	}
	os.Remove(copyPath)

	old := time.Now().Add(-gitArtifactGracePeriod - time.Hour)
	if err := os.Chtimes(firstURL.Path, old, old); err != nil {
		t.Fatal(err)
	}
	f.write("demo/README.md", "fourth generation\n")
	f.commit("fourth generation")
	if _, err := client.RefreshIndex(context.Background(), f.reg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstURL.Path); !os.IsNotExist(err) {
		t.Fatalf("expired unreferenced artifact still exists: %v", err)
	}
}

func TestGitPreviewArtifactCacheLimitPreservesLastSnapshot(t *testing.T) {
	f := newGitRegistryFixture(t)
	client := &Client{CacheDir: t.TempDir()}
	good, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(client.CacheDir, f.reg.Name, "artifacts")
	size, err := directorySizeAtMost(artifactDir, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	oldLimit := maxGitArtifactCacheBytes
	maxGitArtifactCacheBytes = size
	t.Cleanup(func() { maxGitArtifactCacheBytes = oldLimit })

	f.write("demo/README.md", strings.Repeat("changed candidate ", 1000))
	f.commit("candidate exceeds aggregate cache budget")
	stale, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Stale || stale.Revision != good.Revision || !strings.Contains(stale.FallbackReason, "aggregate limit") {
		t.Fatalf("limited refresh = %+v, want retained snapshot %s", stale, good.Revision)
	}
	entries, err := os.ReadDir(artifactDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed limited refresh left artifacts: %v, %v", entries, err)
	}
}

func TestGitPreviewMigratesExistingCachePermissions(t *testing.T) {
	f := newGitRegistryFixture(t)
	client := &Client{CacheDir: t.TempDir()}
	res, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatal(err)
	}
	artifactURL, err := url.Parse(res.Index.Recipes["demo"][0].URLs[0])
	if err != nil {
		t.Fatal(err)
	}
	regDir := filepath.Join(client.CacheDir, f.reg.Name)
	artifactDir := filepath.Join(regDir, "artifacts")
	paths := []string{filepath.Join(regDir, "cache.yaml"), filepath.Join(regDir, "git-refresh.lock"), artifactURL.Path}
	for _, dir := range []string{regDir, artifactDir} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := client.FetchIndex(f.reg); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{regDir, artifactDir} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("cache directory %s mode = %v, %v; want 0700", dir, info, err)
		}
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("cache file %s mode = %v, %v; want 0600", path, info, err)
		}
	}
}

func TestGitPreviewFailedRefreshPreservesLastSnapshot(t *testing.T) {
	f := newGitRegistryFixture(t)
	client := &Client{CacheDir: t.TempDir()}
	good, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatalf("good refresh: %v", err)
	}
	goodDigest := good.Index.Recipes["demo"][0].Digest
	artifactDir := filepath.Join(client.CacheDir, f.reg.Name, "artifacts")
	beforeArtifacts, err := os.ReadDir(artifactDir)
	if err != nil {
		t.Fatal(err)
	}

	// demo validates and publishes a new temporary artifact first; zzz then
	// fails validation. The failed transaction must remove demo's unused new
	// artifact and retain only the prior index's artifact set.
	f.write("demo/README.md", "changed before later recipe fails\n")
	f.write("zzz/recipe.yaml", "apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: zzz\nversion: invalid\ndescription: bad\n")
	f.write("zzz/compose.yaml", gitFixtureCompose)
	f.write("zzz/vaka.yaml", gitFixturePolicy)
	f.commit("invalid candidate")
	stale, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatalf("failed refresh with cache: %v", err)
	}
	if !stale.Stale || stale.Revision != good.Revision || stale.Index.Recipes["demo"][0].Digest != goodDigest {
		t.Fatalf("stale result = %+v, want retained revision %s", stale, good.Revision)
	}
	if !strings.Contains(stale.FallbackReason, "strict SemVer") {
		t.Fatalf("stale fallback reason = %q", stale.FallbackReason)
	}
	afterArtifacts, err := os.ReadDir(artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterArtifacts) != len(beforeArtifacts) {
		t.Fatalf("failed refresh left artifacts: before=%v after=%v", beforeArtifacts, afterArtifacts)
	}
	cached, err := client.FetchIndex(f.reg)
	if err != nil || cached.Revision != good.Revision {
		t.Fatalf("cache after failed refresh = %+v, %v", cached, err)
	}
}

func TestGitPreviewCacheIsBoundToURLAndRef(t *testing.T) {
	f := newGitRegistryFixture(t)
	client := &Client{CacheDir: t.TempDir()}
	if _, err := client.RefreshIndex(context.Background(), f.reg); err != nil {
		t.Fatal(err)
	}
	rebound := f.reg
	rebound.Git = &GitSource{URL: f.reg.Git.URL, Ref: "other-branch"}
	if _, err := client.FetchIndex(rebound); err == nil || !strings.Contains(err.Error(), "no resolved Git preview cache") {
		t.Fatalf("rebound FetchIndex err = %v", err)
	}
	if _, ok := client.CachedIndex(rebound); ok {
		t.Fatal("cache was reused after changing the Git ref")
	}
}

func TestGitPreviewIgnoresAmbientRepositorySelection(t *testing.T) {
	f := newGitRegistryFixture(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "wrong-object-store"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(t.TempDir(), "wrong-worktree"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(t.TempDir(), "hooks"))

	client := &Client{CacheDir: t.TempDir()}
	res, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil || res.Stale || len(res.Index.Recipes["demo"]) != 1 {
		t.Fatalf("refresh under ambient Git variables = %+v, %v", res, err)
	}
}

func TestGitPreviewAcceptsFullTagRef(t *testing.T) {
	f := newGitRegistryFixture(t)
	f.git("tag", "candidate-v1")
	reg := f.reg
	reg.Git = &GitSource{URL: f.reg.Git.URL, Ref: "refs/tags/candidate-v1"}

	res, err := (&Client{CacheDir: t.TempDir()}).RefreshIndex(context.Background(), reg)
	if err != nil || res.Stale || res.Revision != f.git("rev-parse", "HEAD") {
		t.Fatalf("full tag refresh = %+v, %v", res, err)
	}
}

func TestGitRefreshLockExcludesConcurrentRefresh(t *testing.T) {
	dir := t.TempDir()
	unlock, err := acquireGitRefreshLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireGitRefreshLock(dir); err == nil || !strings.Contains(err.Error(), "already running") {
		unlock()
		t.Fatalf("second lock err = %v", err)
	}
	unlock()
	unlockAgain, err := acquireGitRefreshLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlockAgain()
}

func TestGitPreviewRejectsInconsistentCachedProvenance(t *testing.T) {
	f := newGitRegistryFixture(t)
	client := &Client{CacheDir: t.TempDir()}
	res, err := client.RefreshIndex(context.Background(), f.reg)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := indexCachePath(client.CacheDir, f.reg.Name)
	env, _, ok := readIndexCache(cachePath, f.reg.sourceIdentity())
	if !ok {
		t.Fatal("generated cache missing")
	}
	env.Index = []byte(strings.Replace(string(env.Index), res.Revision, strings.Repeat("c", 40), 1))
	if err := writeIndexCache(cachePath, env); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchIndex(f.reg); err == nil || !strings.Contains(err.Error(), "provenance is inconsistent") {
		t.Fatalf("inconsistent cache err = %v", err)
	}
}

func (f *gitRegistryFixture) git(args ...string) string {
	f.t.Helper()
	return strings.TrimSpace(runFixtureGit(f.t, f.dir, args...))
}
