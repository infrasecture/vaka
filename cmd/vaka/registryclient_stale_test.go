// cmd/vaka/registryclient_stale_test.go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/registry"
)

// primeStaleCache writes a cache envelope for a registry whose URL points at a
// closed port, so a MaxIndexAge=0 fetch reads the cache, fails to revalidate,
// and returns Stale=true — without needing the registry package's transport
// seam.
func primeStaleCache(t *testing.T) (cacheDir, deadURL string) {
	t.Helper()
	cacheDir = t.TempDir()
	deadURL = "https://127.0.0.1:1/index.yaml" // port 1: connection refused
	index := "apiVersion: recipes.vaka/v1alpha1\n" +
		"kind: RegistryIndex\n" +
		"recipes:\n  demo:\n  - version: 1.0.0\n" +
		"    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000\n" +
		"    urls: [https://example.com/demo-1.0.0.tar.gz]\n"
	// Mirror the registry package's envelope encoding (url/etag/index, with the
	// index bytes as yaml !!binary) so readIndexCache decodes it.
	envelope, err := yaml.Marshal(map[string]any{
		"url": deadURL, "etag": "", "index": []byte(index),
	})
	if err != nil {
		t.Fatal(err)
	}
	regDir := filepath.Join(cacheDir, "testreg")
	if err := os.MkdirAll(regDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regDir, "cache.yaml"), envelope, 0o644); err != nil {
		t.Fatal(err)
	}
	return cacheDir, deadURL
}

func TestUnqualifiedResolutionRejectsStaleIndex(t *testing.T) {
	cacheDir, deadURL := primeStaleCache(t)

	oldCfg, oldClient := loadRegistriesConfig, newRegistryClient
	loadRegistriesConfig = func() (*registry.Config, error) {
		return &registry.Config{
			APIVersion: registry.APIVersion, Kind: "RegistriesConfig",
			Registries: []registry.Registry{{Name: "testreg", URL: deadURL}},
		}, nil
	}
	newRegistryClient = func(maxAge time.Duration) *registry.Client {
		return &registry.Client{CacheDir: cacheDir, MaxIndexAge: maxAge}
	}
	t.Cleanup(func() { loadRegistriesConfig, newRegistryClient = oldCfg, oldClient })

	var warn bytes.Buffer

	// Unqualified (only == "") + strict: a stale index cannot prove uniqueness,
	// so it must be fatal.
	if _, err := loadRegistryWorld(0, "", true, &warn); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("unqualified strict err = %v, want stale refusal", err)
	}

	// Qualified (only == "testreg") + strict: the name is disambiguated, so a
	// stale index is tolerated with a warning.
	warn.Reset()
	w, err := loadRegistryWorld(0, "testreg", true, &warn)
	if err != nil {
		t.Fatalf("qualified strict got fatal error on stale index: %v", err)
	}
	if _, ok := w.indexes["testreg"]; !ok {
		t.Fatal("qualified stale index was not loaded from cache")
	}
	if !strings.Contains(warn.String(), "cached index") {
		t.Fatalf("qualified stale index did not warn: %q", warn.String())
	}
}

func TestMissingGitPreviewCacheDoesNotBlockUnqualifiedReleaseResolution(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.yaml")
	if err := os.WriteFile(indexPath, []byte(`apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
recipes:
  stable:
  - version: 1.0.0
    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
    urls: [https://example.com/stable.tar.gz]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	oldCfg, oldClient := loadRegistriesConfig, newRegistryClient
	loadRegistriesConfig = func() (*registry.Config, error) {
		return &registry.Config{
			APIVersion: registry.APIVersion, Kind: "RegistriesConfig",
			Registries: []registry.Registry{
				{Name: "release", URL: "file://" + indexPath},
				{Name: "preview", Git: &registry.GitSource{URL: "file:///not-used.git", Ref: "candidate"}},
			},
		}, nil
	}
	newRegistryClient = func(maxAge time.Duration) *registry.Client {
		return &registry.Client{CacheDir: filepath.Join(dir, "cache"), MaxIndexAge: maxAge}
	}
	t.Cleanup(func() { loadRegistriesConfig, newRegistryClient = oldCfg, oldClient })

	var warn bytes.Buffer
	w, err := loadRegistryWorld(0, "", true, &warn)
	if err != nil {
		t.Fatalf("unqualified strict load: %v", err)
	}
	if _, ok := w.indexes["release"]; !ok {
		t.Fatal("published index was not loaded")
	}
	if !strings.Contains(warn.String(), `registry "preview" unavailable`) {
		t.Fatalf("missing preview did not warn: %q", warn.String())
	}
	if _, err := loadRegistryWorld(0, "preview", true, &warn); err == nil {
		t.Fatal("qualified preview unexpectedly tolerated a missing cache")
	}
}
