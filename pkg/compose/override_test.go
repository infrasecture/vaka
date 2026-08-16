package compose_test

import (
	"encoding/hex"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
)

type overrideDoc struct {
	Metadata struct {
		RuntimeVersion string `yaml:"runtime-version"`
		RuntimeImage   string `yaml:"runtime-image"`
	} `yaml:"x-vaka"`
	Services map[string]struct {
		Image       string                             `yaml:"image"`
		PullPolicy  string                             `yaml:"pull_policy"`
		User        string                             `yaml:"user"`
		Entrypoint  []string                           `yaml:"entrypoint"`
		Command     []string                           `yaml:"command"`
		CapAdd      []string                           `yaml:"cap_add"`
		Labels      map[string]string                  `yaml:"labels"`
		Environment map[string]string                  `yaml:"environment"`
		Volumes     []composetypes.ServiceVolumeConfig `yaml:"volumes"`
		VolumesFrom []string                           `yaml:"volumes_from"`
		DependsOn   map[string]any                     `yaml:"depends_on"`
		Healthcheck struct {
			Test    []string `yaml:"test"`
			Disable bool     `yaml:"disable"`
		} `yaml:"healthcheck"`
	} `yaml:"services"`
}

func parseOverride(t *testing.T, raw string) overrideDoc {
	t.Helper()
	var doc overrideDoc
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

const testImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var testRuntime = compose.RuntimeMount{ImageID: testImageID, Version: "v0.1.0"}

func singleEntry(name string) []compose.ServiceEntry {
	return []compose.ServiceEntry{{
		Name:           name,
		ImageID:        "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Entrypoint:     []string{"claude"},
		Command:        []string{"--dangerously-skip-permissions"},
		CapDelta:       []string{"NET_ADMIN"},
		EnvVarName:     "VAKA_CODEX_CONF",
		PolicyRevision: "sha256:policy",
	}}
}

func TestBuildOverrideInjectsPolicyRuntime(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), testRuntime)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]

	if len(svc.Entrypoint) != 2 || svc.Entrypoint[0] != "/opt/vaka/sbin/vaka-init" || svc.Entrypoint[1] != "--" {
		t.Errorf("entrypoint = %v, want [/opt/vaka/sbin/vaka-init --]", svc.Entrypoint)
	}
	if svc.User != "0:0" {
		t.Errorf("user = %q, want 0:0", svc.User)
	}
	if svc.Image != singleEntry("codex")[0].ImageID || svc.PullPolicy != "never" {
		t.Errorf("service image identity = %q pull_policy=%q, want exact inspected ID with pulling disabled", svc.Image, svc.PullPolicy)
	}
	if got := strings.Join(svc.Command, " "); got != "claude --dangerously-skip-permissions" {
		t.Errorf("command = %q", got)
	}
	if len(svc.CapAdd) != 1 || svc.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("cap_add = %v, want [NET_ADMIN]", svc.CapAdd)
	}
	if svc.Environment["AGENT_VAKA_POLICY"] != "${VAKA_CODEX_CONF}" || svc.Environment["AGENT_VAKA_POLICY_REVISION"] != "sha256:policy" {
		t.Errorf("policy environment = %+v", svc.Environment)
	}
}

func TestBuildOverrideFreezesPreparedUnmanagedService(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("app"), testRuntime, "worker")
	if err != nil {
		t.Fatal(err)
	}
	doc := parseOverride(t, out)
	worker, ok := doc.Services["worker"]
	if !ok || worker.PullPolicy != "never" {
		t.Fatalf("prepared unmanaged override = %+v", worker)
	}
	if worker.User != "" || worker.Image != "" || len(worker.Entrypoint) != 0 {
		t.Fatalf("prepared unmanaged service received managed settings: %+v", worker)
	}
}

func TestBuildOverrideExplicitlyDisablesAbsentHealthcheck(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("app"), testRuntime)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	health := parseOverride(t, out).Services["app"].Healthcheck
	if !health.Disable || len(health.Test) != 0 {
		t.Fatalf("healthcheck = %+v, want disable: true", health)
	}
}

func TestBuildOverrideUsesReadOnlyExactIDImageMount(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), testRuntime)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]
	if len(svc.Volumes) != 1 {
		t.Fatalf("volumes = %+v, want one image mount", svc.Volumes)
	}
	mount := svc.Volumes[0]
	if mount.Type != composetypes.VolumeTypeImage || mount.Source != testImageID || mount.Target != "/opt/vaka" || !mount.ReadOnly {
		t.Errorf("mount = %+v, want read-only image %s at /opt/vaka", mount, testImageID)
	}
	if mount.Image == nil || mount.Image.SubPath != "opt/vaka" {
		t.Errorf("image options = %+v, want subpath opt/vaka", mount.Image)
	}
	if len(svc.VolumesFrom) != 0 || len(svc.DependsOn) != 0 {
		t.Errorf("legacy helper references remain: volumes_from=%v depends_on=%v", svc.VolumesFrom, svc.DependsOn)
	}
	if _, exists := doc.Services["__vaka-init"]; exists {
		t.Fatal("override must not define a __vaka-init helper service")
	}
	if strings.Contains(out, "emsi/vaka-init") {
		t.Fatalf("override contains mutable runtime tag:\n%s", out)
	}
}

func TestBuildOverrideMountSourceFitsEngine29LegacyLayerName(t *testing.T) {
	compactRuntime := testRuntime
	compactRuntime.Source = strings.Repeat("a", 40)
	out, err := compose.BuildOverride(singleEntry("codex"), compactRuntime)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	mount := parseOverride(t, out).Services["codex"].Volumes[0]
	legacyLayerIdentity := strings.Repeat("c", 64) + ",src=" + mount.Source + ",dst=" + mount.Target
	if got := hex.EncodedLen(len(legacyLayerIdentity)); got > 255 {
		t.Fatalf("Engine 29.0/29.1 layer name length = %d, exceeds NAME_MAX", got)
	}
}

func TestBuildOverrideLabelsPolicyAndRuntimeIdentity(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testRuntime)
	doc := parseOverride(t, out)
	labels := doc.Services["codex"].Labels
	want := map[string]string{
		compose.ManagedLabel:        "true",
		compose.PolicyRevisionLabel: "sha256:policy",
		compose.ServiceImageLabel:   singleEntry("codex")[0].ImageID,
		compose.RuntimeImageLabel:   testImageID,
		compose.RuntimeVersionLabel: "v0.1.0",
	}
	for key, value := range want {
		if labels[key] != value {
			t.Errorf("label %s = %q, want %q", key, labels[key], value)
		}
	}
	if doc.Metadata.RuntimeVersion != testRuntime.Version || doc.Metadata.RuntimeImage != testImageID {
		t.Errorf("x-vaka = %+v, want runtime identity", doc.Metadata)
	}
}

func TestBuildOverrideWrapsHealthchecksThroughTrampoline(t *testing.T) {
	tests := []struct {
		name  string
		test  []string
		shell []string
		want  []string
	}{
		{
			name: "exec form",
			test: []string{"CMD", "curl", "-f", "http://localhost"},
			want: []string{"CMD", "/opt/vaka/sbin/vaka-init", "exec", "--", "curl", "-f", "http://localhost"},
		},
		{
			name: "shell form default shell",
			test: []string{"CMD-SHELL", "curl -f http://localhost || exit 1"},
			want: []string{"CMD", "/opt/vaka/sbin/vaka-init", "exec", "--", "/bin/sh", "-c", "curl -f http://localhost || exit 1"},
		},
		{
			name:  "shell form image shell",
			test:  []string{"CMD-SHELL", "Test-Path C:\\ready"},
			shell: []string{"pwsh", "-Command"},
			want:  []string{"CMD", "/opt/vaka/sbin/vaka-init", "exec", "--", "pwsh", "-Command", "Test-Path C:\\ready"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries := singleEntry("app")
			entries[0].Healthcheck = tc.test
			entries[0].HealthcheckShell = tc.shell
			out, err := compose.BuildOverride(entries, testRuntime)
			if err != nil {
				t.Fatalf("BuildOverride: %v", err)
			}
			got := parseOverride(t, out).Services["app"].Healthcheck.Test
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("healthcheck = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildOverrideRejectsMalformedHealthcheck(t *testing.T) {
	entries := singleEntry("app")
	entries[0].Healthcheck = []string{"CMD"}
	if _, err := compose.BuildOverride(entries, testRuntime); err == nil || !strings.Contains(err.Error(), "has no command") {
		t.Fatalf("malformed healthcheck error = %v", err)
	}
}

func TestBuildOverrideOptOutSkipsOnlyImageMount(t *testing.T) {
	entries := singleEntry("codex")
	entries[0].OptOut = true
	out, err := compose.BuildOverride(entries, testRuntime)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	svc := parseOverride(t, out).Services["codex"]
	if len(svc.Volumes) != 0 {
		t.Errorf("opted-out service volumes = %+v, want none", svc.Volumes)
	}
	if svc.Labels[compose.PolicyRevisionLabel] == "" || svc.Labels[compose.RuntimeVersionLabel] == "" {
		t.Errorf("opted-out service lost policy/runtime labels: %+v", svc.Labels)
	}
	if _, exists := svc.Labels[compose.RuntimeImageLabel]; exists {
		t.Errorf("opted-out service must not claim a mounted runtime image: %+v", svc.Labels)
	}
}

func TestBuildOverrideSupportsGloballyBakedRuntime(t *testing.T) {
	runtime := testRuntime
	runtime.ImageID = ""
	out, err := compose.BuildOverride(singleEntry("codex"), runtime)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	if got := parseOverride(t, out).Services["codex"].Volumes; len(got) != 0 {
		t.Errorf("globally baked runtime volumes = %+v, want none", got)
	}
}

func TestBuildOverrideRejectsMissingIdentity(t *testing.T) {
	entries := singleEntry("codex")
	entries[0].PolicyRevision = ""
	if _, err := compose.BuildOverride(entries, testRuntime); err == nil || !strings.Contains(err.Error(), "policy revision") {
		t.Fatalf("missing policy revision error = %v", err)
	}
	if _, err := compose.BuildOverride(singleEntry("codex"), compose.RuntimeMount{}); err == nil || !strings.Contains(err.Error(), "runtime version") {
		t.Fatalf("missing runtime version error = %v", err)
	}
	invalidRuntime := testRuntime
	invalidRuntime.ImageID = "sha256:short"
	if _, err := compose.BuildOverride(singleEntry("codex"), invalidRuntime); err == nil || !strings.Contains(err.Error(), "sha256:<64 hex>") {
		t.Fatalf("invalid runtime image ID error = %v", err)
	}
	invalidRuntime = testRuntime
	invalidRuntime.Source = strings.Repeat("b", 40)
	if _, err := compose.BuildOverride(singleEntry("codex"), invalidRuntime); err == nil || !strings.Contains(err.Error(), "does not identify image") {
		t.Fatalf("unrelated runtime mount source error = %v", err)
	}
}

func TestBuildReferenceOverrideContainsOnlyRuntimeMetadata(t *testing.T) {
	out, err := compose.BuildReferenceOverride("v0.1.0")
	if err != nil {
		t.Fatalf("BuildReferenceOverride: %v", err)
	}
	doc := parseOverride(t, out)
	if doc.Metadata.RuntimeVersion != "v0.1.0" {
		t.Errorf("runtime version = %q, want v0.1.0", doc.Metadata.RuntimeVersion)
	}
	if len(doc.Services) != 0 {
		t.Errorf("reference override has services: %+v", doc)
	}
}
