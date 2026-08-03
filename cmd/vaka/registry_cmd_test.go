// cmd/vaka/registry_cmd_test.go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vaka.dev/vaka/pkg/registry"
)

// stubRegistryConfig replaces the load/save seams with an in-memory config
// and returns a pointer to the last saved config (nil until a save happens).
func stubRegistryConfig(t *testing.T, initial *registry.Config) **registry.Config {
	t.Helper()
	oldLoad, oldSave := loadRegistriesConfig, saveRegistriesConfig
	cur := initial
	var saved *registry.Config
	loadRegistriesConfig = func() (*registry.Config, error) {
		// Return a copy so command mutation doesn't alias the fixture.
		cp := *cur
		cp.Registries = append([]registry.Registry{}, cur.Registries...)
		return &cp, nil
	}
	saveRegistriesConfig = func(c *registry.Config) error {
		saved = c
		cur = c
		return nil
	}
	t.Cleanup(func() { loadRegistriesConfig, saveRegistriesConfig = oldLoad, oldSave })
	return &saved
}

func TestRegistryAdd(t *testing.T) {
	saved := stubRegistryConfig(t, registry.DefaultConfig())

	stdout, _, err := runRecipeCmd(t, "registry", "add", "acme", "https://recipes.acme.example/index.yaml")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(stdout, "Added registry \"acme\"") {
		t.Fatalf("stdout: %s", stdout)
	}
	if *saved == nil {
		t.Fatal("config was not saved")
	}
	if _, ok := (*saved).Lookup("acme"); !ok {
		t.Fatalf("acme not in saved config: %+v", (*saved).Registries)
	}

	// Duplicate and invalid inputs are rejected and not saved.
	if _, _, err := runRecipeCmd(t, "registry", "add", "official", "https://x.example/index.yaml"); err == nil {
		t.Fatal("duplicate add should fail")
	}
	if _, _, err := runRecipeCmd(t, "registry", "add", "acme", "http://insecure.example/index.yaml"); err == nil {
		t.Fatal("http URL should fail")
	}
}

func TestRegistryRemove(t *testing.T) {
	cfg := registry.DefaultConfig()
	_ = cfg.Add(registry.Registry{Name: "acme", URL: "https://recipes.acme.example/index.yaml"})
	saved := stubRegistryConfig(t, cfg)
	cacheDir := t.TempDir()
	cachePath := filepath.Join(cacheDir, "acme", "cache.yaml")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldClient := newRegistryClient
	newRegistryClient = func(maxAge time.Duration) *registry.Client {
		return &registry.Client{CacheDir: cacheDir, MaxIndexAge: maxAge}
	}
	t.Cleanup(func() { newRegistryClient = oldClient })

	stdout, _, err := runRecipeCmd(t, "registry", "remove", "acme")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(stdout, "Removed registry \"acme\"") {
		t.Fatalf("stdout: %s", stdout)
	}
	if _, ok := (*saved).Lookup("acme"); ok {
		t.Fatal("acme still in saved config")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "acme")); !os.IsNotExist(err) {
		t.Fatalf("removed registry cache still exists: %v", err)
	}

	// The rm alias works; removing a nonexistent registry fails.
	if _, _, err := runRecipeCmd(t, "registry", "rm", "nope"); err == nil {
		t.Fatal("removing a nonexistent registry should fail")
	}
}

func TestRegistryRefresh(t *testing.T) {
	fixtureRegistry(t, matchingPolicyBlock()) // sets testreg + client seam

	stdout, _, err := runRecipeCmd(t, "registry", "refresh")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !strings.Contains(stdout, "testreg") || !strings.Contains(stdout, "recipe(s), ok") {
		t.Fatalf("stdout: %s", stdout)
	}

	// Refreshing a nonexistent registry fails.
	if _, _, err := runRecipeCmd(t, "registry", "refresh", "nope"); err == nil {
		t.Fatal("refreshing a nonexistent registry should fail")
	}
}
