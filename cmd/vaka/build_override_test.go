package main

import (
	"context"
	"strings"
	"testing"
)

func TestBuildInjectionOverrideEnsureImageAndInitService(t *testing.T) {
	tests := []struct {
		name             string
		optOut           bool
		vakaInitPresent  bool
		wantEnsureCalls  int
		wantInitInOutput bool
	}{
		{
			name:             "injection enabled and service not opted out",
			optOut:           false,
			vakaInitPresent:  false,
			wantEnsureCalls:  1,
			wantInitInOutput: true,
		},
		{
			name:             "all services opted out with vaka-init absent",
			optOut:           true,
			vakaInitPresent:  false,
			wantEnsureCalls:  0,
			wantInitInOutput: false,
		},
		{
			name:             "vaka-init present skips image ensure",
			optOut:           false,
			vakaInitPresent:  true,
			wantEnsureCalls:  0,
			wantInitInOutput: false,
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
			inv, err := ParseInvocation([]string{"show-compose"})
			if err != nil {
				t.Fatalf("ParseInvocation: %v", err)
			}
			gotYAML, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, tc.vakaInitPresent)
			if err != nil {
				t.Fatalf("buildInjectionOverride: %v", err)
			}

			if len(ds.ensureRefs) != tc.wantEnsureCalls {
				t.Fatalf("ensure calls = %d, want %d", len(ds.ensureRefs), tc.wantEnsureCalls)
			}

			hasInit := strings.Contains(gotYAML, "__vaka-init:")
			if hasInit != tc.wantInitInOutput {
				t.Fatalf("has __vaka-init service = %v, want %v\nYAML:\n%s", hasInit, tc.wantInitInOutput, gotYAML)
			}
		})
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
	setExecDockerComposeForTest(t, func(inv *Invocation, overrideYAML string, extraEnv []string) error {
		if overrideYAML == "" && len(inv.ComposeArgs) > 0 && inv.ComposeArgs[0] == "build" {
			prebuildCalls++
			prebuildArgs = append([]string{}, inv.ComposeArgs...)
		}
		return nil
	})

	inv, err := ParseInvocation([]string{"show-compose", "--build"})
	if err != nil {
		t.Fatalf("ParseInvocation: %v", err)
	}
	_, _, err = buildInjectionOverride(context.Background(), ds, "vaka.yaml", inv, true)
	if err != nil {
		t.Fatalf("buildInjectionOverride: %v", err)
	}

	if prebuildCalls != 1 {
		t.Fatalf("prebuild calls = %d, want 1", prebuildCalls)
	}
	if !strings.Contains(strings.Join(prebuildArgs, " "), "app") {
		t.Fatalf("prebuild args %v do not include service name", prebuildArgs)
	}
}
