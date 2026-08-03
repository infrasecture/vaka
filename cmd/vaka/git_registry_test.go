package main

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vaka.dev/vaka/pkg/recipe"
	"vaka.dev/vaka/pkg/registry"
)

type commandGitFixture struct {
	t   *testing.T
	dir string
	url string
}

func newCommandGitFixture(t *testing.T) *commandGitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	f := &commandGitFixture{
		t:   t,
		dir: dir,
		url: (&url.URL{Scheme: "file", Path: filepath.ToSlash(dir)}).String(),
	}
	f.git("init", "--quiet", "--initial-branch=preview")
	f.write("demo/recipe.yaml", `apiVersion: recipes.vaka/v1alpha1
kind: Recipe
name: demo
version: 1.0.0
description: command Git preview fixture
tags: [test]
`)
	f.write("demo/compose.yaml", fixtureCompose)
	f.write("demo/vaka.yaml", fixturePolicy)
	f.write("demo/README.md", "# original\n")
	f.commit("initial recipe")
	return f
}

func (f *commandGitFixture) write(name, content string) {
	f.t.Helper()
	path := filepath.Join(f.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *commandGitFixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *commandGitFixture) commit(message string) string {
	f.t.Helper()
	f.git("add", "-A")
	f.git("-c", "user.name=Vaka Test", "-c", "user.email=vaka@example.invalid", "commit", "--quiet", "-m", message)
	return f.git("rev-parse", "HEAD")
}

func TestGitPreviewCommandLifecycle(t *testing.T) {
	fixture := newCommandGitFixture(t)
	publishedIndex := filepath.Join(t.TempDir(), "index.yaml")
	if err := os.WriteFile(publishedIndex, []byte(`apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
recipes:
  stable:
    - version: 1.0.0
      description: published fixture
      digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
      urls: [https://example.invalid/stable.tar.gz]
      sourceRevision: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`), 0o600); err != nil {
		t.Fatal(err)
	}
	initial := &registry.Config{
		APIVersion: registry.APIVersion,
		Kind:       "RegistriesConfig",
		Registries: []registry.Registry{{Name: "official", URL: "file://" + publishedIndex}},
	}
	saved := stubRegistryConfig(t, initial)
	cacheDir := t.TempDir()
	oldClient := newRegistryClient
	newRegistryClient = func(maxAge time.Duration) *registry.Client {
		return &registry.Client{CacheDir: cacheDir, MaxIndexAge: maxAge}
	}
	t.Cleanup(func() { newRegistryClient = oldClient })

	stdout, _, err := runRecipeCmd(t, "registry", "add-git", "preview", fixture.url, "--ref", "preview")
	if err != nil {
		t.Fatalf("add-git: %v", err)
	}
	if !strings.Contains(stdout, "Added Git preview registry") || *saved == nil {
		t.Fatalf("add-git stdout/config = %q / %+v", stdout, *saved)
	}
	reg, ok := (*saved).Lookup("preview")
	if !ok || reg.Git == nil || reg.Git.Ref != "preview" {
		t.Fatalf("saved Git registry = %+v, %v", reg, ok)
	}

	stdout, _, err = runRecipeCmd(t, "registry", "list")
	if err != nil || !strings.Contains(stdout, "git "+fixture.url+"#preview") || !strings.Contains(stdout, "REVISION") {
		t.Fatalf("registry list = %q, %v", stdout, err)
	}
	stdout, _, err = runRecipeCmd(t, "recipes", "list")
	if err != nil || !strings.Contains(stdout, "preview/demo") || !strings.Contains(stdout, "stable") || strings.Contains(stdout, "official/stable") {
		t.Fatalf("recipes list = %q, %v", stdout, err)
	}
	if got, _ := completeRecipeRefs(""); len(got) != 2 || got[0] != "preview/demo" || got[1] != "stable" {
		t.Fatalf("mixed published/preview completions = %v, want [preview/demo stable]", got)
	}
	stdout, _, err = runRecipeCmd(t, "recipes", "info", "stable")
	if err != nil || strings.Contains(stdout, "Git commit:") {
		t.Fatalf("published info accepted preview provenance = %q, %v", stdout, err)
	}

	target := filepath.Join(t.TempDir(), "demo")
	if _, _, err := runRecipeCmd(t, "get", "demo", target); err == nil ||
		!strings.Contains(err.Error(), "preview/demo") {
		t.Fatalf("unqualified preview get err = %v", err)
	}
	stdout, _, err = runRecipeCmd(t, "get", "preview/demo", target)
	if err != nil {
		t.Fatalf("qualified preview get: %v", err)
	}
	if !strings.Contains(stdout, "Source commit:") || readCommandFixtureFile(t, target, "README.md") != "# original\n" {
		t.Fatalf("initial get stdout/content = %q / %q", stdout, readCommandFixtureFile(t, target, "README.md"))
	}
	firstLock := commandFixtureLock(t, target)
	if firstLock.SourceRevision == "" {
		t.Fatal("installed lock has no Git source revision")
	}

	fixture.write("demo/README.md", "# changed without version bump\n")
	changedCommit := fixture.commit("change candidate")
	stdout, _, err = runRecipeCmd(t, "get", "preview/demo", target)
	if err != nil || !strings.Contains(stdout, "already up to date") {
		t.Fatalf("get before refresh = %q, %v", stdout, err)
	}
	if got := readCommandFixtureFile(t, target, "README.md"); got != "# original\n" {
		t.Fatalf("get advanced branch without refresh: %q", got)
	}

	stdout, _, err = runRecipeCmd(t, "registry", "refresh", "preview")
	if err != nil || !strings.Contains(stdout, shortCommit(changedCommit)) {
		t.Fatalf("refresh = %q, %v", stdout, err)
	}
	stdout, _, err = runRecipeCmd(t, "get", "preview/demo", target)
	if err != nil || !strings.Contains(stdout, "updated in") {
		t.Fatalf("get after refresh = %q, %v", stdout, err)
	}
	if got := readCommandFixtureFile(t, target, "README.md"); got != "# changed without version bump\n" {
		t.Fatalf("updated README = %q", got)
	}
	updatedLock := commandFixtureLock(t, target)
	if updatedLock.Version != firstLock.Version || updatedLock.Digest == firstLock.Digest ||
		updatedLock.SourceRevision != changedCommit {
		t.Fatalf("updated lock = %+v; first = %+v", updatedLock, firstLock)
	}

	fixture.write("demo/recipe.yaml", "apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: demo\nversion: invalid\ndescription: broken candidate\n")
	fixture.commit("break candidate")
	stdout, _, err = runRecipeCmd(t, "registry", "refresh", "preview")
	if err == nil || !strings.Contains(stdout, "refresh failed (retained cache") ||
		!strings.Contains(stdout, "strict SemVer") {
		t.Fatalf("invalid refresh = %q, %v", stdout, err)
	}
	stdout, _, err = runRecipeCmd(t, "get", "preview/demo", target)
	if err != nil || !strings.Contains(stdout, "already up to date") {
		t.Fatalf("get after failed refresh = %q, %v", stdout, err)
	}
	if got := commandFixtureLock(t, target).SourceRevision; got != changedCommit {
		t.Fatalf("failed refresh changed installed provenance to %s, want %s", got, changedCommit)
	}
}

func TestRegistryAddGitRequiresRef(t *testing.T) {
	stubRegistryConfig(t, &registry.Config{APIVersion: registry.APIVersion, Kind: "RegistriesConfig"})
	_, _, err := runRecipeCmd(t, "registry", "add-git", "preview", "https://github.com/example/recipes.git")
	if err == nil || !strings.Contains(err.Error(), "required flag") {
		t.Fatalf("missing --ref err = %v", err)
	}
}

func readCommandFixtureFile(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func commandFixtureLock(t *testing.T, dir string) *recipe.Lock {
	t.Helper()
	lock, exists, err := recipe.LockForDir(dir)
	if err != nil || !exists {
		t.Fatalf("LockForDir: exists=%v err=%v", exists, err)
	}
	return lock
}
