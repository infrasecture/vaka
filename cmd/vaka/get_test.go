// cmd/vaka/get_test.go
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vaka.dev/vaka/pkg/registry"
)

const fixtureCompose = `services:
  app:
    image: alpine:3.20
`

const fixturePolicy = `apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`

// fixtureRegistry builds a complete file:// registry (tarball + index) and
// points the command seams at it. Returns the recipe tarball's digest.
func fixtureRegistry(t *testing.T, policyBlock string) string {
	t.Helper()
	dir := t.TempDir()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"demo/compose.yaml": fixtureCompose,
		"demo/vaka.yaml":    fixturePolicy,
		"demo/README.md":    "# demo\n",
	}
	if err := tw.WriteHeader(&tar.Header{Name: "demo/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(dir, "demo-1.0.0.tar.gz")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	digest := "sha256:" + hex.EncodeToString(sum[:])

	index := fmt.Sprintf(`apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
recipes:
  demo:
  - version: 1.0.0
    description: offline fixture recipe
    tags: [test]
    digest: %s
    urls: [file://%s]
    env:
    - name: VAKA_TEST_REQUIRED_ENV
      required: true
      description: exists only to test the unset-env hint
%s`, digest, tarPath, policyBlock)
	indexPath := filepath.Join(dir, "index.yaml")
	if err := os.WriteFile(indexPath, []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}

	oldCfg, oldClient := loadRegistriesConfig, newRegistryClient
	cacheDir := t.TempDir()
	loadRegistriesConfig = func() (*registry.Config, error) {
		return &registry.Config{
			APIVersion: registry.APIVersion,
			Kind:       "RegistriesConfig",
			Registries: []registry.Registry{{Name: "testreg", URL: "file://" + indexPath}},
		}, nil
	}
	newRegistryClient = func(maxAge time.Duration) *registry.Client {
		return &registry.Client{CacheDir: cacheDir, MaxIndexAge: maxAge}
	}
	t.Cleanup(func() {
		loadRegistriesConfig, newRegistryClient = oldCfg, oldClient
	})
	return digest
}

func matchingPolicyBlock() string {
	return `    policy:
      defaultActions: {app: reject}
      riskFlags: []
`
}

// runRecipeCmd executes the root command and captures stdout/stderr.
func runRecipeCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml", Rest: args})
	var out, errOut bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

func TestGetInstallUpdateRejectConvergeCycle(t *testing.T) {
	digest := fixtureRegistry(t, matchingPolicyBlock())
	target := filepath.Join(t.TempDir(), "demo")

	// Install.
	stdout, stderr, err := runRecipeCmd(t, "get", "demo", target)
	if err != nil {
		t.Fatalf("get: %v (stderr: %s)", err, stderr)
	}
	for _, want := range []string{
		"demo@1.0.0 installed into " + target,
		digest,
		"Egress policy: app: reject",
		"Required env not set: VAKA_TEST_REQUIRED_ENV",
		"Next: cd " + target + " && vaka up",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stderr, "stale") {
		t.Fatalf("unexpected staleness warning:\n%s", stderr)
	}

	// Idempotent re-get.
	stdout, _, err = runRecipeCmd(t, "get", "demo", target)
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if !strings.Contains(stdout, "already up to date") {
		t.Fatalf("stdout missing up-to-date notice:\n%s", stdout)
	}

	// Modified tracked file blocks.
	if err := os.WriteFile(filepath.Join(target, "compose.yaml"), []byte("hacked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = runRecipeCmd(t, "get", "demo", target)
	if err == nil || !strings.Contains(err.Error(), "does not merge") {
		t.Fatalf("err = %v, want merge refusal", err)
	}

	// Resolution converges: move the edit aside, re-get restores the file.
	if err := os.Rename(filepath.Join(target, "compose.yaml"), filepath.Join(target, "my-compose.yaml")); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runRecipeCmd(t, "get", "demo", target)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if !strings.Contains(stderr, "restored from the recipe") {
		t.Fatalf("stderr missing restore warning:\n%s", stderr)
	}
	data, err := os.ReadFile(filepath.Join(target, "compose.yaml"))
	if err != nil || string(data) != fixtureCompose {
		t.Fatalf("compose.yaml not restored: %q %v", data, err)
	}
}

func TestGetRefusesForeignDirectory(t *testing.T) {
	fixtureRegistry(t, matchingPolicyBlock())
	target := t.TempDir() // exists, no lock

	_, _, err := runRecipeCmd(t, "get", "demo", target)
	if err == nil || !strings.Contains(err.Error(), "never adopts") {
		t.Fatalf("err = %v, want adoption refusal", err)
	}
}

func TestGetWarnsOnStaleIndexPolicy(t *testing.T) {
	fixtureRegistry(t, `    policy:
      defaultActions: {app: accept}
      riskFlags: [app:privileged]
`)
	target := filepath.Join(t.TempDir(), "demo")
	stdout, stderr, err := runRecipeCmd(t, "get", "demo", target)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(stderr, "stale/inaccurate") {
		t.Fatalf("stderr missing staleness warning:\n%s", stderr)
	}
	// The local, correct summary is still what gets printed.
	if !strings.Contains(stdout, "Egress policy: app: reject") {
		t.Fatalf("stdout missing local policy:\n%s", stdout)
	}
}

func TestSearchListInfoAndRegistryList(t *testing.T) {
	fixtureRegistry(t, matchingPolicyBlock())

	stdout, _, err := runRecipeCmd(t, "search", "fixture")
	if err != nil || !strings.Contains(stdout, "demo") || !strings.Contains(stdout, "1.0.0") {
		t.Fatalf("search: %v\n%s", err, stdout)
	}
	if _, _, err := runRecipeCmd(t, "search", "nonexistent-term"); err == nil {
		t.Fatal("search with no matches must fail")
	}

	stdout, _, err = runRecipeCmd(t, "recipes", "list")
	if err != nil || !strings.Contains(stdout, "demo") {
		t.Fatalf("recipes list: %v\n%s", err, stdout)
	}

	stdout, _, err = runRecipeCmd(t, "recipes", "info", "demo")
	if err != nil {
		t.Fatalf("recipes info: %v", err)
	}
	for _, want := range []string{"testreg/demo", "offline fixture recipe", "VAKA_TEST_REQUIRED_ENV", "advisory"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("info missing %q:\n%s", want, stdout)
		}
	}

	stdout, _, err = runRecipeCmd(t, "registry", "list")
	if err != nil || !strings.Contains(stdout, "testreg") || !strings.Contains(stdout, "file://") {
		t.Fatalf("registry list: %v\n%s", err, stdout)
	}
}

func TestDeviationNoticeOnRenderVerbs(t *testing.T) {
	dir := t.TempDir()
	lock := `apiVersion: recipes.vaka/v1alpha1
kind: RecipeLock
registry: official
name: demo
version: 1.0.0
digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
fetched: "2026-07-11T00:00:00Z"
files:
  compose.yaml: sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
deviations:
  - {path: extra.yaml, kind: skipped-collision}
`
	if err := os.WriteFile(filepath.Join(dir, ".vaka-recipe.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	printDeviationNotice(&buf, dir)
	got := buf.String()
	if !strings.Contains(got, "deviates from published demo@1.0.0") || !strings.Contains(got, "1 file(s)") {
		t.Fatalf("notice = %q", got)
	}

	// No lock, or no deviations → silence.
	buf.Reset()
	printDeviationNotice(&buf, t.TempDir())
	if buf.Len() != 0 {
		t.Fatalf("notice for lock-less dir: %q", buf.String())
	}
}
