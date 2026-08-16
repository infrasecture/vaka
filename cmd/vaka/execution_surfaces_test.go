package main

import (
	"reflect"
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
		{name: "cap drop", args: []string{"run", "--cap-drop", "SETPCAP", "app"}, want: "--cap-add/--cap-drop"},
		{name: "cap add", args: []string{"run", "--cap-add=NET_ADMIN", "app"}, want: "--cap-add/--cap-drop"},
		{name: "policy environment", args: []string{"run", "-e", "AGENT_VAKA_POLICY=bad", "app"}, want: "reserved Vaka environment"},
		{name: "environment file", args: []string{"run", "--env-from-file", "override.env", "app"}, want: "--env-from-file"},
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
		{name: "sync", svc: composetypes.ServiceConfig{Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchActionSync}}}}, want: "action sync"},
		{name: "sync restart", svc: composetypes.ServiceConfig{Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchActionSyncRestart}}}}, want: "action sync+restart"},
		{name: "sync exec", svc: composetypes.ServiceConfig{Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchActionSyncExec}}}}, want: "action sync+exec"},
		{name: "rebuild", svc: composetypes.ServiceConfig{Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchActionRebuild}}}}, want: "action rebuild"},
		{name: "nested volume", svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{Target: "/opt/vaka/sbin"}}}, want: "volume target"},
		{name: "ancestor volume", svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{Target: "/opt"}}}, want: "volume target"},
		{name: "config", svc: composetypes.ServiceConfig{Configs: []composetypes.ServiceConfigObjConfig{{Source: "cfg", Target: "/opt/vaka/config"}}}, want: "config target"},
		{name: "secret", svc: composetypes.ServiceConfig{Secrets: []composetypes.ServiceSecretConfig{{Source: "replacement", Target: "/run/secrets/vaka.yaml"}}}, want: "secret target"},
		{name: "tmpfs", svc: composetypes.ServiceConfig{Tmpfs: []string{"/opt/vaka/sbin:rw"}}, want: "tmpfs target"},
		{name: "volumes from", svc: composetypes.ServiceConfig{VolumesFrom: []string{"sidecar:rw"}}, want: "volumes_from"},
		{name: "device", svc: composetypes.ServiceConfig{Devices: []composetypes.DeviceMapping{{Source: "/dev/null", Target: "/opt/vaka/sbin/vaka-init"}}}, want: "device target"},
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

func TestValidateManagedExecutionSurfacesAllowsUnrelatedSecrets(t *testing.T) {
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"app": {Secrets: []composetypes.ServiceSecretConfig{{Source: "api-key"}}},
	}}
	if err := validateManagedExecutionSurfaces(managedTestPolicy(), project); err != nil {
		t.Fatalf("unrelated secret rejected: %v", err)
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
	err = validateReferenceExecutionSurfaces("vaka.yaml", inv)
	if err == nil || !strings.Contains(err.Error(), "pre_stop") {
		t.Fatalf("lifecycle validation error = %v", err)
	}
}

func TestLifecycleResumeRejectsOldRuntime(t *testing.T) {
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
	for _, verb := range []string{"start", "restart", "unpause"} {
		t.Run(verb, func(t *testing.T) {
			inv, _ := ParseComposeInvocation([]string{verb})
			err := validateReferenceExecutionSurfaces("vaka.yaml", inv)
			if err == nil || !strings.Contains(err.Error(), "force-recreate") {
				t.Fatalf("old runtime error = %v", err)
			}
		})
	}
}

func TestLifecycleValidationIgnoresUnrelatedContainers(t *testing.T) {
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
  stale:
    image: alpine:3.20
`)
	current := execTarget{
		Managed: true, RuntimeVersion: runtimeBundleVersion,
		RuntimeImage:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RuntimeMounted: true,
	}
	setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
		"app":   {current},
		"stale": {{Managed: true, RuntimeVersion: "v0.1.0", RuntimeMounted: true}},
	}})

	inv, _ := ParseComposeInvocation([]string{"start", "app"})
	if err := validateReferenceExecutionSurfaces("vaka.yaml", inv); err != nil {
		t.Fatalf("targeted start was blocked by unrelated container: %v", err)
	}
	inv, _ = ParseComposeInvocation([]string{"start"})
	if err := validateReferenceExecutionSurfaces("vaka.yaml", inv); err == nil || !strings.Contains(err.Error(), "force-recreate") {
		t.Fatalf("project-wide start did not reject stale container: %v", err)
	}
}

func TestReferenceValidationServiceTargets(t *testing.T) {
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"app": {Name: "app", DependsOn: map[string]composetypes.ServiceDependency{"db": {Condition: "service_started"}}},
		"db":  {Name: "db"},
	}}
	for _, tc := range []struct {
		args []string
		want map[string]bool
	}{
		{args: []string{"restart", "app"}, want: map[string]bool{"app": true, "db": true}},
		{args: []string{"restart", "--no-deps", "app"}, want: map[string]bool{"app": true}},
		{args: []string{"rm", "-fsv", "app"}, want: map[string]bool{"app": true}},
		{args: []string{"stop", "--timeout=5", "app"}, want: map[string]bool{"app": true}},
	} {
		inv, _ := ParseComposeInvocation(tc.args)
		got, err := referenceValidationServices(project, inv)
		if err != nil {
			t.Fatalf("referenceValidationServices(%v): %v", tc.args, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("referenceValidationServices(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestLifecycleRejectsPolicyManagedUnlabeledContainer(t *testing.T) {
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
`)
	setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
		"app": {{Managed: false}},
	}})

	inv, _ := ParseComposeInvocation([]string{"start", "app"})
	err := validateReferenceExecutionSurfaces("vaka.yaml", inv)
	if err == nil || !strings.Contains(err.Error(), "was not created by Vaka") || !strings.Contains(err.Error(), "raw `docker compose start`") {
		t.Fatalf("unlabeled policy container error = %v", err)
	}
}

func TestReferenceRequiresExecutionValidationForRmStop(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"rm", "-s"}, want: true},
		{args: []string{"rm", "--stop"}, want: true},
		{args: []string{"rm", "--stop=true"}, want: true},
		{args: []string{"rm", "--stop=false"}, want: false},
		{args: []string{"rm"}, want: false},
	}
	for _, tc := range tests {
		inv, err := ParseComposeInvocation(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		got, err := referenceRequiresExecutionValidation(inv)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("referenceRequiresExecutionValidation(%v) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

func TestComposeBoolOptionFalseIsDisabled(t *testing.T) {
	got, err := composeBoolOptionEnabled([]string{"--no-recreate=false"}, "--no-recreate", "")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("--no-recreate=false treated as enabled")
	}
	got, err = composeBoolOptionEnabled([]string{"--no-up=false"}, "--no-up", "")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("--no-up=false treated as enabled")
	}
}

func TestComposeBoolOptionUsesLastValue(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		long  string
		short string
		want  bool
	}{
		{name: "long false then true", args: []string{"--no-recreate=false", "--no-recreate"}, long: "--no-recreate", want: true},
		{name: "long true then false", args: []string{"--no-recreate", "--no-recreate=false"}, long: "--no-recreate", want: false},
		{name: "long true then zero", args: []string{"--build", "--build=0"}, long: "--build", want: false},
		{name: "short false then true", args: []string{"-s=false", "-s"}, long: "--stop", short: "s", want: true},
		{name: "short true then zero", args: []string{"-s", "-s=0"}, long: "--stop", short: "s", want: false},
		{name: "short cluster false", args: []string{"-fs=0"}, long: "--stop", short: "s", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := composeBoolOptionEnabled(tc.args, tc.long, tc.short)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("composeBoolOptionEnabled(%v) = %t, want %t", tc.args, got, tc.want)
			}
		})
	}
}

func TestComposeBoolOptionRejectsMalformedValues(t *testing.T) {
	for _, args := range [][]string{
		{"--build=garbage"},
		{"--build= false "},
		{"-s=garbage"},
		{"--build=garbage", "--build"},
	} {
		if _, err := composeBoolOptionEnabled(args, "--build", "s"); err == nil {
			t.Errorf("composeBoolOptionEnabled(%v) accepted malformed value", args)
		}
	}
}
