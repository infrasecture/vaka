package main

import (
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"vaka.dev/vaka/pkg/policy"
)

func managedTestPolicy() *policy.ServicePolicy {
	return &policy.ServicePolicy{Services: map[string]*policy.ServiceConfig{"app": {}}}
}

func TestValidateRunInvocationRejectsSecurityOverrides(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "entrypoint", args: []string{"run", "--entrypoint", "sh", "app"}, want: "--entrypoint bypasses"},
		{name: "user", args: []string{"run", "-u1000", "app"}, want: "--user is incompatible"},
		{name: "runtime child mount", args: []string{"run", "-v", "scratch:/opt/vaka/sbin", "app"}, want: "protected runtime"},
		{name: "runtime ancestor mount", args: []string{"run", "--volume=scratch:/opt", "app"}, want: "protected runtime"},
		{name: "root mount", args: []string{"run", "-v", "scratch:/", "app"}, want: "protected runtime"},
		{name: "secret mount", args: []string{"run", "-v", "scratch:/run/secrets/vaka.yaml", "app"}, want: "policy mounts"},
		{name: "protected label", args: []string{"run", "--label", "agent.vaka.managed=false", "app"}, want: "security metadata"},
		{name: "compose identity label", args: []string{"run", "--label", "com.docker.compose.oneoff=false", "app"}, want: "security metadata"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := ParseComposeInvocation(tc.args)
			if err != nil {
				t.Fatalf("ParseComposeInvocation: %v", err)
			}
			err = validateRunInvocation(inv, managedTestPolicy())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateRunInvocationPreservesSafeOptions(t *testing.T) {
	inv, err := ParseComposeInvocation([]string{
		"run", "--rm", "-e", "A=1", "-v", "scratch:/workspace", "--label", "purpose=test", "app", "sh", "-c", "id",
	})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	if err := validateRunInvocation(inv, managedTestPolicy()); err != nil {
		t.Fatalf("safe run options rejected: %v", err)
	}
}

func TestValidateRunInvocationLeavesUnmanagedServiceAlone(t *testing.T) {
	inv, _ := ParseComposeInvocation([]string{"run", "--entrypoint", "sh", "sidecar"})
	if err := validateRunInvocation(inv, managedTestPolicy()); err != nil {
		t.Fatalf("unmanaged service override rejected: %v", err)
	}
}

func TestValidateManagedExecutionSurfaces(t *testing.T) {
	tests := []struct {
		name string
		svc  composetypes.ServiceConfig
		want string
	}{
		{name: "post start", svc: composetypes.ServiceConfig{PostStart: []composetypes.ServiceHook{{Command: []string{"id"}}}}, want: "post_start"},
		{name: "pre stop", svc: composetypes.ServiceConfig{PreStop: []composetypes.ServiceHook{{Command: []string{"id"}}}}, want: "pre_stop"},
		{name: "sync exec", svc: composetypes.ServiceConfig{Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchActionSyncExec}}}}, want: "sync+exec"},
		{name: "sync protected ancestor", svc: composetypes.ServiceConfig{Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchActionSync, Target: "/opt"}}}}, want: "protected runtime"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{"app": tc.svc}}
			err := validateManagedExecutionSurfaces(managedTestPolicy(), project)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPathsOverlap(t *testing.T) {
	for _, pair := range [][2]string{{"/", "/opt/vaka"}, {"/opt", "/opt/vaka"}, {"/opt/vaka", "/opt/vaka"}, {"/opt/vaka/sbin", "/opt/vaka"}} {
		if !pathsOverlap(pair[0], pair[1]) {
			t.Errorf("pathsOverlap(%q, %q) = false", pair[0], pair[1])
		}
	}
	if pathsOverlap("/workspace", "/opt/vaka") {
		t.Fatal("unrelated paths overlap")
	}
}

func TestLifecycleValidationUsesLiveManagedServices(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services: {}
`, `
services:
  app:
    image: alpine:3.20
    pre_stop:
      - command: ["sh", "-c", "echo unsafe"]
`)
	ds := &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
		"app": {{
			Managed:        true,
			RuntimeVersion: runtimeBundleVersion,
			RuntimeImage:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RuntimeMounted: true,
		}},
	}}
	setDockerServicesFactoryForTest(t, ds)
	inv, err := ParseComposeInvocation([]string{"stop"})
	if err != nil {
		t.Fatal(err)
	}
	err = validateReferenceExecutionSurfaces("missing-vaka-file-is-not-used.yaml", inv)
	if err == nil || !strings.Contains(err.Error(), "pre_stop") {
		t.Fatalf("lifecycle validation error = %v", err)
	}
}

func TestLifecycleStartRejectsOldRuntime(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services: {}
`, `
services:
  app:
    image: alpine:3.20
`)
	ds := &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
		"app": {{Managed: true, RuntimeVersion: "v0.1.1", RuntimeMounted: true}},
	}}
	setDockerServicesFactoryForTest(t, ds)
	inv, _ := ParseComposeInvocation([]string{"start"})
	err := validateReferenceExecutionSurfaces("unused.yaml", inv)
	if err == nil || !strings.Contains(err.Error(), "force-recreate") {
		t.Fatalf("old runtime error = %v", err)
	}
}
