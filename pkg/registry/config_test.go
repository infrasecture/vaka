package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registries.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigAddRemove(t *testing.T) {
	cfg := DefaultConfig()

	if err := cfg.Add(Registry{Name: "acme", URL: "https://recipes.acme.example/index.yaml"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(cfg.Registries) != 2 {
		t.Fatalf("got %d registries, want 2", len(cfg.Registries))
	}

	if err := cfg.Add(Registry{Name: "acme", URL: "https://other.example/index.yaml"}); err == nil ||
		!strings.Contains(err.Error(), "already configured") {
		t.Fatalf("duplicate add err = %v", err)
	}
	if err := cfg.Add(Registry{Name: "Bad_Name", URL: "https://a.example/index.yaml"}); err == nil ||
		!strings.Contains(err.Error(), "[a-z0-9-]+") {
		t.Fatalf("bad-name add err = %v", err)
	}
	if err := cfg.Add(Registry{Name: "insecure", URL: "http://a.example/index.yaml"}); err == nil ||
		!strings.Contains(err.Error(), "plain http") {
		t.Fatalf("http add err = %v", err)
	}

	if err := cfg.Remove("acme"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := cfg.Lookup("acme"); ok {
		t.Fatal("acme still present after remove")
	}
	if err := cfg.Remove("nonexistent"); err == nil ||
		!strings.Contains(err.Error(), "not configured") {
		t.Fatalf("remove-nonexistent err = %v", err)
	}
}

func TestSaveConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "registries.yaml")
	cfg := DefaultConfig()
	_ = cfg.Add(Registry{Name: "acme", URL: "https://recipes.acme.example/index.yaml"})

	if err := SaveConfigTo(path, cfg); err != nil {
		t.Fatalf("SaveConfigTo: %v", err)
	}
	back, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if len(back.Registries) != 2 || back.Registries[1].Name != "acme" {
		t.Fatalf("round trip = %+v", back.Registries)
	}
	// The saved document is strictly valid (identity fields set).
	if back.APIVersion != APIVersion || back.Kind != "RegistriesConfig" {
		t.Fatalf("identity = %s/%s", back.APIVersion, back.Kind)
	}
}

func TestLoadConfigMissingFileYieldsDefault(t *testing.T) {
	cfg, err := LoadConfigFrom(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if len(cfg.Registries) != 1 || cfg.Registries[0] != Official {
		t.Fatalf("default config = %+v, want just the official registry", cfg.Registries)
	}
}

func TestLoadConfigValid(t *testing.T) {
	path := writeConfig(t, `
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
registries:
  - name: official
    url: https://infrasecture.github.io/vaka-registry/index.yaml
  - name: acme
    url: https://recipes.acme.example/index.yaml
`)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if len(cfg.Registries) != 2 {
		t.Fatalf("got %d registries, want 2", len(cfg.Registries))
	}
	if reg, ok := cfg.Lookup("acme"); !ok || reg.URL != "https://recipes.acme.example/index.yaml" {
		t.Fatalf("Lookup(acme) = %+v, %v", reg, ok)
	}
	if _, ok := cfg.Lookup("nope"); ok {
		t.Fatal("Lookup(nope) unexpectedly succeeded")
	}
}

func TestLoadConfigRejections(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "duplicate names",
			content: `
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
registries:
  - {name: acme, url: "https://a.example/index.yaml"}
  - {name: acme, url: "https://b.example/index.yaml"}
`,
			want: "duplicate registry name",
		},
		{
			name: "bad charset",
			content: `
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
registries:
  - {name: "Acme_Corp", url: "https://a.example/index.yaml"}
`,
			want: "must match [a-z0-9-]+",
		},
		{
			name: "plain http rejected",
			content: `
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
registries:
  - {name: acme, url: "http://a.example/index.yaml"}
`,
			want: "plain http URLs are not allowed",
		},
		{
			name: "unknown scheme rejected",
			content: `
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
registries:
  - {name: acme, url: "ftp://a.example/index.yaml"}
`,
			want: "unsupported URL scheme",
		},
		{
			name: "unknown field rejected (vaka-owned document is strict)",
			content: `
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
surprise: true
registries: []
`,
			want: "surprise",
		},
		{
			name: "wrong kind",
			content: `
apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
registries: []
`,
			want: "kind must be",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfigFrom(writeConfig(t, tc.content))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}
