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
