// pkg/compose/override_test.go
package compose_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
)

type overrideDoc struct {
	Secrets  map[string]struct {
		Environment string `yaml:"environment"`
	} `yaml:"secrets"`
	Services map[string]struct {
		Entrypoint []string `yaml:"entrypoint"`
		Command    []string `yaml:"command"`
		CapAdd     []string `yaml:"cap_add"`
		Secrets    []struct {
			Source string `yaml:"source"`
			Target string `yaml:"target"`
		} `yaml:"secrets"`
	} `yaml:"services"`
}

func TestOverrideSecretNameDerivedFromService(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:        "codex",
			Entrypoint:  []string{"claude"},
			Command:     []string{"--dangerously-skip-permissions"},
			CapDelta:    []string{"NET_ADMIN"},
			EnvVarName:  "VAKA_CODEX_CONF",
		},
	}
	out, err := compose.BuildOverride(entries)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}

	var doc overrideDoc
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := doc.Secrets["vaka_codex_conf"]; !ok {
		t.Errorf("expected secret key 'vaka_codex_conf'; got secrets: %+v", doc.Secrets)
	}
	if doc.Secrets["vaka_codex_conf"].Environment != "VAKA_CODEX_CONF" {
		t.Errorf("secret env = %q, want VAKA_CODEX_CONF", doc.Secrets["vaka_codex_conf"].Environment)
	}
}

func TestOverrideEntrypointIsVakaInit(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			Command:    []string{"--dangerously-skip-permissions"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	svc := doc.Services["codex"]
	if len(svc.Entrypoint) < 2 || svc.Entrypoint[0] != "vaka-init" || svc.Entrypoint[1] != "--" {
		t.Errorf("entrypoint = %v, want [vaka-init --]", svc.Entrypoint)
	}
}

func TestOverrideCommandIsOriginalEntrypoint(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			Command:    []string{"--dangerously-skip-permissions"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	svc := doc.Services["codex"]
	// command must be: original entrypoint + original command concatenated
	if len(svc.Command) == 0 || svc.Command[0] != "claude" {
		t.Errorf("command = %v, want [claude --dangerously-skip-permissions]", svc.Command)
	}
}

func TestOverrideCapAddContainsDelta(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			CapDelta:   []string{"NET_ADMIN"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	for _, cap := range doc.Services["codex"].CapAdd {
		if cap == "NET_ADMIN" {
			return
		}
	}
	t.Errorf("cap_add does not contain NET_ADMIN; got %v\n%s", doc.Services["codex"].CapAdd, out)
}

func TestOverrideSecretMountTargetIsVakaYaml(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "codex",
			Entrypoint: []string{"claude"},
			EnvVarName: "VAKA_CODEX_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)

	var doc overrideDoc
	yaml.Unmarshal([]byte(out), &doc) //nolint
	secrets := doc.Services["codex"].Secrets
	if len(secrets) == 0 {
		t.Fatal("no secrets in service override")
	}
	if secrets[0].Target != "vaka.yaml" {
		t.Errorf("secret target = %q, want vaka.yaml", secrets[0].Target)
	}
}

func TestOverrideHyphensInServiceNameBecomesUnderscores(t *testing.T) {
	entries := []compose.ServiceEntry{
		{
			Name:       "llm-gateway",
			Entrypoint: []string{"/usr/local/bin/litellm"},
			EnvVarName: "VAKA_LLM_GATEWAY_CONF",
		},
	}
	out, _ := compose.BuildOverride(entries)
	if !strings.Contains(out, "vaka_llm_gateway_conf") {
		t.Errorf("expected secret key with underscores; got:\n%s", out)
	}
}
