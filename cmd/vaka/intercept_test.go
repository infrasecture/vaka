// cmd/vaka/intercept_test.go
package main

import (
	"context"
	"os"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"vaka.dev/vaka/pkg/policy"
)

func TestClassifyComposeVerb(t *testing.T) {
	tests := []struct {
		verb string
		want composeVerbClass
	}{
		{"up", verbRender},
		{"run", verbRender},
		{"create", verbRender},
		{"scale", verbRender},
		{"watch", verbRender},
		{"volumes", verbReference},
		{"down", verbReference},
		{"stop", verbReference},
		{"kill", verbReference},
		{"rm", verbReference},
		{"logs", verbReference},
		{"ps", verbReference},
		{"exec", verbExec},
		{"pull", verbReference},
		{"version", verbMetadata},
		{"ls", verbMetadata},
		{"bridge", verbMetadata},
		{"future-container-command", verbUnknown},
	}
	for _, tc := range tests {
		if got := classifyComposeVerb(tc.verb); got != tc.want {
			t.Errorf("classifyComposeVerb(%q) = %v, want %v", tc.verb, got, tc.want)
		}
	}
}

func TestReferenceOverrideYAMLInjectsMetadataOnly(t *testing.T) {
	yaml, err := referenceOverrideYAML()
	if err != nil {
		t.Fatalf("reference override: unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "x-vaka:") || !strings.Contains(yaml, runtimeBundleVersion) {
		t.Errorf("reference override missing runtime metadata:\n%s", yaml)
	}
	if strings.Contains(yaml, "__vaka-init") || strings.Contains(yaml, "services:") {
		t.Errorf("reference override must not define helper/services:\n%s", yaml)
	}
}

func TestExecDockerComposeReferenceRequiresComposeConfig(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	oldComposeFile, hadComposeFile := os.LookupEnv("COMPOSE_FILE")
	_ = os.Unsetenv("COMPOSE_FILE")
	t.Cleanup(func() {
		if hadComposeFile {
			_ = os.Setenv("COMPOSE_FILE", oldComposeFile)
			return
		}
		_ = os.Unsetenv("COMPOSE_FILE")
	})

	inv, err := ParseComposeInvocation([]string{"down"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	err = execDockerCompose(inv, "services: {}\n", nil)
	if err == nil {
		t.Fatal("expected error when compose config is missing")
	}
	if !strings.Contains(err.Error(), "reference command requires compose configuration") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// fakeDS is a minimal DockerServices used to drive servicesNeedingPrebuild.
type fakeDS struct {
	exists map[string]bool // ref -> present locally
}

func (f *fakeDS) CheckRuntimeCompatibility(context.Context) error { return nil }
func (f *fakeDS) ResolveRuntimeImage(context.Context, string, string, bool) (ResolvedImage, error) {
	return ResolvedImage{ID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
}
func (f *fakeDS) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.exists[ref], nil
}
func (f *fakeDS) ResolveRuntime(context.Context, string, composetypes.ServiceConfig) (ResolvedRuntime, error) {
	return ResolvedRuntime{}, nil
}

func TestServicesNeedingPrebuild(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{
		"needsbuild":  {},
		"needsuser":   {},
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
				User:       "1000:1000",
			},
			// Has build + image already local → no pre-build needed.
			"prebuilt": {
				Image: "prebuilt:latest",
				Build: &composetypes.BuildConfig{Context: "."},
			},
			// Entrypoint is compose-declared, but user fallback still needs image inspect.
			"needsuser": {
				Image:      "needsuser:latest",
				Build:      &composetypes.BuildConfig{Context: "."},
				Entrypoint: []string{"/bin/run"},
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
		"prebuilt:latest":  true,
		"myapp:latest":     false,
		"needsuser:latest": false,
		"external:latest":  false,
	}}

	got, err := servicesNeedingPrebuild(context.Background(), ds, policySvcs, project, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"buildonly", "hasentry", "needsbuild", "needsuser"}
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
				User:       "1000:1000",
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
	want := []string{"buildonly", "hasentry", "prebuilt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestComposeImageRefreshOptionsStopAtRunService(t *testing.T) {
	inv, err := ParseComposeInvocation([]string{"run", "app", "sh", "--pull=always", "--build"})
	if err != nil {
		t.Fatal(err)
	}
	if composePullAlwaysRequested(inv) || composeBuildRequested(inv) {
		t.Fatal("run command payload was interpreted as Compose image options")
	}
	original := append([]string{}, inv.Args...)
	if err := consumeComposeImageRefreshOptions(inv, true, true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(inv.Args, "\x00") != strings.Join(original, "\x00") {
		t.Fatalf("run payload changed: got %v want %v", inv.Args, original)
	}
}

func TestComposeBuildTrueIsConsumedBeforeExactImageExecution(t *testing.T) {
	inv, err := ParseComposeInvocation([]string{"up", "--build=true", "-d"})
	if err != nil {
		t.Fatal(err)
	}
	if !composeBuildRequested(inv) {
		t.Fatal("--build=true was not detected")
	}
	if err := consumeComposeImageRefreshOptions(inv, true, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(inv.Args, " "), "--build") {
		t.Fatalf("consumed build option remains: %v", inv.Args)
	}
}

func TestParseComposeInvocationComposeGlobals(t *testing.T) {
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
			inv, err := ParseComposeInvocation(tc.args)
			if err != nil {
				t.Fatalf("ParseComposeInvocation: %v", err)
			}
			got := inv.ComposeGlobals
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestComputeCapabilityPlan(t *testing.T) {
	tests := []struct {
		name     string
		capAdd   []string
		capDrop  []string
		user     string
		runtime  *policy.RuntimeConfig
		wantAdd  []string
		wantDrop []string
	}{
		{name: "default root", wantAdd: []string{"NET_ADMIN"}, wantDrop: []string{"NET_ADMIN"}},
		{name: "setpcap explicitly dropped", capDrop: []string{"SETPCAP"}, wantAdd: []string{"NET_ADMIN", "SETPCAP"}, wantDrop: []string{"NET_ADMIN", "SETPCAP"}},
		{name: "all dropped for nonroot", capDrop: []string{"ALL"}, user: "1000:1000", wantAdd: []string{"NET_ADMIN", "SETUID", "SETGID", "SETPCAP"}, wantDrop: []string{"NET_ADMIN", "SETUID", "SETGID", "SETPCAP"}},
		{name: "root with nonroot group", capDrop: []string{"ALL"}, user: "0:1000", wantAdd: []string{"NET_ADMIN", "SETGID", "SETPCAP"}, wantDrop: []string{"NET_ADMIN", "SETGID", "SETPCAP"}},
		{name: "runtime drops are unioned", runtime: &policy.RuntimeConfig{DropCaps: []string{"NET_RAW"}}, wantAdd: []string{"NET_ADMIN"}, wantDrop: []string{"NET_RAW", "NET_ADMIN"}},
		{name: "chown gets missing setup caps", capDrop: []string{"ALL"}, runtime: &policy.RuntimeConfig{Chown: []policy.ChownAction{{Path: "/data"}}}, wantAdd: []string{"NET_ADMIN", "CHOWN", "DAC_OVERRIDE", "SETPCAP"}, wantDrop: []string{"NET_ADMIN", "CHOWN", "DAC_OVERRIDE", "SETPCAP"}},
		{name: "user supplied net admin remains intentional", capAdd: []string{"CAP_NET_ADMIN"}},
		{name: "cap add wins over cap drop", capAdd: []string{"NET_ADMIN", "SETPCAP"}, capDrop: []string{"ALL"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := composetypes.ServiceConfig{CapAdd: tc.capAdd, CapDrop: tc.capDrop}
			got := computeCapabilityPlan(svc, tc.runtime, tc.user)
			if strings.Join(got.Add, ",") != strings.Join(tc.wantAdd, ",") {
				t.Errorf("add = %v, want %v", got.Add, tc.wantAdd)
			}
			if strings.Join(got.Drop, ",") != strings.Join(tc.wantDrop, ",") {
				t.Errorf("drop = %v, want %v", got.Drop, tc.wantDrop)
			}
		})
	}
}

func TestParseRootArgsVakaInitPresentBool(t *testing.T) {
	// --vaka-init-present is a boolean flag and must appear before subcommand.
	root, err := parseRootArgs([]string{"--vaka-init-present", "up", "--remove-orphans"})
	if err != nil {
		t.Fatalf("parseRootArgs: %v", err)
	}
	if !root.VakaInitPresent {
		t.Error("expected VakaInitPresent=true")
	}
	want := []string{"up", "--remove-orphans"}
	if strings.Join(root.Rest, " ") != strings.Join(want, " ") {
		t.Errorf("rest args = %v, want %v", root.Rest, want)
	}
}
