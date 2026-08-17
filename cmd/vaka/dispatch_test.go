package main

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// composeExecCall records one execDockerComposeFn invocation.
type composeExecCall struct {
	args         []string
	overrideYAML string
	extraEnv     []string
}

// runRootCapturingExec executes the vaka command tree with argv and returns
// every compose execution it produced, with the docker services factory and
// compose exec hook faked out.
func runRootCapturingExec(t *testing.T, argv []string) ([]composeExecCall, error) {
	t.Helper()
	var calls []composeExecCall
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, overrideYAML string, extraEnv []string) error {
		calls = append(calls, composeExecCall{
			args:         append([]string{}, inv.Args...),
			overrideYAML: overrideYAML,
			extraEnv:     append([]string{}, extraEnv...),
		})
		return nil
	})
	oldContainerExec := execDockerContainerFn
	execDockerContainerFn = func(args []string) error {
		calls = append(calls, composeExecCall{args: append([]string{"docker"}, args...)})
		return nil
	}
	t.Cleanup(func() { execDockerContainerFn = oldContainerExec })
	setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{})

	root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml", Rest: argv})
	root.SetArgs(argv)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	return calls, err
}

func stubComposeVerbDiscoveryUnavailable(t *testing.T) {
	t.Helper()
	old := dockerComposeHelpOutput
	dockerComposeHelpOutput = func() ([]byte, error) {
		return nil, errors.New("docker unavailable in tests")
	}
	t.Cleanup(func() {
		dockerComposeHelpOutput = old
	})
}

// TestShorthandEquivalence proves that every top-level shorthand produces a
// compose execution byte-identical to its `vaka compose ...` form, at the
// execDockerComposeFn boundary.
func TestShorthandEquivalence(t *testing.T) {
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
    image: alpine:3.20
    user: "1000:1000"
    entrypoint: ["sleep"]
    command: ["infinity"]
`

	tests := []struct {
		name string
		args []string
	}{
		{"up", []string{"up", "-d"}},
		{"up with build", []string{"up", "--build"}},
		{"down", []string{"down", "--volumes"}},
		{"start", []string{"start", "app"}},
		{"stop", []string{"stop", "app"}},
		{"run", []string{"run", "--rm", "app", "sh"}},
		{"run with payload dashes", []string{"run", "app", "sh", "-c", "echo hi"}},
		{"exec", []string{"exec", "app", "sh"}},
		{"logs", []string{"logs", "-f", "app"}},
		{"ps", []string{"ps", "-a"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)
			writeFixtureFiles(t, dir, policyYAML, composeYAML)

			shorthandCalls, err := runRootCapturingExec(t, tc.args)
			if err != nil {
				t.Fatalf("shorthand execute: %v", err)
			}
			composeCalls, err := runRootCapturingExec(t, append([]string{"compose"}, tc.args...))
			if err != nil {
				t.Fatalf("compose execute: %v", err)
			}

			if len(shorthandCalls) == 0 {
				t.Fatal("shorthand produced no compose execution")
			}
			if !reflect.DeepEqual(shorthandCalls, composeCalls) {
				t.Fatalf("shorthand and compose executions differ\nshorthand: %+v\ncompose:   %+v", shorthandCalls, composeCalls)
			}
		})
	}
}

func TestUnknownTopLevelCommandErrors(t *testing.T) {
	stubComposeVerbDiscoveryUnavailable(t)

	tests := []struct {
		name    string
		args    []string
		want    []string
		wantNot []string
	}{
		{
			name: "compose verb points at namespace",
			args: []string{"pull"},
			want: []string{"vaka compose pull", "up, down, start, stop, run, exec, logs, ps"},
		},
		{
			name: "demoted create points at namespace",
			args: []string{"create"},
			want: []string{"vaka compose create"},
		},
		{
			name: "compose verb with flags still points at namespace",
			args: []string{"pull", "-q"},
			want: []string{"vaka compose pull"},
		},
		{
			name:    "non-compose token is a plain unknown command",
			args:    []string{"frobnicate"},
			want:    []string{"unknown command \"frobnicate\""},
			wantNot: []string{"vaka compose"},
		},
		{
			name: "near-miss native command gets suggestion",
			args: []string{"validat"},
			want: []string{"Did you mean this?", "validate"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := runRootCapturingExec(t, tc.args)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if len(calls) != 0 {
				t.Fatalf("unknown command must not execute docker compose, got %+v", calls)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing %q", err.Error(), want)
				}
			}
			for _, wantNot := range tc.wantNot {
				if strings.Contains(err.Error(), wantNot) {
					t.Fatalf("error %q unexpectedly contains %q", err.Error(), wantNot)
				}
			}
		})
	}
}

func TestComposeHelpAndMetadataProxying(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantArgs []string
	}{
		{"bare compose proxies docker usage", []string{"compose"}, nil},
		{"compose --help proxies", []string{"compose", "--help"}, []string{"--help"}},
		{"shorthand --help proxies subcommand help", []string{"up", "--help"}, []string{"up", "--help"}},
		{"compose subcommand --help proxies", []string{"compose", "logs", "-h"}, []string{"logs", "-h"}},
		{"vaka help up proxies subcommand help", []string{"help", "up"}, []string{"up", "--help"}},
		{"vaka help compose proxies compose help", []string{"help", "compose"}, []string{"--help"}},
		{"compose version is metadata passthrough", []string{"compose", "version"}, []string{"version"}},
		{"compose ls is metadata passthrough", []string{"compose", "ls", "-q"}, []string{"ls", "-q"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := runRootCapturingExec(t, tc.args)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("expected exactly one compose execution, got %+v", calls)
			}
			if calls[0].overrideYAML != "" {
				t.Fatalf("help/metadata proxying must not inject an override, got:\n%s", calls[0].overrideYAML)
			}
			if len(calls[0].args) != len(tc.wantArgs) {
				t.Fatalf("args = %v, want %v", calls[0].args, tc.wantArgs)
			}
			for i := range tc.wantArgs {
				if calls[0].args[i] != tc.wantArgs[i] {
					t.Fatalf("args = %v, want %v", calls[0].args, tc.wantArgs)
				}
			}
		})
	}
}

func TestComposeBackedCompletionIsQuietAndDisablesFileFallback(t *testing.T) {
	for _, args := range [][]string{
		{"__complete", "compose", ""},
		{"__complete", "up", ""},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			setExecDockerComposeForTest(t, func(inv *ComposeInvocation, overrideYAML string, extraEnv []string) error {
				t.Fatalf("completion must not execute docker compose, got args %v", inv.Args)
				return nil
			})

			root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml", VakaInitPresent: true, Rest: args})
			var stdout, stderr bytes.Buffer
			root.SetArgs(args)
			root.SetOut(&stdout)
			root.SetErr(&stderr)

			if err := root.Execute(); err != nil {
				t.Fatalf("execute completion: %v", err)
			}
			if got := stdout.String(); got != ":4\n" {
				t.Fatalf("completion stdout = %q, want no-file directive only", got)
			}
			if strings.Contains(stderr.String(), "docker compose") {
				t.Fatalf("completion stderr should not contain docker compose proxy output:\n%s", stderr.String())
			}
		})
	}
}

func TestComposeNamespaceKeepsComposeGlobals(t *testing.T) {
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

	calls, err := runRootCapturingExec(t, []string{"compose", "-f", "docker-compose.yaml", "up", "-d"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected one compose execution, got %+v", calls)
	}
	assertArgv(t, []string{"-f", "docker-compose.yaml", "up", "-d"}, calls[0].args)
	if calls[0].overrideYAML == "" {
		t.Fatal("render path must inject an override")
	}
}

func TestContainerCreatingComposeCommandsUseFullOverride(t *testing.T) {
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

	for _, args := range [][]string{
		{"compose", "scale", "app=2"},
		{"compose", "watch"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls, err := runRootCapturingExec(t, args)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("expected one compose execution, got %+v", calls)
			}
			if !strings.Contains(calls[0].overrideYAML, "services:") ||
				!strings.Contains(calls[0].overrideYAML, "agent.vaka.policy-revision") {
				t.Fatalf("command must receive full policy override:\n%s", calls[0].overrideYAML)
			}
		})
	}
}

func TestComposeWatchNoUpRejectsManagedContainerReuse(t *testing.T) {
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

	calls, err := runRootCapturingExec(t, []string{"compose", "watch", "--no-up"})
	if err == nil || !strings.Contains(err.Error(), "older unsafe runtime") {
		t.Fatalf("watch --no-up error = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("watch --no-up must not execute Compose for managed services, got %+v", calls)
	}
}

func TestComposeNoRecreateRejectsManagedContainerReuse(t *testing.T) {
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

	for _, verb := range []string{"up", "create"} {
		t.Run(verb+" enabled last", func(t *testing.T) {
			calls, err := runRootCapturingExec(t, []string{"compose", verb, "--no-recreate=false", "--no-recreate"})
			if err == nil || !strings.Contains(err.Error(), "older unsafe runtime") {
				t.Fatalf("%s --no-recreate error = %v", verb, err)
			}
			if len(calls) != 0 {
				t.Fatalf("%s --no-recreate must not execute Compose, got %+v", verb, calls)
			}
		})
		t.Run(verb+" disabled last", func(t *testing.T) {
			calls, err := runRootCapturingExec(t, []string{"compose", verb, "--no-recreate", "--no-recreate=0"})
			if err != nil {
				t.Fatalf("%s disabled no-recreate: %v", verb, err)
			}
			if len(calls) != 1 {
				t.Fatalf("%s calls = %+v, want one", verb, calls)
			}
		})
	}
}

func TestContainerReuseGuardsFollowComposeServiceSelection(t *testing.T) {
	policyYAML := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  policy:
    network:
      egress:
        defaultAction: reject
`
	composeYAML := `
services:
  policy:
    image: alpine:3.20
  app:
    image: alpine:3.20
    depends_on:
      policy:
        condition: service_started
`

	for _, verb := range []string{"up", "create"} {
		t.Run(verb+" unmanaged target", func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)
			writeFixtureFiles(t, dir, policyYAML, `
services:
  policy:
    image: alpine:3.20
  app:
    image: alpine:3.20
`)

			calls, err := runRootCapturingExec(t, []string{"compose", verb, "--no-recreate", "app"})
			if err != nil {
				t.Fatalf("%s unmanaged selection: %v", verb, err)
			}
			if len(calls) != 1 {
				t.Fatalf("%s calls = %+v, want one", verb, calls)
			}
		})
		t.Run(verb+" managed dependency", func(t *testing.T) {
			dir := t.TempDir()
			chdirForTest(t, dir)
			writeFixtureFiles(t, dir, policyYAML, composeYAML)

			calls, err := runRootCapturingExec(t, []string{"compose", verb, "--no-recreate", "app"})
			if err == nil || !strings.Contains(err.Error(), "older unsafe runtime") {
				t.Fatalf("%s managed dependency error = %v", verb, err)
			}
			if len(calls) != 0 {
				t.Fatalf("%s must not execute Compose, got %+v", verb, calls)
			}
		})
	}
	t.Run("up unmanaged target without dependencies", func(t *testing.T) {
		dir := t.TempDir()
		chdirForTest(t, dir)
		writeFixtureFiles(t, dir, policyYAML, composeYAML)

		calls, err := runRootCapturingExec(t, []string{"compose", "up", "--no-recreate", "--no-deps", "app"})
		if err != nil {
			t.Fatalf("up unmanaged selection: %v", err)
		}
		if len(calls) != 1 {
			t.Fatalf("up calls = %+v, want one", calls)
		}
	})

	t.Run("watch unmanaged target without managed dependency", func(t *testing.T) {
		dir := t.TempDir()
		chdirForTest(t, dir)
		writeFixtureFiles(t, dir, policyYAML, `
services:
  policy:
    image: alpine:3.20
  app:
    image: alpine:3.20
`)

		calls, err := runRootCapturingExec(t, []string{"compose", "watch", "--no-up", "app"})
		if err != nil {
			t.Fatalf("watch unmanaged selection: %v", err)
		}
		if len(calls) != 1 {
			t.Fatalf("watch calls = %+v, want one", calls)
		}
	})
	t.Run("watch managed dependency", func(t *testing.T) {
		dir := t.TempDir()
		chdirForTest(t, dir)
		writeFixtureFiles(t, dir, policyYAML, composeYAML)

		calls, err := runRootCapturingExec(t, []string{"compose", "watch", "--no-up", "app"})
		if err == nil || !strings.Contains(err.Error(), "older unsafe runtime") {
			t.Fatalf("watch managed dependency error = %v", err)
		}
		if len(calls) != 0 {
			t.Fatalf("watch must not execute Compose, got %+v", calls)
		}
	})
}

func TestUnknownComposeCommandFailsClosed(t *testing.T) {
	calls, err := runRootCapturingExec(t, []string{"compose", "future-container-command"})
	if err == nil || !strings.Contains(err.Error(), "has not been reviewed") {
		t.Fatalf("unknown command error = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("unknown command must not execute Compose, got %+v", calls)
	}
}

func TestComposeDryRunRejectedBeforeDockerAction(t *testing.T) {
	tests := [][]string{
		{"compose", "--dry-run", "up", "--build"},
		{"up", "--dry-run", "--build"},
		{"compose", "--dry-run", "exec", "app", "id"},
		{"exec", "--dry-run", "app", "id"},
		{"compose", "--dry-run", "exec", "--help"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls, err := runRootCapturingExec(t, args)
			if err == nil || !strings.Contains(err.Error(), "raw `docker compose --dry-run`") {
				t.Fatalf("dry-run error = %v", err)
			}
			if len(calls) != 0 {
				t.Fatalf("dry-run executed Docker action: %+v", calls)
			}
		})
	}
}

func TestComposeDryRunRespectsOptionBoundaryAndLastValue(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr bool
	}{
		{args: []string{"--dry-run", "exec", "--dry-run=false", "app", "id"}},
		{args: []string{"--dry-run=false", "exec", "--dry-run", "app", "id"}, wantErr: true},
		{args: []string{"run", "app", "sh", "--dry-run"}},
		{args: []string{"exec", "app", "sh", "--dry-run"}},
	}
	for _, tc := range tests {
		inv, err := ParseComposeInvocation(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		err = rejectComposeDryRun(inv)
		if tc.wantErr && err == nil {
			t.Errorf("rejectComposeDryRun(%v) returned nil", tc.args)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("rejectComposeDryRun(%v) = %v", tc.args, err)
		}
	}
}

func TestMalformedConsumedBooleanRejectedBeforeDockerAction(t *testing.T) {
	for _, args := range [][]string{
		{"compose", "up", "--build=garbage"},
		{"compose", "up", "--force-recreate=garbage"},
		{"compose", "create", "-y=garbage"},
		{"compose", "watch", "--quiet=garbage"},
		{"up", "--no-recreate=garbage"},
		{"compose", "run", "--build=garbage", "app"},
		{"compose", "rm", "--stop=garbage"},
	} {
		calls, err := runRootCapturingExec(t, args)
		if err == nil || !strings.Contains(err.Error(), "invalid boolean value") {
			t.Errorf("malformed boolean %v error = %v", args, err)
		}
		if len(calls) != 0 {
			t.Errorf("malformed boolean %v executed Docker action: %+v", args, calls)
		}
	}
}

func TestInvalidRenderInvocationRejectedBeforeDockerAction(t *testing.T) {
	tests := [][]string{
		{"compose", "scale", "--no-deps=garbage", "app=1"},
		{"compose", "scale"},
		{"compose", "scale", "app"},
		{"compose", "scale", "=1"},
		{"compose", "up", "--no-start", "--scale", "app=garbage", "app"},
		{"compose", "up", "--no-start", "--wait-timeout=-1", "app"},
		{"compose", "up", "--force-recreate", "--no-recreate", "app"},
		{"compose", "up", "--always-recreate-deps", "--no-recreate", "app"},
		{"compose", "up", "--renew-anon-volumes", "--no-recreate", "app"},
		{"compose", "up", "--attach", "app", "--attach-dependencies"},
		{"compose", "create", "--build", "--no-build", "app"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls, err := runRootCapturingExec(t, args)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if len(calls) != 0 {
				t.Fatalf("invalid render invocation executed Docker action: %+v", calls)
			}
		})
	}
}

func TestComposeGlobalValidationPrecedesDockerAction(t *testing.T) {
	tests := [][]string{
		{"compose", "--ansi=rainbow", "up", "--build"},
		{"compose", "--progress=loud", "up", "--build"},
		{"compose", "--parallel=many", "up", "--build"},
		{"compose", "--future-global", "up", "--build"},
		{"compose", "--workdir=/tmp", "up", "--build"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls, err := runRootCapturingExec(t, args)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if len(calls) != 0 {
				t.Fatalf("invalid global invocation executed Docker action: %+v", calls)
			}
		})
	}
}

func TestComposeHelpAfterOptionsDoesNotPrepare(t *testing.T) {
	for _, args := range [][]string{
		{"compose", "up", "--build", "--help"},
		{"compose", "pull", "--quiet", "--help"},
		{"compose", "--help", "up", "--build"},
	} {
		calls, err := runRootCapturingExec(t, args)
		if err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
		if len(calls) != 1 || calls[0].overrideYAML != "" {
			t.Fatalf("help %v calls = %+v, want one raw Compose proxy", args, calls)
		}
	}
}

func TestInvalidPullOptionRejectedBeforeDockerAction(t *testing.T) {
	for _, args := range [][]string{
		{"compose", "up", "--pull=policy"},
		{"up", "--pull=garbage"},
		{"compose", "up", "--pull=ALWAYS"},
		{"compose", "run", "--pull= always", "app"},
		{"compose", "create", "--pull"},
	} {
		calls, err := runRootCapturingExec(t, args)
		if err == nil || !strings.Contains(err.Error(), "--pull") {
			t.Errorf("invalid pull %v error = %v", args, err)
		}
		if len(calls) != 0 {
			t.Errorf("invalid pull %v executed Docker action: %+v", args, calls)
		}
	}
}

func TestPullBuildAcceptedForContainerCreatingCommands(t *testing.T) {
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

	for _, args := range [][]string{
		{"compose", "up", "--pull=build", "app"},
		{"compose", "create", "--pull", "build", "app"},
		{"compose", "run", "--pull=build", "app", "id"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls, err := runRootCapturingExec(t, args)
			if err != nil {
				t.Fatalf("valid --pull=build rejected: %v", err)
			}
			if len(calls) == 0 {
				t.Fatal("valid --pull=build produced no Compose call")
			}
			final := calls[len(calls)-1].args
			if strings.Contains(strings.Join(final, " "), "--pull") {
				t.Fatalf("consumed --pull=build remains in final call: %v", final)
			}
		})
	}
}

func TestUnsupportedImageRefreshOptionsRejectedBeforeDockerAction(t *testing.T) {
	for _, args := range [][]string{
		{"compose", "scale", "--build", "app=2"},
		{"compose", "scale", "--pull=always", "app=2"},
		{"compose", "watch", "--build"},
		{"compose", "watch", "--no-build=false"},
		{"compose", "watch", "--pull", "always"},
	} {
		calls, err := runRootCapturingExec(t, args)
		if err == nil || !strings.Contains(err.Error(), "does not support image refresh option") {
			t.Errorf("unsupported image option %v error = %v", args, err)
		}
		if len(calls) != 0 {
			t.Errorf("unsupported image option %v executed Docker action: %+v", args, calls)
		}
	}
}

func TestComposePullEnsuresRuntimeBeforeReferenceProxy(t *testing.T) {
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

	ds := &fakeBuilderDockerServices{}
	setDockerServicesFactoryForTest(t, ds)
	var calls []composeExecCall
	setExecDockerComposeForTest(t, func(inv *ComposeInvocation, overrideYAML string, extraEnv []string) error {
		calls = append(calls, composeExecCall{
			args:         append([]string{}, inv.Args...),
			overrideYAML: overrideYAML,
			extraEnv:     append([]string{}, extraEnv...),
		})
		return nil
	})

	root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml"})
	root.SetArgs([]string{"compose", "pull"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !reflect.DeepEqual(ds.ensureRefs, []string{vakaInitImageReference()}) {
		t.Fatalf("resolved runtime refs = %v, want %s", ds.ensureRefs, vakaInitImageReference())
	}
	if len(calls) != 1 || !strings.Contains(calls[0].overrideYAML, "x-vaka:") || strings.Contains(calls[0].overrideYAML, "services:") {
		t.Fatalf("pull reference call = %+v", calls)
	}
}

func TestShowNftArgErrorsNameTheMissingService(t *testing.T) {
	t.Run("no args", func(t *testing.T) {
		_, err := runRootCapturingExec(t, []string{"show-nft"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		for _, want := range []string{"missing required <service> argument", "vaka show-nft <service>"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err.Error(), want)
			}
		}
	})

	t.Run("too many args", func(t *testing.T) {
		_, err := runRootCapturingExec(t, []string{"show-nft", "app", "db"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "expected exactly one <service> argument, got 2") {
			t.Fatalf("error %q missing arg-count explanation", err.Error())
		}
	})
}

func TestRootWithoutArgsShowsHelp(t *testing.T) {
	calls, err := runRootCapturingExec(t, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("bare vaka must not execute docker compose, got %+v", calls)
	}
}
