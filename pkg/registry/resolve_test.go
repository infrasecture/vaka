package registry

import (
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	valid := []struct {
		in   string
		want Ref
	}{
		{"codex", Ref{Name: "codex"}},
		{"official/codex", Ref{Registry: "official", Name: "codex"}},
		{"codex@1.2.3", Ref{Name: "codex", Version: "1.2.3"}},
		{"acme/my-recipe@0.1.0", Ref{Registry: "acme", Name: "my-recipe", Version: "0.1.0"}},
	}
	for _, tc := range valid {
		got, err := ParseRef(tc.in)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseRef(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}

	invalid := []struct {
		in   string
		want string
	}{
		{"a/b/c", "more than one '/'"},
		{"x@1.2.3@4.5.6", "more than one '@'"},
		{"Codex", "must match [a-z0-9-]+"},
		{"ACME/codex", "must match [a-z0-9-]+"},
		{"codex@^1.2", "constraints are not supported"},
		{"codex@>=0.2.0", "constraints are not supported"},
		{"codex@1.2", "exact SemVer"},
		{"codex@", "exact SemVer"},
		{"", "must match [a-z0-9-]+"},
	}
	for _, tc := range invalid {
		_, err := ParseRef(tc.in)
		if err == nil {
			t.Fatalf("ParseRef(%q) unexpectedly succeeded", tc.in)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("ParseRef(%q) error %q missing %q", tc.in, err.Error(), tc.want)
		}
	}
}

func testWorld() (*Config, map[string]*Index) {
	cfg := &Config{
		APIVersion: APIVersion,
		Kind:       "RegistriesConfig",
		Registries: []Registry{
			{Name: "official", URL: "https://official.example/index.yaml"},
			{Name: "acme", URL: "https://acme.example/index.yaml"},
		},
	}
	indexes := map[string]*Index{
		"official": {Recipes: map[string][]IndexEntry{
			"codex": {
				{Version: "0.2.0", Digest: "sha256:aa"},
				{Version: "0.10.0", Digest: "sha256:bb"},
				{Version: "0.9.0", Digest: "sha256:cc"},
			},
			"shared": {{Version: "1.0.0"}},
		}},
		"acme": {Recipes: map[string][]IndexEntry{
			"shared":  {{Version: "2.0.0"}},
			"private": {{Version: "1.0.0"}, {Version: "not-semver"}},
		}},
	}
	return cfg, indexes
}

func TestResolveUnqualifiedUnique(t *testing.T) {
	cfg, indexes := testWorld()
	res, err := Resolve(cfg, indexes, Ref{Name: "codex"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Registry.Name != "official" || res.Entry.Version != "0.10.0" {
		t.Fatalf("resolved %s@%s from %s, want codex@0.10.0 from official (semver order, not lexical)",
			res.Name, res.Entry.Version, res.Registry.Name)
	}
}

func TestResolveAmbiguousNameDemandsQualification(t *testing.T) {
	cfg, indexes := testWorld()
	_, err := Resolve(cfg, indexes, Ref{Name: "shared"})
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	for _, want := range []string{"more than one configured registry", "acme/shared", "official/shared"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestResolveQualified(t *testing.T) {
	cfg, indexes := testWorld()
	res, err := Resolve(cfg, indexes, Ref{Registry: "acme", Name: "shared"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Registry.Name != "acme" || res.Entry.Version != "2.0.0" {
		t.Fatalf("resolved %+v", res)
	}
}

func TestResolveExactVersion(t *testing.T) {
	cfg, indexes := testWorld()
	res, err := Resolve(cfg, indexes, Ref{Name: "codex", Version: "0.9.0"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Entry.Version != "0.9.0" {
		t.Fatalf("resolved %s, want 0.9.0", res.Entry.Version)
	}

	_, err = Resolve(cfg, indexes, Ref{Name: "codex", Version: "3.0.0"})
	if err == nil || !strings.Contains(err.Error(), "no published version 3.0.0") {
		t.Fatalf("err = %v, want unknown-version error", err)
	}
}

func TestResolveErrors(t *testing.T) {
	cfg, indexes := testWorld()

	if _, err := Resolve(cfg, indexes, Ref{Registry: "nope", Name: "codex"}); err == nil ||
		!strings.Contains(err.Error(), `registry "nope" is not configured`) {
		t.Fatalf("unknown registry: %v", err)
	}
	if _, err := Resolve(cfg, indexes, Ref{Name: "missing"}); err == nil ||
		!strings.Contains(err.Error(), "not found in any configured registry") {
		t.Fatalf("unknown recipe: %v", err)
	}
	if _, err := Resolve(cfg, indexes, Ref{Registry: "official", Name: "private"}); err == nil ||
		!strings.Contains(err.Error(), `not found in registry "official"`) {
		t.Fatalf("recipe in wrong registry: %v", err)
	}

	// A registry whose index failed to load is skipped for unqualified
	// resolution but is a hard error when explicitly addressed.
	delete(indexes, "acme")
	if res, err := Resolve(cfg, indexes, Ref{Name: "shared"}); err != nil {
		t.Fatalf("unqualified with missing index: %v", err)
	} else if res.Registry.Name != "official" {
		t.Fatalf("resolved from %s, want official", res.Registry.Name)
	}
	if _, err := Resolve(cfg, indexes, Ref{Registry: "acme", Name: "shared"}); err == nil ||
		!strings.Contains(err.Error(), "index unavailable") {
		t.Fatalf("qualified with missing index: %v", err)
	}
}

func TestResolveToleratesMalformedVersions(t *testing.T) {
	cfg, indexes := testWorld()
	res, err := Resolve(cfg, indexes, Ref{Registry: "acme", Name: "private"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Entry.Version != "1.0.0" {
		t.Fatalf("resolved %s, want 1.0.0 (malformed entry skipped)", res.Entry.Version)
	}
}
