// cmd/vaka/intercept_test.go
package main

import (
	"context"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"vaka.dev/vaka/pkg/policy"
)

func TestClassifySubcmd(t *testing.T) {
	tests := []struct {
		subcmd string
		want   subcmdPath
	}{
		{"up", pathFull},
		{"run", pathFull},
		{"create", pathFull},
		{"down", pathLifecycle},
		{"stop", pathLifecycle},
		{"kill", pathLifecycle},
		{"rm", pathLifecycle},
		{"validate", pathCobra},
		{"show", pathCobra},
		{"version", pathCobra},
		{"", pathCobra},
		{"logs", pathPassthrough},
		{"ps", pathPassthrough},
		{"exec", pathPassthrough},
		{"pull", pathPassthrough},
	}
	for _, tc := range tests {
		if got := classifySubcmd(tc.subcmd); got != tc.want {
			t.Errorf("classifySubcmd(%q) = %v, want %v", tc.subcmd, got, tc.want)
		}
	}
}

func TestLifecycleOverrideYAMLPassthrough(t *testing.T) {
	yaml, err := lifecycleOverrideYAML(true, "emsi/vaka-init:v0.1.0")
	if err != nil {
		t.Fatalf("passthrough: unexpected error: %v", err)
	}
	if yaml != "" {
		t.Errorf("passthrough: expected empty string, got:\n%s", yaml)
	}
}

func TestLifecycleOverrideYAMLInjectsContainer(t *testing.T) {
	yaml, err := lifecycleOverrideYAML(false, "emsi/vaka-init:v0.1.0")
	if err != nil {
		t.Fatalf("injection: unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "__vaka-init") {
		t.Errorf("injection: expected __vaka-init in YAML, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "emsi/vaka-init:v0.1.0") {
		t.Errorf("injection: expected image ref in YAML, got:\n%s", yaml)
	}
}

// fakeDS is a minimal DockerServices used to drive servicesNeedingPrebuild.
type fakeDS struct {
	exists map[string]bool // ref -> present locally
}

func (f *fakeDS) EnsureImage(context.Context, string) error { return nil }
func (f *fakeDS) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.exists[ref], nil
}
func (f *fakeDS) ResolveEntrypoint(context.Context, string, composetypes.ServiceConfig) ([]string, []string, error) {
	return nil, nil, nil
}

func TestServicesNeedingPrebuild(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{
		"needsbuild":  {},
		"hasentry":    {},
		"prebuilt":    {},
		"nobuild":     {},
		"buildonly":   {},
		"notinpolicy": {},
	}
	project := &composetypes.Project{
		Services: map[string]composetypes.ServiceConfig{
			// Needs build: no entrypoint, has build, image not local.
			"needsbuild": {
				Image: "myapp:latest",
				Build: &composetypes.BuildConfig{Context: "."},
			},
			// Has entrypoint in compose → no inspect → no pre-build.
			"hasentry": {
				Image:      "app:latest",
				Build:      &composetypes.BuildConfig{Context: "."},
				Entrypoint: []string{"/bin/run"},
			},
			// Has build + image already local → no pre-build needed.
			"prebuilt": {
				Image: "prebuilt:latest",
				Build: &composetypes.BuildConfig{Context: "."},
			},
			// No build section → cannot pre-build even if missing.
			"nobuild": {
				Image: "external:latest",
			},
			// Build-only (no image field) → pre-build unconditionally.
			"buildonly": {
				Build: &composetypes.BuildConfig{Context: "."},
			},
		},
	}
	ds := &fakeDS{exists: map[string]bool{
		"prebuilt:latest": true,
		"myapp:latest":    false,
		"external:latest": false,
	}}

	got, err := servicesNeedingPrebuild(context.Background(), ds, policySvcs, project, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"buildonly", "needsbuild"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestServicesNeedingPrebuildForceRebuild verifies that forceRebuild=true
// includes services whose image already exists locally. Without this, a stale
// local image could be inspected for its ENTRYPOINT even though the final
// `docker compose up --build` will rebuild it to a different image.
func TestServicesNeedingPrebuildForceRebuild(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{
		"prebuilt":  {},
		"buildonly": {},
		"nobuild":   {},
		"hasentry":  {},
	}
	project := &composetypes.Project{
		Services: map[string]composetypes.ServiceConfig{
			// Has image locally + build section → under forceRebuild, still included.
			"prebuilt": {
				Image: "prebuilt:latest",
				Build: &composetypes.BuildConfig{Context: "."},
			},
			// Build-only (no image) → included regardless of forceRebuild.
			"buildonly": {
				Build: &composetypes.BuildConfig{Context: "."},
			},
			// No build section → never prebuilt.
			"nobuild": {
				Image: "external:latest",
			},
			// Has compose-declared entrypoint → no inspection needed → never prebuilt.
			"hasentry": {
				Image:      "app:latest",
				Build:      &composetypes.BuildConfig{Context: "."},
				Entrypoint: []string{"/bin/run"},
			},
		},
	}
	ds := &fakeDS{exists: map[string]bool{
		"prebuilt:latest": true,
		"external:latest": false,
	}}

	got, err := servicesNeedingPrebuild(context.Background(), ds, policySvcs, project, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"buildonly", "prebuilt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestGlobalFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"no flags", []string{"up", "-d"}, nil},
		{"single -f", []string{"-f", "foo.yml", "up", "-d"}, []string{"-f", "foo.yml"}},
		{"multiple globals", []string{"-f", "a.yml", "--project-name", "p", "up"}, []string{"-f", "a.yml", "--project-name", "p"}},
		{"--file=value form", []string{"--file=foo.yml", "up"}, []string{"--file=foo.yml"}},
		{"boolean global", []string{"--dry-run", "up"}, []string{"--dry-run"}},
		{"stops at subcommand", []string{"-f", "a.yml", "up", "-f", "ignored.yml"}, []string{"-f", "a.yml"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := globalFlags(tc.args)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComputeCapDelta(t *testing.T) {
	tests := []struct {
		name   string
		capAdd []string
		want   []string
	}{
		{"no cap_add", nil, []string{"NET_ADMIN"}},
		{"unrelated cap", []string{"SYS_PTRACE"}, []string{"NET_ADMIN"}},
		{"short form present", []string{"NET_ADMIN"}, nil},
		{"prefixed form present", []string{"CAP_NET_ADMIN"}, nil},
		{"lowercase prefixed", []string{"cap_net_admin"}, nil},
		{"ALL catch-all", []string{"ALL"}, nil},
		{"lowercase all", []string{"all"}, nil},
		{"mixed prefixed + unrelated", []string{"CAP_NET_ADMIN", "SYS_PTRACE"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := composetypes.ServiceConfig{CapAdd: tc.capAdd}
			got := computeCapDelta(svc)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractVakaFlagsBool(t *testing.T) {
	// --vaka-init-present is a boolean flag: no value token consumed.
	flags, rest := extractVakaFlags([]string{"up", "--vaka-init-present", "--remove-orphans"})
	if flags["--vaka-init-present"] != "true" {
		t.Errorf("expected --vaka-init-present=true, got %q", flags["--vaka-init-present"])
	}
	want := []string{"up", "--remove-orphans"}
	if strings.Join(rest, " ") != strings.Join(want, " ") {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}
