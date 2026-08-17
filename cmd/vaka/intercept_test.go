// cmd/vaka/intercept_test.go
package main

import (
	"context"
	"os"
	"reflect"
	"sort"
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

	got, err := servicesNeedingPrebuild(context.Background(), ds, policySvcs, project, false, nil, false)
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
			"unmanaged": {
				Image: "unmanaged:latest",
				Build: &composetypes.BuildConfig{Context: "."},
			},
		},
	}
	ds := &fakeDS{exists: map[string]bool{
		"prebuilt:latest": true,
		"external:latest": false,
	}}

	got, err := servicesNeedingPrebuild(context.Background(), ds, policySvcs, project, true, nil, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"buildonly", "hasentry", "prebuilt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPlanManagedImagePreparationUsesManagedServicesOnly(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{
		"always":       {},
		"build":        {},
		"missing":      {},
		"missingbuild": {},
	}
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"always":       {Image: "always:latest", PullPolicy: composetypes.PullPolicyAlways},
		"build":        {Image: "build:latest", Build: &composetypes.BuildConfig{Context: "."}, PullPolicy: composetypes.PullPolicyBuild},
		"missing":      {Image: "missing:latest", PullPolicy: composetypes.PullPolicyMissing},
		"missingbuild": {Image: "missingbuild:latest", Build: &composetypes.BuildConfig{Context: "."}, PullPolicy: composetypes.PullPolicyMissing},
		"unmanaged":    {Image: "unmanaged:latest", PullPolicy: composetypes.PullPolicyAlways},
	}}
	ds := &fakeDS{exists: map[string]bool{
		"missing:latest":      false,
		"missingbuild:latest": false,
	}}

	got, err := planManagedImagePreparation(context.Background(), ds, policySvcs, project, "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.pullAlways, ",") != "always" {
		t.Errorf("always pulls = %v", got.pullAlways)
	}
	if strings.Join(got.pullMissing, ",") != "missing" {
		t.Errorf("missing pulls = %v", got.pullMissing)
	}
	if len(got.pullOrBuild) != 1 || got.pullOrBuild[0] != (managedPullOrBuild{service: "missingbuild", policy: "missing"}) {
		t.Errorf("pull/build fallbacks = %v", got.pullOrBuild)
	}
	if !got.forceBuild["build"] || got.forceBuild["missingbuild"] || got.forceBuild["unmanaged"] {
		t.Errorf("forced builds = %v", got.forceBuild)
	}
}

func TestPlanManagedImagePreparationCLIOverridesFilePolicy(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{"app": {}}
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"app": {Image: "app:latest", PullPolicy: composetypes.PullPolicyAlways},
	}}
	ds := &fakeDS{exists: map[string]bool{"app:latest": true}}

	never, err := planManagedImagePreparation(context.Background(), ds, policySvcs, project, "never", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(never.pullAlways) != 0 || len(never.pullMissing) != 0 || len(never.pullOrBuild) != 0 {
		t.Fatalf("--pull=never plan = %+v", never)
	}

	always, err := planManagedImagePreparation(context.Background(), ds, policySvcs, project, "always", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(always.pullAlways, ",") != "app" {
		t.Fatalf("--pull=always plan = %+v", always)
	}

	imageOnly, err := planManagedImagePreparation(context.Background(), ds, policySvcs, project, "build", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if imageOnly.forceBuild["app"] || imageOnly.effectivePullPolicy["app"] != composetypes.PullPolicyBuild {
		t.Fatalf("image-only --pull=build plan = %+v", imageOnly)
	}

	buildSvc := project.Services["app"]
	buildSvc.Build = &composetypes.BuildConfig{Context: "."}
	project.Services["app"] = buildSvc
	build, err := planManagedImagePreparation(context.Background(), ds, policySvcs, project, "build", true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !build.forceBuild["app"] {
		t.Fatalf("--pull=build plan = %+v", build)
	}
}

func TestBuildTakesPrecedenceOverPullForBuildableServices(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{"buildable": {}, "image-only": {}}
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"buildable":  {Name: "buildable", Image: "buildable:latest", Build: &composetypes.BuildConfig{Context: "."}},
		"image-only": {Name: "image-only", Image: "image-only:latest"},
	}}
	managed, err := planManagedImagePreparation(
		context.Background(), &fakeDS{}, policySvcs, project,
		composetypes.PullPolicyAlways, true, true, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !managed.forceBuild["buildable"] || managed.effectivePullPolicy["buildable"] != composetypes.PullPolicyBuild {
		t.Fatalf("buildable service did not use build precedence: %+v", managed)
	}
	if !reflect.DeepEqual(managed.pullAlways, []string{"image-only"}) || len(managed.pullOrBuild) != 0 {
		t.Fatalf("managed pull plan = %+v, want only image-only pull", managed)
	}

	unmanaged, err := planSelectedUnmanagedRefresh(
		project, map[string]*policy.ServiceConfig{},
		composetypes.PullPolicyAlways, true, true, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !unmanaged.forceBuild["buildable"] || !reflect.DeepEqual(unmanaged.pullAlways, []string{"image-only"}) || len(unmanaged.pullOrBuild) != 0 {
		t.Fatalf("unmanaged refresh plan did not preserve build precedence: %+v", unmanaged)
	}
}

func TestImageUsesLatestTag(t *testing.T) {
	tests := map[string]bool{
		"alpine":                      true,
		"alpine:latest":               true,
		"registry.example/app:latest": true,
		"registry.example/app:stable": false,
		"alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": false,
	}
	for image, want := range tests {
		if got := imageUsesLatestTag(image); got != want {
			t.Errorf("imageUsesLatestTag(%q) = %t, want %t", image, got, want)
		}
	}
}

func TestDefaultComposeImageNameHonorsCompatibilityMode(t *testing.T) {
	project := &composetypes.Project{Name: "demo", Environment: map[string]string{}, Services: composetypes.Services{
		"app": {Name: "app", Build: &composetypes.BuildConfig{Context: "."}},
	}}
	inv, _ := ParseComposeInvocation([]string{"up"})
	applyDefaultImageNames(project, inv)
	if got := project.Services["app"].Image; got != "demo-app" {
		t.Fatalf("default image = %q, want demo-app", got)
	}

	project.Services["app"] = composetypes.ServiceConfig{Name: "app", Build: &composetypes.BuildConfig{Context: "."}}
	project.Environment["COMPOSE_COMPATIBILITY"] = "y"
	applyDefaultImageNames(project, inv)
	if got := project.Services["app"].Image; got != "demo_app" {
		t.Fatalf("compatible image = %q, want demo_app", got)
	}
}

func TestPlanManagedImagePreparationRejectsUnsupportedOrConflictingPolicy(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{"app": {}}
	ds := &fakeDS{exists: map[string]bool{}}
	tests := []struct {
		name      string
		svc       composetypes.ServiceConfig
		cliPull   string
		cliSet    bool
		noBuild   bool
		wantError string
	}{
		{name: "timed", svc: composetypes.ServiceConfig{Image: "app:latest", PullPolicy: "daily"}, wantError: "cannot apply"},
		{name: "bad CLI pull", svc: composetypes.ServiceConfig{Image: "app:latest"}, cliPull: "sometimes", cliSet: true, wantError: "unsupported Compose --pull"},
		{name: "missing image with build disabled", svc: composetypes.ServiceConfig{Image: "app:latest", Build: &composetypes.BuildConfig{Context: "."}, PullPolicy: composetypes.PullPolicyBuild}, noBuild: true, wantError: "requires an image build"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{"app": tc.svc}}
			_, err := planManagedImagePreparation(context.Background(), ds, policySvcs, project, tc.cliPull, tc.cliSet, false, tc.noBuild)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestNoBuildUsesExistingImageForBuildPullPolicy(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{"app": {}}
	project := &composetypes.Project{Name: "demo", Services: map[string]composetypes.ServiceConfig{
		"app": {Image: "app:latest", Build: &composetypes.BuildConfig{Context: "."}, PullPolicy: composetypes.PullPolicyBuild},
	}}
	ds := &fakeDS{exists: map[string]bool{"app:latest": true}}
	plan, err := planManagedImagePreparation(context.Background(), ds, policySvcs, project, "", false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.forceBuild["app"] {
		t.Fatalf("--no-build unexpectedly scheduled a build: %+v", plan)
	}

	unmanaged, err := planSelectedUnmanagedRefresh(project, map[string]*policy.ServiceConfig{}, composetypes.PullPolicyBuild, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if unmanaged.forceBuild["app"] || !reflect.DeepEqual(unmanaged.prepared, []string{"app"}) {
		t.Fatalf("unmanaged --no-build plan = %+v", unmanaged)
	}
}

func TestPlanManagedImagePreparationCarriesEffectivePullPolicy(t *testing.T) {
	policySvcs := map[string]*policy.ServiceConfig{"app": {}}
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"app": {Image: "app:latest", PullPolicy: composetypes.PullPolicyNever},
	}}
	plan, err := planManagedImagePreparation(context.Background(), &fakeDS{}, policySvcs, project, "", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.effectivePullPolicy["app"]; got != composetypes.PullPolicyNever {
		t.Fatalf("effective pull policy = %q, want never", got)
	}

	plan, err = planManagedImagePreparation(context.Background(), &fakeDS{}, policySvcs, project, composetypes.PullPolicyMissing, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.effectivePullPolicy["app"]; got != composetypes.PullPolicyMissing {
		t.Fatalf("CLI effective pull policy = %q, want missing", got)
	}
}

func TestScanCreateServiceTargetsAndDependencySelection(t *testing.T) {
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"app": {Name: "app", DependsOn: map[string]composetypes.ServiceDependency{"db": {Condition: "service_started"}}},
		"db":  {Name: "db"},
		"old": {Name: "old"},
	}}
	tests := []struct {
		args []string
		want []string
	}{
		{args: []string{"up", "--build", "--pull=always", "app"}, want: []string{"app", "db"}},
		{args: []string{"up", "--build", "-t0", "app"}, want: []string{"app", "db"}},
		{args: []string{"up", "--no-deps=false", "--no-deps", "app", "--build"}, want: []string{"app"}},
		{args: []string{"create", "--scale", "app=2", "app"}, want: []string{"app", "db"}},
	}
	for _, tc := range tests {
		inv, _ := ParseComposeInvocation(tc.args)
		targets, err := scanCreateServiceTargets(inv)
		if err != nil {
			t.Fatalf("scan %v: %v", tc.args, err)
		}
		selected, err := selectComposeServices(project, targets, inv.PostSubcommand)
		if err != nil {
			t.Fatalf("select %v: %v", tc.args, err)
		}
		var got []string
		for name := range selected.Services {
			got = append(got, name)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("selected %v = %v, want %v", tc.args, got, tc.want)
		}
	}

	inv, _ := ParseComposeInvocation([]string{"create", "--build", "--wait", "app"})
	if _, err := scanCreateServiceTargets(inv); err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("unknown create option error = %v", err)
	}
	for _, args := range [][]string{{"up", "--build", "-tbad", "app"}, {"up", "--build", "--timeout=bad", "app"}} {
		inv, _ := ParseComposeInvocation(args)
		if _, err := scanCreateServiceTargets(inv); err == nil || !strings.Contains(err.Error(), "requires an integer") {
			t.Errorf("invalid timeout %v error = %v", args, err)
		}
	}
}

func TestPullBuildFreezesSelectedUnmanagedImageOnlyService(t *testing.T) {
	project := &composetypes.Project{Services: composetypes.Services{
		"app": {Name: "app", Image: "app:latest", DependsOn: map[string]composetypes.ServiceDependency{
			"policy": {Condition: "service_started"},
		}},
		"policy": {Name: "policy", Image: "policy:latest"},
	}}
	plan, err := planSelectedUnmanagedRefresh(
		project,
		map[string]*policy.ServiceConfig{"policy": {}},
		composetypes.PullPolicyBuild,
		true,
		false,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.prepared, []string{"app"}) || len(plan.forceBuild) != 0 {
		t.Fatalf("image-only --pull=build plan = %+v", plan)
	}
}

func TestComposeImageRefreshOptionsStopAtRunService(t *testing.T) {
	inv, err := ParseComposeInvocation([]string{"run", "app", "sh", "--pull=ALWAYS", "--build=garbage"})
	if err != nil {
		t.Fatal(err)
	}
	buildRequested, err := composeBuildRequested(inv)
	if err != nil {
		t.Fatal(err)
	}
	if composePullAlwaysRequested(inv) || buildRequested {
		t.Fatal("run command payload was interpreted as Compose image options")
	}
	if err := validateConsumedComposeBooleans(inv, composeCommandSpecFor("run")); err != nil {
		t.Fatalf("run command payload was rejected as a Compose option: %v", err)
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
	buildRequested, err := composeBuildRequested(inv)
	if err != nil {
		t.Fatal(err)
	}
	if !buildRequested {
		t.Fatal("--build=true was not detected")
	}
	if err := consumeComposeImageRefreshOptions(inv, true, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(inv.Args, " "), "--build") {
		t.Fatalf("consumed build option remains: %v", inv.Args)
	}
}

func TestComposeImageRefreshOptionsUseLastValue(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantBuild bool
		wantPull  bool
	}{
		{name: "build false then true", args: []string{"up", "--build=false", "--build"}, wantBuild: true},
		{name: "build true then false", args: []string{"up", "--build", "--build=0"}},
		{name: "pull never then always", args: []string{"up", "--pull=never", "--pull", "always"}, wantPull: true},
		{name: "pull always then never", args: []string{"up", "--pull=always", "--pull", "never"}},
		{name: "run build false then true", args: []string{"run", "--build=false", "--build", "app"}, wantBuild: true},
		{name: "run build true then false", args: []string{"run", "--build", "--build=0", "app"}},
		{name: "run pull always then never", args: []string{"run", "--pull=always", "--pull", "never", "app"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := ParseComposeInvocation(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			got, err := composeBuildRequested(inv)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.wantBuild {
				t.Errorf("composeBuildRequested(%v) = %t, want %t", tc.args, got, tc.wantBuild)
			}
			if got := composePullAlwaysRequested(inv); got != tc.wantPull {
				t.Errorf("composePullAlwaysRequested(%v) = %t, want %t", tc.args, got, tc.wantPull)
			}
		})
	}
}

func TestConsumeComposePullRemovesEarlierValues(t *testing.T) {
	inv, err := ParseComposeInvocation([]string{"up", "--pull=never", "--pull", "always", "app"})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumeComposeImageRefreshOptions(inv, false, true); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(inv.Args, " "), "up app"; got != want {
		t.Fatalf("rewritten invocation = %q, want %q", got, want)
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
		name       string
		capAdd     []string
		capDrop    []string
		privileged bool
		runtime    *policy.RuntimeConfig
		wantAdd    []string
		wantDrop   []string
		wantErr    string
	}{
		{name: "default root", wantAdd: []string{"NET_ADMIN"}, wantDrop: []string{"NET_ADMIN"}},
		{name: "setpcap explicitly dropped", capDrop: []string{"SETPCAP"}, wantAdd: []string{"NET_ADMIN", "SETPCAP"}, wantDrop: []string{"NET_ADMIN", "SETPCAP"}},
		{name: "all dropped provisions exec identity caps", capDrop: []string{"ALL"}, wantAdd: []string{"NET_ADMIN", "SETUID", "SETGID", "SETPCAP"}, wantDrop: []string{"NET_ADMIN", "SETUID", "SETGID", "SETPCAP"}},
		{name: "root stores exec identity caps", capDrop: []string{"SETUID", "SETGID"}, wantAdd: []string{"NET_ADMIN", "SETUID", "SETGID"}, wantDrop: []string{"NET_ADMIN", "SETUID", "SETGID"}},
		{name: "runtime drops are unioned", runtime: &policy.RuntimeConfig{DropCaps: []string{"NET_RAW"}}, wantAdd: []string{"NET_ADMIN"}, wantDrop: []string{"NET_RAW", "NET_ADMIN"}},
		{name: "chown gets missing setup caps", capDrop: []string{"ALL"}, runtime: &policy.RuntimeConfig{Chown: []policy.ChownAction{{Path: "/data"}}}, wantAdd: []string{"NET_ADMIN", "SETUID", "SETGID", "CHOWN", "DAC_OVERRIDE", "SETPCAP"}, wantDrop: []string{"NET_ADMIN", "SETUID", "SETGID", "CHOWN", "DAC_OVERRIDE", "SETPCAP"}},
		{name: "user supplied net admin remains intentional", capAdd: []string{"CAP_NET_ADMIN"}},
		{name: "cap add wins over cap drop", capAdd: []string{"NET_ADMIN", "SETPCAP"}, capDrop: []string{"ALL"}, wantAdd: []string{"SETUID", "SETGID"}, wantDrop: []string{"SETUID", "SETGID"}},
		{name: "privileged preserves all capabilities", privileged: true, capDrop: []string{"ALL"}, wantAdd: nil, wantDrop: nil},
		{name: "privileged honors explicit runtime removal", privileged: true, runtime: &policy.RuntimeConfig{DropCaps: []string{"NET_ADMIN"}}, wantDrop: []string{"NET_ADMIN"}},
		{name: "all capabilities preserve intentional net admin", capAdd: []string{"ALL"}},
		{name: "all with required capability specifically dropped", capAdd: []string{"ALL"}, capDrop: []string{"NET_ADMIN"}, wantErr: "runtime.dropCaps"},
		{name: "all with setpcap dropped and runtime removal", capAdd: []string{"ALL"}, capDrop: []string{"SETPCAP"}, runtime: &policy.RuntimeConfig{DropCaps: []string{"NET_RAW"}}, wantErr: "SETPCAP"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := composetypes.ServiceConfig{CapAdd: tc.capAdd, CapDrop: tc.capDrop, Privileged: tc.privileged}
			got, err := computeCapabilityPlan(svc, tc.runtime)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
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
