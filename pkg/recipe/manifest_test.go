package recipe

import (
	"strings"
	"testing"
)

func TestParseManifestValid(t *testing.T) {
	data := []byte(`apiVersion: recipes.vaka/v1alpha1
kind: Recipe
name: codex
version: 0.3.0
description: Codex agent in an egress-restricted container
homepage: https://example.com
tags: [agent, openai]
minVakaVersion: 0.1.0
env:
  - name: OPENAI_API_KEY
    required: true
    description: forwarded to the gateway
riskAcknowledgements:
  - flag: privileged
    reason: needs it
`)
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "codex" || m.Version != "0.3.0" || m.MinVakaVersion != "0.1.0" {
		t.Fatalf("manifest = %+v", m)
	}
	if len(m.Env) != 1 || m.Env[0].Name != "OPENAI_API_KEY" || !m.Env[0].Required {
		t.Fatalf("env = %+v", m.Env)
	}
}

func TestParseManifestRejects(t *testing.T) {
	base := "apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: demo\nversion: 1.0.0\ndescription: x\n"
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"not a mapping", "- just\n- a\n- list\n", "must be a YAML mapping"},
		{"wrong apiVersion", strings.Replace(base, "recipes.vaka/v1alpha1", "other/v1", 1), "apiVersion must be"},
		{"wrong kind", strings.Replace(base, "kind: Recipe", "kind: Nope", 1), "kind must be"},
		{"bad name", strings.Replace(base, "name: demo", "name: Demo_Bad", 1), "name must match"},
		{"non-strict version", strings.Replace(base, "version: 1.0.0", "version: 1.2", 1), "strict SemVer"},
		{"prerelease version", strings.Replace(base, "version: 1.0.0", "version: 1.0.0-rc1", 1), "strict SemVer"},
		{"empty description", strings.Replace(base, "description: x", "description: \"\"", 1), "description is required"},
		{"bad minVakaVersion", base + "minVakaVersion: latest\n", "minVakaVersion"},
		{"reserved field", base + "provides: [db]\n", "reserved"},
		{"unknown field", base + "surprise: true\n", "unknown field"},
		{"env without name", base + "env:\n  - description: no name\n", "needs a string 'name'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestManifestCheckMinVakaVersion(t *testing.T) {
	m := &Manifest{MinVakaVersion: "1.2.0"}

	if err := m.CheckMinVakaVersion("1.2.0"); err != nil {
		t.Fatalf("equal version rejected: %v", err)
	}
	if err := m.CheckMinVakaVersion("2.0.0"); err != nil {
		t.Fatalf("newer version rejected: %v", err)
	}
	if err := m.CheckMinVakaVersion("1.1.9"); err == nil ||
		!strings.Contains(err.Error(), "requires vaka >= 1.2.0") {
		t.Fatalf("older version not refused: %v", err)
	}
	// Dev/unparseable running version cannot be compared: skip, do not block.
	if err := m.CheckMinVakaVersion("dev"); err != nil {
		t.Fatalf("dev build must skip enforcement: %v", err)
	}
	// No floor declared: nothing to enforce.
	if err := (&Manifest{}).CheckMinVakaVersion("0.0.1"); err != nil {
		t.Fatalf("empty minVakaVersion rejected: %v", err)
	}
}

func TestManifestCheckIdentity(t *testing.T) {
	m := &Manifest{Name: "demo", Version: "1.0.0"}

	if err := m.CheckIdentity(ExpectedIdentity{Name: "demo", Version: "1.0.0"}); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if err := m.CheckIdentity(ExpectedIdentity{}); err != nil {
		t.Fatalf("empty expectation must skip the check: %v", err)
	}
	if err := m.CheckIdentity(ExpectedIdentity{Name: "other", Version: "1.0.0"}); err == nil ||
		!strings.Contains(err.Error(), "name") {
		t.Fatalf("name mismatch not caught: %v", err)
	}
	if err := m.CheckIdentity(ExpectedIdentity{Name: "demo", Version: "2.0.0"}); err == nil ||
		!strings.Contains(err.Error(), "version") {
		t.Fatalf("version mismatch not caught: %v", err)
	}
}
