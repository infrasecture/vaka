package registry

import (
	"strings"
	"testing"
)

// realisticIndex mirrors the live official index format (CI-generated).
const realisticIndex = `
apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
generated: '2026-07-10T22:26:52Z'
recipes:
  codex:
  - version: 0.1.0
    description: Codex agent in an egress-restricted container
    tags: [agent, openai]
    created: '2026-07-10T22:26:52Z'
    digest: sha256:c2021014b22c8a3029a04fcb24d0750717a628821eca394daa103c598c99a7f1
    urls:
    - https://github.com/infrasecture/vaka-registry/releases/download/codex-0.1.0/codex-0.1.0.tar.gz
    minVakaVersion: 0.0.2
    env:
    - name: OPENAI_API_KEY
      required: true
      description: Model provider API key
    policy:
      defaultActions: {codex: reject, litellm: reject}
      riskFlags: []
`

func TestParseIndexRealistic(t *testing.T) {
	idx, err := ParseIndex([]byte(realisticIndex))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	entries := idx.Recipes["codex"]
	if len(entries) != 1 {
		t.Fatalf("got %d codex entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Version != "0.1.0" || !strings.HasPrefix(e.Digest, "sha256:c2021014") {
		t.Fatalf("entry = %+v", e)
	}
	if len(e.Env) != 1 || e.Env[0].Name != "OPENAI_API_KEY" || !e.Env[0].Required {
		t.Fatalf("env = %+v", e.Env)
	}
	if e.Policy == nil || e.Policy.DefaultActions["litellm"] != "reject" || len(e.Policy.RiskFlags) != 0 {
		t.Fatalf("policy = %+v", e.Policy)
	}
}

func TestParseIndexToleratesUnknownFields(t *testing.T) {
	// Registries may be newer than this client: lenient decoding.
	idx, err := ParseIndex([]byte(`
apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
futureTopLevelField: hello
recipes:
  demo:
  - version: 1.0.0
    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
    urls: [https://example.com/demo-1.0.0.tar.gz]
    futureEntryField: {nested: true}
`))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	if idx.Recipes["demo"][0].Version != "1.0.0" {
		t.Fatalf("entry = %+v", idx.Recipes["demo"][0])
	}
}

func TestParseIndexRejectsWrongIdentity(t *testing.T) {
	for _, doc := range []string{
		"apiVersion: recipes.vaka/v2\nkind: RegistryIndex\n",
		"apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\n",
	} {
		if _, err := ParseIndex([]byte(doc)); err == nil {
			t.Fatalf("ParseIndex accepted %q", doc)
		}
	}
}

func TestParseIndexEmptyRecipesIsNonNil(t *testing.T) {
	idx, err := ParseIndex([]byte("apiVersion: recipes.vaka/v1alpha1\nkind: RegistryIndex\n"))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	if idx.Recipes == nil {
		t.Fatal("Recipes map must be non-nil")
	}
}
