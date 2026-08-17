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

func currentManagedLifecycleTarget() execTarget {
	return execTarget{
		Managed:        true,
		RuntimeVersion: runtimeBundleVersion,
		RuntimeImage:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RuntimeMounted: true,
	}
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

func TestParseRunNoDepsUsesLastValueAndStopsAtService(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"--no-deps=false", "--no-deps", "app"}, want: true},
		{args: []string{"--no-deps", "--no-deps=0", "app"}, want: false},
		{args: []string{"app", "sh", "--no-deps"}, want: false},
	}
	for _, tc := range tests {
		parsed, err := parseRun(tc.args)
		if err != nil {
			t.Fatalf("parseRun(%v): %v", tc.args, err)
		}
		if parsed.noDeps != tc.want {
			t.Errorf("parseRun(%v).noDeps = %t, want %t", tc.args, parsed.noDeps, tc.want)
		}
	}
	if _, err := parseRun([]string{"--no-deps=garbage", "app"}); err == nil {
		t.Fatal("malformed --no-deps value was accepted")
	}
}

func TestParseRunQuietOptionsUseLastValueAndStopAtService(t *testing.T) {
	tests := []struct {
		args           []string
		wantQuietPull  bool
		wantQuietBuild bool
	}{
		{
			args:           []string{"--quiet-pull=false", "--quiet-pull", "--quiet-build=0", "--quiet-build", "app"},
			wantQuietPull:  true,
			wantQuietBuild: true,
		},
		{
			args: []string{"--quiet-pull", "--quiet-pull=false", "--quiet-build", "--quiet-build=0", "app"},
		},
		{
			args: []string{"app", "tool", "--quiet-pull=garbage", "--quiet-build=garbage"},
		},
		{
			args: []string{"app", "--", "--quiet-pull", "--quiet-build"},
		},
	}
	for _, tc := range tests {
		parsed, err := parseRun(tc.args)
		if err != nil {
			t.Fatalf("parseRun(%v): %v", tc.args, err)
		}
		if parsed.quietPull != tc.wantQuietPull || parsed.quietBuild != tc.wantQuietBuild {
			t.Errorf("parseRun(%v) quiet pull/build = %t/%t, want %t/%t", tc.args,
				parsed.quietPull, parsed.quietBuild, tc.wantQuietPull, tc.wantQuietBuild)
		}
	}
}

func TestParseRunAcceptsComposeAliasesAndValidatesBeforePreparation(t *testing.T) {
	parsed, err := parseRun([]string{
		"--volumes", "scratch:/workspace",
		"--labels", "purpose=test",
		"--no-TTY=false",
		"--service-ports=false",
		"--publish", "127.0.0.1:8080:80",
		"app", "id",
	})
	if err != nil {
		t.Fatalf("valid aliases rejected: %v", err)
	}
	if parsed.service != "app" || parsed.servicePorts {
		t.Fatalf("parsed run = %+v", parsed)
	}

	tests := [][]string{
		{"--service-ports", "--publish", "8080:80", "app"},
		{"--publish", "not-a-port", "app"},
		{"--volume", "bad::volume", "app"},
		{"--label", "missing-value", "app"},
		{"--tty", "--no-TTY=false", "app"},
		{"--entrypoint", "'unterminated", "app"},
		{"--env-from-file", "does-not-exist.env", "app"},
	}
	for _, args := range tests {
		if _, err := parseRun(args); err == nil {
			t.Errorf("parseRun(%v) unexpectedly succeeded", args)
		}
	}
}

func TestSelectRunServicesUsesDependencyGraph(t *testing.T) {
	project := &composetypes.Project{Services: composetypes.Services{
		"app": {Name: "app", DependsOn: map[string]composetypes.ServiceDependency{
			"db": {Condition: "service_started"},
		}},
		"db": {Name: "db", DependsOn: map[string]composetypes.ServiceDependency{
			"cache": {Condition: "service_started"},
		}},
		"cache":     {Name: "cache"},
		"unrelated": {Name: "unrelated"},
	}}
	for _, tc := range []struct {
		args []string
		want map[string]bool
	}{
		{args: []string{"app"}, want: map[string]bool{"app": true, "db": true, "cache": true}},
		{args: []string{"--no-deps", "app"}, want: map[string]bool{"app": true}},
		{args: []string{"--no-deps", "--no-deps=false", "app"}, want: map[string]bool{"app": true, "db": true, "cache": true}},
	} {
		parsed, err := parseRun(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		selected, err := selectRunServices(project, parsed)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for name := range selected.Services {
			got[name] = true
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("selected run graph for %v = %v, want %v", tc.args, got, tc.want)
		}
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
		{name: "future watch action", svc: composetypes.ServiceConfig{Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchAction("future")}}}}, want: "has not been reviewed"},
		{name: "deprecated x-develop", svc: composetypes.ServiceConfig{Extensions: composetypes.Extensions{"x-develop": map[string]any{"watch": []any{}}}}, want: "x-develop"},
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
		"app": {
			Secrets: []composetypes.ServiceSecretConfig{{Source: "api-key"}},
			Develop: &composetypes.DevelopConfig{Watch: []composetypes.Trigger{{Action: composetypes.WatchActionRestart}}},
		},
	}}
	if err := validateManagedExecutionSurfaces(managedTestPolicy(), project); err != nil {
		t.Fatalf("unrelated secret rejected: %v", err)
	}
}

func TestPreStartFailsClosedUntilExecutionSurfaceIsReviewed(t *testing.T) {
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
    pre_start:
      - command: ["true"]
`)
	inv, _ := ParseComposeInvocation([]string{"up", "app"})
	_, _, err := buildInjectionOverride(t.Context(), &fakeBuilderDockerServices{}, "vaka.yaml", inv, false)
	if err == nil || !strings.Contains(err.Error(), "pre_start") {
		t.Fatalf("pre_start must fail before Compose execution until Vaka reviews this surface; error = %v", err)
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
	ds := &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{"app": {currentManagedLifecycleTarget()}}}
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
	current := currentManagedLifecycleTarget()
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
		"dep": {Name: "dep"},
		"app": {Name: "app", DependsOn: map[string]composetypes.ServiceDependency{
			"dep": {Condition: "service_started", Restart: true},
		}},
		"worker": {Name: "worker", DependsOn: map[string]composetypes.ServiceDependency{
			"app": {Condition: "service_started", Restart: true},
		}},
		"inert": {Name: "inert", DependsOn: map[string]composetypes.ServiceDependency{
			"dep": {Condition: "service_started", Restart: false},
		}},
	}}
	for _, tc := range []struct {
		args []string
		want map[string]bool
	}{
		{args: []string{"restart", "dep"}, want: map[string]bool{"dep": true, "app": true, "worker": true}},
		{args: []string{"restart", "app"}, want: map[string]bool{"app": true, "worker": true}},
		{args: []string{"restart", "-t0", "app"}, want: map[string]bool{"app": true, "worker": true}},
		{args: []string{"restart", "--no-deps", "app"}, want: map[string]bool{"app": true}},
		{args: []string{"start", "app"}, want: map[string]bool{"app": true, "dep": true}},
		{args: []string{"down", "--remove-orphans", "dep"}, want: map[string]bool{"dep": true}},
		{args: []string{"down", "app"}, want: map[string]bool{"app": true}},
		{args: []string{"rm", "-fsv", "app"}, want: map[string]bool{"app": true}},
		{args: []string{"stop", "--timeout=5", "app"}, want: map[string]bool{"app": true}},
		{args: []string{"stop", "-t0", "app"}, want: map[string]bool{"app": true}},
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
	for _, args := range [][]string{{"restart", "-tbad", "app"}, {"stop", "--timeout=bad", "app"}} {
		inv, _ := ParseComposeInvocation(args)
		if _, err := referenceValidationServices(project, inv); err == nil || !strings.Contains(err.Error(), "requires an integer") {
			t.Errorf("referenceValidationServices(%v) error = %v", args, err)
		}
	}
}

func TestReferenceValidationServiceTargetsRespectProfiles(t *testing.T) {
	project := &composetypes.Project{
		Services: composetypes.Services{
			"app": {Name: "app"},
		},
		DisabledServices: composetypes.Services{
			"debug": {Name: "debug", Profiles: []string{"debug"}},
		},
	}

	inv, _ := ParseComposeInvocation([]string{"start"})
	got, err := referenceValidationServices(project, inv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]bool{"app": true}) {
		t.Fatalf("project-wide selection = %v, want only active app", got)
	}

	inv, _ = ParseComposeInvocation([]string{"start", "debug"})
	got, err = referenceValidationServices(project, inv)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]bool{"debug": true}) {
		t.Fatalf("explicit profile selection = %v, want debug", got)
	}
}

func TestLifecycleRestartValidatesRestartDependents(t *testing.T) {
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
  dep:
    image: alpine:3.20
  app:
    image: alpine:3.20
    depends_on:
      dep:
        condition: service_started
        restart: true
`)
	setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
		"dep": {currentManagedLifecycleTarget()},
		"app": {{Managed: false}},
	}})

	inv, _ := ParseComposeInvocation([]string{"restart", "dep"})
	err := validateReferenceExecutionSurfaces("vaka.yaml", inv)
	if err == nil || !strings.Contains(err.Error(), "service app") || !strings.Contains(err.Error(), "was not created by Vaka") {
		t.Fatalf("restart dependent validation error = %v", err)
	}

	inv, _ = ParseComposeInvocation([]string{"restart", "--no-deps", "dep"})
	if err := validateReferenceExecutionSurfaces("vaka.yaml", inv); err != nil {
		t.Fatalf("restart --no-deps validated an excluded dependent: %v", err)
	}
}

func TestLifecycleTargetedDownValidatesOnlyExplicitServices(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services: {}
`, `
services:
  dep:
    image: alpine:3.20
  app:
    image: alpine:3.20
    depends_on:
      dep:
        condition: service_started
        restart: false
  worker:
    image: alpine:3.20
    depends_on:
      app:
        condition: service_started
    pre_stop:
      - command: ["true"]
`)
	setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
		"dep":    {currentManagedLifecycleTarget()},
		"app":    {currentManagedLifecycleTarget()},
		"worker": {currentManagedLifecycleTarget()},
	}})

	inv, _ := ParseComposeInvocation([]string{"down", "dep"})
	if err := validateReferenceExecutionSurfaces("vaka.yaml", inv); err != nil {
		t.Fatalf("targeted down validated an untouched dependent: %v", err)
	}
}

func TestDownRemoveOrphansValidatesCurrentHooksForOneoffs(t *testing.T) {
	tests := []struct {
		name       string
		policyBody string
		target     execTarget
	}{
		{
			name: "policy managed but unlabelled",
			policyBody: `
  app:
    network:
      egress:
        defaultAction: reject`,
			target: execTarget{Oneoff: true},
		},
		{name: "live labelled after policy removal", policyBody: " {}", target: execTarget{Oneoff: true, Managed: true}},
		{name: "legacy oneoff", policyBody: " {}", target: execTarget{Oneoff: true, LegacyManaged: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)
			writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:`+tc.policyBody, `
services:
  app:
    image: alpine:3.20
    pre_stop:
      - command: ["id"]
`)
			ds := &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{"app": {tc.target}}}
			setDockerServicesFactoryForTest(t, ds)

			inv, _ := ParseComposeInvocation([]string{"down", "--remove-orphans", "app"})
			err := validateReferenceExecutionSurfaces("vaka.yaml", inv)
			if err == nil || !strings.Contains(err.Error(), "pre_stop") {
				t.Fatalf("one-off hook error = %v", err)
			}
			if len(ds.inspections) != 1 || !ds.inspections[0].IncludeOneoffs {
				t.Fatalf("one-offs were not included in inspection: %+v", ds.inspections)
			}

			inv, _ = ParseComposeInvocation([]string{"down", "--remove-orphans=false", "app"})
			if err := validateReferenceExecutionSurfaces("vaka.yaml", inv); err != nil {
				t.Fatalf("disabled --remove-orphans inspected one-off hook: %v", err)
			}

			t.Setenv("COMPOSE_REMOVE_ORPHANS", "true")
			inv, _ = ParseComposeInvocation([]string{"down", "app"})
			if err := validateReferenceExecutionSurfaces("vaka.yaml", inv); err == nil || !strings.Contains(err.Error(), "pre_stop") {
				t.Fatalf("COMPOSE_REMOVE_ORPHANS did not include one-off hook: %v", err)
			}
		})
	}
}

func TestLifecycleResumeClassification(t *testing.T) {
	tests := []struct {
		name          string
		policyManaged bool
		targets       []execTarget
		want          string
	}{
		{name: "ordinary unmanaged", targets: []execTarget{{}}},
		{name: "current managed", targets: []execTarget{currentManagedLifecycleTarget()}},
		{name: "policy-managed ordinary", policyManaged: true, targets: []execTarget{{}}, want: "was not created by Vaka"},
		{name: "legacy", targets: []execTarget{{LegacyManaged: true}}, want: "older or mutable Vaka runtime"},
		{name: "mixed replicas", targets: []execTarget{currentManagedLifecycleTarget(), {}}, want: "mix of Vaka-managed and ordinary"},
		{name: "one stale replica", targets: []execTarget{currentManagedLifecycleTarget(), {Managed: true, RuntimeVersion: "v0.1.0", RuntimeMounted: true}}, want: "older or mutable Vaka runtime"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLifecycleResumeTargets("app", "vaka.yaml", "start", tc.policyManaged, tc.targets)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLifecycleContainmentDoesNotRequireCurrentRuntime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policyYAML string
		target     execTarget
	}{
		{
			name: "policy-managed ordinary container",
			policyYAML: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`,
		},
		{
			name: "stale managed container",
			policyYAML: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services: {}
`,
			target: execTarget{Managed: true, RuntimeVersion: "v0.1.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)
			writeFixtureFiles(t, dir, tc.policyYAML, `
services:
  app:
    image: alpine:3.20
`)
			setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
				"app": {tc.target},
			}})
			inv, _ := ParseComposeInvocation([]string{"stop", "app"})
			if err := validateReferenceExecutionSurfaces("vaka.yaml", inv); err != nil {
				t.Fatalf("containment was blocked: %v", err)
			}
		})
	}
}

func TestLifecycleHooksFollowLiveManagementAndOperation(t *testing.T) {
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
      - command: ["true"]
    post_start:
      - command: ["true"]
`)

	for _, tc := range []struct {
		name   string
		target execTarget
		verb   string
		want   string
	}{
		{name: "ordinary stop remains native", target: execTarget{}, verb: "stop"},
		{name: "managed stop checks pre-stop", target: currentManagedLifecycleTarget(), verb: "stop", want: "pre_stop"},
		{name: "managed start checks post-start", target: currentManagedLifecycleTarget(), verb: "start", want: "post_start"},
		{name: "unpause runs no hooks", target: currentManagedLifecycleTarget(), verb: "unpause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{projectTargets: map[string][]execTarget{
				"app": {tc.target},
			}})
			inv, _ := ParseComposeInvocation([]string{tc.verb, "app"})
			err := validateReferenceExecutionSurfaces("vaka.yaml", inv)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
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
