package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

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
	if len(extraEnv) != 1 {
		t.Fatalf("extraEnv = %v, want one policy payload", extraEnv)
	}
	encoded := strings.TrimPrefix(extraEnv[0], policyPayloadEnvironmentName("app")+"=")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	policyYAML := string(raw)
	if !strings.Contains(policyYAML, "generatedBy: vaka/"+version) {
		t.Errorf("generated policy missing CLI diagnostic:\n%s", policyYAML)
	}
	if !strings.Contains(policyYAML, "requiredRuntimeVersion: "+runtimeBundleVersion) {
		t.Errorf("generated policy missing runtime requirement:\n%s", policyYAML)
	}
	if !strings.Contains(override, "agent.vaka.policy-revision:") {
		t.Errorf("override missing policy revision label:\n%s", override)
	}
}

func TestPolicyPayloadEnvironmentNamesDoNotCollide(t *testing.T) {
	left := policyPayloadEnvironmentName("foo-bar")
	right := policyPayloadEnvironmentName("foo_bar")
	if left == right {
		t.Fatalf("policy environment names collide: %q", left)
	}
	if left != "VAKA_SERVICE_666F6F2D626172_CONF" || right != "VAKA_SERVICE_666F6F5F626172_CONF" {
		t.Fatalf("unexpected policy environment names: %q, %q", left, right)
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
