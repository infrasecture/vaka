package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/internal/runtimebundle"
)

func decodePolicyPayloadFromOverride(t *testing.T, override, service string) string {
	t.Helper()
	var document struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(override), &document); err != nil {
		t.Fatal(err)
	}
	encoded := document.Services[service].Environment[runtimebundle.PolicyEnvironment]
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode policy for %s: %v", service, err)
	}
	return string(raw)
}

func TestBuildInjectionOverrideResolvesAndMountsRuntimeImage(t *testing.T) {
	tests := []struct {
		name             string
		optOut           bool
		vakaInitPresent  bool
		wantEnsureCalls  int
		wantRuntimeMount bool
		wantErr          string
	}{
		{
			name:             "injection enabled and service not opted out",
			optOut:           false,
			vakaInitPresent:  false,
			wantEnsureCalls:  1,
			wantRuntimeMount: true,
		},
		{
			name:            "baked helper label is rejected",
			optOut:          true,
			vakaInitPresent: false,
			wantErr:         "verified read-only runtime mount",
		},
		{
			name:            "baked helper flag is rejected",
			optOut:          false,
			vakaInitPresent: true,
			wantErr:         "verified read-only runtime mount",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)

			policyYAML := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`

			labelBlock := ""
			if tc.optOut {
				labelBlock = `
    labels:
      agent.vaka.init: present`
			}

			composeYAML := `
services:
  app:
    image: alpine:3.20
    user: "1000:1000"
    entrypoint: ["sleep"]
    command: ["infinity"]` + labelBlock + `
`
			writeFixtureFiles(t, dir, policyYAML, composeYAML)

			ds := &fakeBuilderDockerServices{}
			inv, err := ParseComposeInvocation([]string{"show-compose"})
			if err != nil {
				t.Fatalf("ParseComposeInvocation: %v", err)
			}
			gotYAML, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, tc.vakaInitPresent)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("buildInjectionOverride error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildInjectionOverride: %v", err)
			}

			if len(ds.ensureRefs) != tc.wantEnsureCalls {
				t.Fatalf("ensure calls = %d, want %d", len(ds.ensureRefs), tc.wantEnsureCalls)
			}

			hasRuntimeMount := strings.Contains(gotYAML, "type: image") &&
				strings.Contains(gotYAML, "source: sha256:aaaaaaaa")
			if hasRuntimeMount != tc.wantRuntimeMount {
				t.Fatalf("has runtime image mount = %v, want %v\nYAML:\n%s", hasRuntimeMount, tc.wantRuntimeMount, gotYAML)
			}
			if strings.Contains(gotYAML, "__vaka-init") || strings.Contains(gotYAML, "volumes_from") {
				t.Fatalf("legacy helper service reference in override:\n%s", gotYAML)
			}
		})
	}
}

func TestBuildInjectionOverridePreservesExplicitInit(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
    init: true
`)
	inv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatal(err)
	}
	override, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(override, "init: true") {
		t.Fatalf("explicit init was not preserved:\n%s", override)
	}
}

func TestBuildInjectionOverrideSeparatesGeneratorAndRuntimeIdentity(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
    user: "1000:1000"
    group_add: [2000, shared]
    entrypoint: ["sleep"]
    command: ["infinity"]
`)

	ds := &fakeBuilderDockerServices{}
	inv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatal(err)
	}
	override, extraEnv, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(extraEnv) != 0 {
		t.Fatalf("policy payload leaked into host environment: %v", extraEnv)
	}
	policyYAML := decodePolicyPayloadFromOverride(t, override, "app")
	if !strings.Contains(policyYAML, "generatedBy: vaka/"+version) {
		t.Errorf("generated policy missing CLI diagnostic:\n%s", policyYAML)
	}
	if !strings.Contains(policyYAML, "requiredRuntimeVersion: "+runtimeBundleVersion) {
		t.Errorf("generated policy missing runtime requirement:\n%s", policyYAML)
	}
	if !strings.Contains(policyYAML, "groupAdd:") || !strings.Contains(policyYAML, "- \"2000\"") || !strings.Contains(policyYAML, "- shared") {
		t.Errorf("generated policy missing Compose group_add:\n%s", policyYAML)
	}
	if !strings.Contains(override, "agent.vaka.policy-revision:") {
		t.Errorf("override missing policy revision label:\n%s", override)
	}
}

func TestBuildInjectionPreservesIntentionallyBroadCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name       string
		composeCap string
	}{
		{name: "privileged", composeCap: "    privileged: true\n"},
		{name: "cap add all", composeCap: "    cap_add: [ALL]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)
			writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
`+tc.composeCap)

			inv, _ := ParseComposeInvocation([]string{"up", "app"})
			override, extraEnv, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(override, "cap_add:") {
				t.Fatalf("Vaka added temporary capabilities to intentionally broad service:\n%s", override)
			}
			if len(extraEnv) != 0 {
				t.Fatalf("policy payload leaked into host environment: %v", extraEnv)
			}
			raw := decodePolicyPayloadFromOverride(t, override, "app")
			if strings.Contains(raw, "dropCaps") || strings.Contains(raw, "NET_ADMIN") {
				t.Fatalf("generated policy removed an intentional capability:\n%s", raw)
			}
		})
	}
}

func TestBuildInjectionRejectsIneffectiveTemporaryCapabilityAdd(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
    cap_add: [ALL]
    cap_drop: [NET_ADMIN]
`)
	inv, _ := ParseComposeInvocation([]string{"up", "app"})
	_, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err == nil || !strings.Contains(err.Error(), "runtime.dropCaps") {
		t.Fatalf("ineffective capability error = %v", err)
	}
}

func TestOversizedPolicyRejectedBeforeImagePreparation(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	policyYAML := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
        accept:
` + strings.Repeat("          - to: [10.0.0.1/32]\n", 5000)
	writeFixtureFiles(t, dir, policyYAML, `
services:
  app:
    image: oversized-policy:test
    build: .
`)

	ds := &fakeBuilderDockerServices{}
	var composeCalls int
	setExecDockerComposeForTest(t, func(_ *ComposeInvocation, _ string, _ []string) error {
		composeCalls++
		return nil
	})
	inv, _ := ParseComposeInvocation([]string{"show-compose", "--build"})
	_, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, false)
	if err == nil || !strings.Contains(err.Error(), "maximum supported size") {
		t.Fatalf("oversized policy error = %v", err)
	}
	if composeCalls != 0 || len(ds.ensureRefs) != 0 {
		t.Fatalf("oversized policy reached image preparation: composeCalls=%d runtime=%v", composeCalls, ds.ensureRefs)
	}
}

func TestPolicyPayloadSizeBoundary(t *testing.T) {
	if err := validatePolicyPayloadSize("app", strings.Repeat("a", maxEncodedPolicyPayloadBytes)); err != nil {
		t.Fatalf("boundary payload rejected: %v", err)
	}
	if err := validatePolicyPayloadSize("app", strings.Repeat("a", maxEncodedPolicyPayloadBytes+1)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestBakedHelperLabelRejectedBeforeImagePreparation(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
    pull_policy: always
    labels:
      agent.vaka.init: present
`)

	var composeCalls int
	setExecDockerComposeForTest(t, func(*ComposeInvocation, string, []string) error {
		composeCalls++
		return nil
	})
	inv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err == nil || !strings.Contains(err.Error(), "verified read-only runtime mount") {
		t.Fatalf("error = %v, want baked-helper rejection", err)
	}
	if composeCalls != 0 {
		t.Fatalf("compose calls = %d, want zero before rejection", composeCalls)
	}
}

func TestBuildInjectionOverridePrebuildOnBuildFlag(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	policyYAML := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`
	composeYAML := `
services:
  app:
    image: app:latest
    build: .
    user: "1000:1000"
  unmanaged:
    image: unmanaged:latest
    build: .
`
	writeFixtureFiles(t, dir, policyYAML, composeYAML)

	ds := &fakeBuilderDockerServices{
		imageExists: map[string]bool{"app:latest": true},
		runtimes: map[string]ResolvedRuntime{
			"app": {Entrypoint: []string{"/bin/app"}},
		},
	}

	var prebuildCalls int
	var prebuildArgs []string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, overrideYAML string, extraEnv []string) error {
		if overrideYAML == "" && len(inv.Args) > 0 && inv.Args[0] == "build" {
			prebuildCalls++
			prebuildArgs = append([]string{}, inv.Args...)
		}
		return nil
	})

	inv, err := ParseComposeInvocation([]string{"show-compose", "--build"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	_, _, err = buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, false)
	if err != nil {
		t.Fatalf("buildInjectionOverride: %v", err)
	}

	if prebuildCalls != 1 {
		t.Fatalf("prebuild calls = %d, want 1", prebuildCalls)
	}
	if !strings.Contains(strings.Join(prebuildArgs, " "), "app") {
		t.Fatalf("prebuild args %v do not include service name", prebuildArgs)
	}
	if strings.Contains(strings.Join(prebuildArgs, " "), "unmanaged") {
		t.Fatalf("prebuild args %v include unmanaged service", prebuildArgs)
	}
}

func TestRunNoDepsKeepsImageOptionsNativeForUnmanagedTarget(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
  unmanaged:
    image: unmanaged:latest
    build: .
    depends_on:
      - app
`)

	var prepCalls int
	setExecDockerComposeForTest(t, func(*ComposeInvocation, string, []string) error {
		prepCalls++
		return nil
	})
	inv, _ := ParseComposeInvocation([]string{"run", "--build", "--pull=always", "--no-deps", "unmanaged", "id"})
	original := strings.Join(inv.Args, "\x00")
	if _, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false); err != nil {
		t.Fatal(err)
	}
	if prepCalls != 0 {
		t.Fatalf("preparation calls = %d, want native Compose handling", prepCalls)
	}
	if got := strings.Join(inv.Args, "\x00"); got != original {
		t.Fatalf("unmanaged run args changed: %v", inv.Args)
	}
}

func TestRunManagedTargetPreparesUnmanagedDependencies(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
    depends_on:
      - helper
  helper:
    image: helper:latest
    build: .
`)

	var pulls [][]string
	var builds [][]string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, override string, _ []string) error {
		if override != "" || len(inv.Args) == 0 {
			return nil
		}
		switch inv.Args[0] {
		case "pull":
			pulls = append(pulls, append([]string{}, inv.Args...))
		case "build":
			builds = append(builds, append([]string{}, inv.Args...))
		}
		return nil
	})

	inv, _ := ParseComposeInvocation([]string{"run", "--build", "--pull=always", "app", "id"})
	override, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(flattenArgs(pulls), " "); !strings.Contains(got, "app") || strings.Contains(got, "helper") {
		t.Fatalf("pull calls = %v, want only image-only managed target", pulls)
	}
	if got := strings.Join(flattenArgs(builds), " "); !strings.Contains(got, "helper") {
		t.Fatalf("build calls = %v, want unmanaged dependency", builds)
	}
	if strings.Contains(strings.Join(inv.Args, " "), "--build") || strings.Contains(strings.Join(inv.Args, " "), "--pull") {
		t.Fatalf("consumed run refresh flags remain: %v", inv.Args)
	}
	if !strings.Contains(override, "helper:") || !strings.Contains(override, "pull_policy: never") {
		t.Fatalf("override does not freeze prepared helper:\n%s", override)
	}
}

func TestRunUnmanagedTargetPreparesWhenDependencyIsManaged(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  policy:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
    build: .
    depends_on:
      - policy
  policy:
    image: policy:latest
`)

	var pulls [][]string
	var builds [][]string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, override string, _ []string) error {
		if override != "" || len(inv.Args) == 0 {
			return nil
		}
		switch inv.Args[0] {
		case "pull":
			pulls = append(pulls, append([]string{}, inv.Args...))
		case "build":
			builds = append(builds, append([]string{}, inv.Args...))
		}
		return nil
	})

	inv, _ := ParseComposeInvocation([]string{"run", "--build", "--pull=always", "app", "id"})
	override, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(flattenArgs(pulls), " "); strings.Contains(got, "app") || !strings.Contains(got, "policy") {
		t.Fatalf("pull calls = %v, want only image-only managed dependency", pulls)
	}
	if got := strings.Join(flattenArgs(builds), " "); !strings.Contains(got, "app") {
		t.Fatalf("build calls = %v, want unmanaged target", builds)
	}
	if strings.Contains(strings.Join(inv.Args, " "), "--build") || strings.Contains(strings.Join(inv.Args, " "), "--pull") {
		t.Fatalf("consumed run refresh flags remain: %v", inv.Args)
	}
	if !strings.Contains(override, "app:") || !strings.Contains(override, "pull_policy: never") {
		t.Fatalf("override does not freeze prepared app:\n%s", override)
	}
}

func flattenArgs(calls [][]string) []string {
	var out []string
	for _, call := range calls {
		out = append(out, call...)
	}
	return out
}

func TestUpBuildKeepsNativeBehaviorForUnmanagedSelection(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
  selected:
    image: selected:latest
    build: .
  unrelated:
    image: unrelated:latest
    build: .
`)

	var buildArgs []string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, override string, _ []string) error {
		if override == "" && len(inv.Args) > 0 && inv.Args[0] == "build" {
			buildArgs = append([]string{}, inv.Args...)
		}
		return nil
	})
	inv, _ := ParseComposeInvocation([]string{"up", "--build", "--no-deps", "selected"})
	override, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(buildArgs) != 0 {
		t.Fatalf("Vaka pre-built an entirely unmanaged selection: %v", buildArgs)
	}
	if !strings.Contains(strings.Join(inv.Args, " "), "--build") {
		t.Fatalf("native Compose build flag was consumed: %v", inv.Args)
	}
	if strings.Contains(override, "selected:") {
		t.Fatalf("override unexpectedly freezes an unmanaged service:\n%s", override)
	}
	if strings.Contains(override, "unrelated:") {
		t.Fatalf("override includes unrelated unmanaged service:\n%s", override)
	}
}

func TestBuildInjectionUsesOnlySelectedManagedServices(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
  tool:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
  tool:
    image: alpine:3.20
    profiles: [tools]
`)

	inv, _ := ParseComposeInvocation([]string{"up", "app"})
	override, extraEnv, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(override, "app:") || strings.Contains(override, "tool:") {
		t.Fatalf("selected override contains wrong services:\n%s", override)
	}
	if len(extraEnv) != 0 {
		t.Fatalf("selected policy payload leaked into host environment: %v", extraEnv)
	}
	if decodePolicyPayloadFromOverride(t, override, "app") == "" {
		t.Fatal("selected service has no embedded policy payload")
	}
}

func TestGeneratedPreparationPreservesQuietOptions(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
  image:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
    build: .
  image:
    image: image:latest
`)

	var calls [][]string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, _ string, _ []string) error {
		calls = append(calls, append([]string{}, inv.Args...))
		return nil
	})
	inv, _ := ParseComposeInvocation([]string{
		"up", "--build", "--pull=always", "--quiet-build", "--quiet-pull", "app", "image",
	})
	if _, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false); err != nil {
		t.Fatal(err)
	}

	foundPull, foundBuild := false, false
	for _, args := range calls {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "pull ") {
			foundPull = strings.Contains(joined, "--quiet") && strings.Contains(joined, "image")
		}
		if strings.HasPrefix(joined, "build ") {
			foundBuild = strings.Contains(joined, "--quiet") && strings.Contains(joined, "app")
		}
	}
	if !foundPull || !foundBuild {
		t.Fatalf("quiet preparation calls = %v", calls)
	}
}

func TestRunPreparationDoesNotParseCommandPayloadAsQuietOptions(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
    build: .
`)

	var calls [][]string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, _ string, _ []string) error {
		calls = append(calls, append([]string{}, inv.Args...))
		return nil
	})
	inv, _ := ParseComposeInvocation([]string{
		"run", "--build", "--pull=always", "app", "tool",
		"--quiet-pull=garbage", "--quiet-build=garbage",
	})
	if _, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false); err != nil {
		t.Fatal(err)
	}

	if len(calls) == 0 {
		t.Fatal("run preparation did not execute")
	}
	for _, args := range calls {
		for _, arg := range args {
			if arg == "--quiet" {
				t.Fatalf("run payload enabled quiet preparation: %v", calls)
			}
		}
	}
}

func TestInvalidCapabilityPlanPrecedesImagePreparation(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
    build: .
    pull_policy: always
    cap_add: [ALL]
    cap_drop: [NET_ADMIN]
`)

	var calls [][]string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, _ string, _ []string) error {
		calls = append(calls, append([]string{}, inv.Args...))
		return nil
	})
	ds := &fakeBuilderDockerServices{}
	inv, _ := ParseComposeInvocation([]string{"up", "--build", "--pull=always", "app"})
	_, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, false)
	if err == nil || !strings.Contains(err.Error(), "prevents Vaka from provisioning required temporary capability NET_ADMIN") {
		t.Fatalf("capability-plan error = %v", err)
	}
	if len(calls) != 0 || len(ds.ensureRefs) != 0 {
		t.Fatalf("invalid capability plan prepared images: calls=%v runtime=%v", calls, ds.ensureRefs)
	}
}

func TestUpMissingPullRemainsForUnmanagedComposeBehavior(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
  unmanaged:
    image: unmanaged:latest
`)
	setExecDockerComposeForTest(t, func(*ComposeInvocation, string, []string) error { return nil })
	inv, _ := ParseComposeInvocation([]string{"up", "--pull=missing", "unmanaged"})
	if _, _, err := buildInjectionOverride(context.Background(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(inv.Args, " "), "--pull=missing") {
		t.Fatalf("safe pull policy was stripped: %v", inv.Args)
	}
}

func TestBuildInjectionOverridePrePullsManagedPolicyOnly(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: app:latest
    pull_policy: always
    user: "1000:1000"
    entrypoint: ["sleep"]
  unmanaged:
    image: unmanaged:latest
    pull_policy: always
`)

	ds := &fakeBuilderDockerServices{}
	var pullArgs []string
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, overrideYAML string, extraEnv []string) error {
		if overrideYAML == "" && len(inv.Args) > 0 && inv.Args[0] == "pull" {
			pullArgs = append([]string{}, inv.Args...)
		}
		return nil
	})

	inv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, false); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(pullArgs, " ")
	if !strings.Contains(joined, "--policy always app") {
		t.Fatalf("pre-pull args = %v", pullArgs)
	}
	if strings.Contains(joined, "unmanaged") {
		t.Fatalf("pre-pull args include unmanaged service: %v", pullArgs)
	}
}

func TestBuildInjectionOverrideFallsBackFromPullToManagedBuild(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: local-app:latest
    pull_policy: missing
    build:
      context: .
      dockerfile_inline: |
        FROM scratch
    user: "1000:1000"
    entrypoint: ["sleep"]
`)

	ds := &fakeBuilderDockerServices{imageExists: map[string]bool{"local-app:latest": false}}
	var pullCalls, buildCalls int
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, overrideYAML string, extraEnv []string) error {
		if overrideYAML != "" || len(inv.Args) == 0 {
			return nil
		}
		switch inv.Args[0] {
		case "pull":
			pullCalls++
			return errors.New("image is not available")
		case "build":
			buildCalls++
		}
		return nil
	})

	inv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, false); err != nil {
		t.Fatal(err)
	}
	if pullCalls != 1 || buildCalls != 1 {
		t.Fatalf("pull calls = %d, build calls = %d; want one of each", pullCalls, buildCalls)
	}
}
