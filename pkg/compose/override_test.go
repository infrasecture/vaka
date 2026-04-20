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
		Image       string   `yaml:"image"`
		Entrypoint  []string `yaml:"entrypoint"`
		Command     []string `yaml:"command"`
		CapAdd      []string `yaml:"cap_add"`
		Restart     string   `yaml:"restart"`
		Attach      *bool    `yaml:"attach"`
		VolumesFrom []string `yaml:"volumes_from"`
		DependsOn   map[string]struct {
			Condition string `yaml:"condition"`
		} `yaml:"depends_on"`
		Secrets []struct {
			Source string `yaml:"source"`
			Target string `yaml:"target"`
		} `yaml:"secrets"`
	} `yaml:"services"`
}

func parseOverride(t *testing.T, yaml_str string) overrideDoc {
	t.Helper()
	var doc overrideDoc
	if err := yaml.Unmarshal([]byte(yaml_str), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

const testImage = "emsi/vaka-init:v0.1.0"

func singleEntry(name string) []compose.ServiceEntry {
	return []compose.ServiceEntry{{
		Name:       name,
		Entrypoint: []string{"claude"},
		Command:    []string{"--dangerously-skip-permissions"},
		CapDelta:   []string{"NET_ADMIN"},
		EnvVarName: "VAKA_CODEX_CONF",
	}}
}

func TestOverrideSecretNameDerivedFromService(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	if _, ok := doc.Secrets["vaka_codex_conf"]; !ok {
		t.Errorf("expected secret key 'vaka_codex_conf'; got secrets: %+v", doc.Secrets)
	}
	if doc.Secrets["vaka_codex_conf"].Environment != "VAKA_CODEX_CONF" {
		t.Errorf("secret env = %q, want VAKA_CODEX_CONF", doc.Secrets["vaka_codex_conf"].Environment)
	}
}

func TestOverrideEntrypointIsVakaInitAbsPath(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]
	if len(svc.Entrypoint) < 2 || svc.Entrypoint[0] != "/opt/vaka/sbin/vaka-init" || svc.Entrypoint[1] != "--" {
		t.Errorf("entrypoint = %v, want [/opt/vaka/sbin/vaka-init --]", svc.Entrypoint)
	}
}

func TestOverrideCommandIsOriginalEntrypoint(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]
	if len(svc.Command) == 0 || svc.Command[0] != "claude" {
		t.Errorf("command = %v, want [claude --dangerously-skip-permissions]", svc.Command)
	}
}

func TestOverrideCapAddContainsDelta(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	for _, cap := range doc.Services["codex"].CapAdd {
		if cap == "NET_ADMIN" {
			return
		}
	}
	t.Errorf("cap_add does not contain NET_ADMIN; got %v", doc.Services["codex"].CapAdd)
}

func TestOverrideSecretMountTargetIsVakaYaml(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	secrets := doc.Services["codex"].Secrets
	if len(secrets) == 0 {
		t.Fatal("no secrets in service override")
	}
	if secrets[0].Target != "vaka.yaml" {
		t.Errorf("secret target = %q, want vaka.yaml", secrets[0].Target)
	}
}

func TestOverrideHyphensInServiceNameBecomesUnderscores(t *testing.T) {
	entries := []compose.ServiceEntry{{
		Name:       "llm-gateway",
		Entrypoint: []string{"/usr/local/bin/litellm"},
		EnvVarName: "VAKA_LLM_GATEWAY_CONF",
	}}
	out, _ := compose.BuildOverride(entries, testImage)
	if !strings.Contains(out, "vaka_llm_gateway_conf") {
		t.Errorf("expected secret key with underscores; got:\n%s", out)
	}
}

func TestOverrideVakaInitContainerEmitted(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	container, ok := doc.Services["__vaka-init"]
	if !ok {
		t.Fatalf("__vaka-init service not in override:\n%s", out)
	}
	if container.Image != testImage {
		t.Errorf("__vaka-init image = %q, want %q", container.Image, testImage)
	}
	if len(container.Entrypoint) != 1 || container.Entrypoint[0] != "/opt/vaka/sbin/vaka-init" {
		t.Errorf("__vaka-init entrypoint = %v, want [/opt/vaka/sbin/vaka-init]", container.Entrypoint)
	}
	if container.Restart != "no" {
		t.Errorf("__vaka-init restart = %q, want no", container.Restart)
	}
	// attach: false — compose must not stream __vaka-init's logs or wait on it
	// for foreground exit. Without this, `vaka up` returns as soon as the
	// short-lived helper completes, breaking the prior pass-through UX.
	if container.Attach == nil || *container.Attach != false {
		t.Errorf("__vaka-init attach = %v, want &false", container.Attach)
	}
}

func TestOverrideServiceGetsVolumesFromAndDependsOn(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]
	if len(svc.VolumesFrom) != 1 || svc.VolumesFrom[0] != "__vaka-init:ro" {
		t.Errorf("volumes_from = %v, want [__vaka-init:ro]", svc.VolumesFrom)
	}
	dep, ok := svc.DependsOn["__vaka-init"]
	if !ok {
		t.Errorf("depends_on missing __vaka-init; got %+v", svc.DependsOn)
	}
	if dep.Condition != "service_completed_successfully" {
		t.Errorf("depends_on condition = %q, want service_completed_successfully", dep.Condition)
	}
}

func TestOverrideNoVakaInitContainerWhenImageEmpty(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), "")
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	if _, ok := doc.Services["__vaka-init"]; ok {
		t.Errorf("__vaka-init must not be emitted when imageRef is empty:\n%s", out)
	}
	svc := doc.Services["codex"]
	if len(svc.VolumesFrom) != 0 {
		t.Errorf("volumes_from must be empty when imageRef is empty, got %v", svc.VolumesFrom)
	}
}

func TestOverridePerServiceOptOut(t *testing.T) {
	entries := []compose.ServiceEntry{
		{Name: "svc-a", Entrypoint: []string{"a"}, EnvVarName: "VAKA_SVC_A_CONF", OptOut: false},
		{Name: "svc-b", Entrypoint: []string{"b"}, EnvVarName: "VAKA_SVC_B_CONF", OptOut: true},
	}
	out, err := compose.BuildOverride(entries, testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	// __vaka-init container still emitted because svc-a needs it.
	if _, ok := doc.Services["__vaka-init"]; !ok {
		t.Errorf("__vaka-init must be emitted when at least one service needs injection:\n%s", out)
	}
	// svc-a gets volumes_from.
	if len(doc.Services["svc-a"].VolumesFrom) == 0 {
		t.Errorf("svc-a must have volumes_from")
	}
	// svc-b does NOT get volumes_from.
	if len(doc.Services["svc-b"].VolumesFrom) != 0 {
		t.Errorf("svc-b must not have volumes_from when OptOut=true, got %v", doc.Services["svc-b"].VolumesFrom)
	}
}

func TestOverrideAllOptOutNoVakaInitContainer(t *testing.T) {
	entries := []compose.ServiceEntry{
		{Name: "svc-a", Entrypoint: []string{"a"}, EnvVarName: "VAKA_SVC_A_CONF", OptOut: true},
		{Name: "svc-b", Entrypoint: []string{"b"}, EnvVarName: "VAKA_SVC_B_CONF", OptOut: true},
	}
	out, err := compose.BuildOverride(entries, testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	if _, ok := doc.Services["__vaka-init"]; ok {
		t.Errorf("__vaka-init must not be emitted when all services opt out:\n%s", out)
	}
}

func TestBuildVakaInitOnlyOverride(t *testing.T) {
	out, err := compose.BuildVakaInitOnlyOverride(testImage)
	if err != nil {
		t.Fatalf("BuildVakaInitOnlyOverride: %v", err)
	}
	doc := parseOverride(t, out)
	container, ok := doc.Services["__vaka-init"]
	if !ok {
		t.Fatalf("__vaka-init not in vaka-init-only override:\n%s", out)
	}
	if container.Image != testImage {
		t.Errorf("image = %q, want %q", container.Image, testImage)
	}
	if container.Attach == nil || *container.Attach != false {
		t.Errorf("__vaka-init attach = %v, want &false", container.Attach)
	}
	// Must not contain any other services or secrets.
	if len(doc.Secrets) != 0 {
		t.Errorf("vaka-init-only override must have no secrets, got %+v", doc.Secrets)
	}
	if len(doc.Services) != 1 {
		t.Errorf("vaka-init-only override must have exactly 1 service, got %d", len(doc.Services))
	}
}
