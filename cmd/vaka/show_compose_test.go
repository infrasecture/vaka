package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestShowComposeCmdBuildsSyntheticComposeArgv(t *testing.T) {
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
    image: alpine:3.20
    user: "1000:1000"
    entrypoint: ["sleep"]
    command: ["infinity"]
`
	writeFixtureFiles(t, dir, policyYAML, composeYAML)

	tests := []struct {
		name            string
		args            []string
		wantFactoryArgs []string
		wantErr         string
	}{
		{
			name:            "no flags",
			args:            []string{"show-compose"},
			wantFactoryArgs: []string{"show-compose"},
		},
		{
			name:            "compose file and build flags",
			args:            []string{"show-compose", "-f", "docker-compose.yaml", "--build"},
			wantFactoryArgs: []string{"-f", "docker-compose.yaml", "show-compose", "--build"},
		},
		{
			name:    "unknown flag",
			args:    []string{"show-compose", "--wat"},
			wantErr: "unknown flag",
		},
		{
			name:    "missing output value",
			args:    []string{"show-compose", "-o"},
			wantErr: "flag needs an argument",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds := &fakeBuilderDockerServices{}
			var gotFactoryArgs [][]string
			setDockerServicesFactoryForTest(t, ds, &gotFactoryArgs)

			root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml", VakaInitPresent: true})
			root.SetArgs(tc.args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			_, err := captureStdout(t, root.Execute)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(gotFactoryArgs) != 1 {
				t.Fatalf("newDockerServices called %d times, want 1", len(gotFactoryArgs))
			}
			if !reflect.DeepEqual(gotFactoryArgs[0], tc.wantFactoryArgs) {
				t.Fatalf("factory args = %v, want %v", gotFactoryArgs[0], tc.wantFactoryArgs)
			}
		})
	}
}

func TestRunShowComposeStdoutMatchesBuilder(t *testing.T) {
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
    image: alpine:3.20
    user: "1000:1000"
    entrypoint: ["sleep"]
    command: ["infinity"]
`
	writeFixtureFiles(t, dir, policyYAML, composeYAML)

	ds := &fakeBuilderDockerServices{}
	var gotFactoryArgs [][]string
	setDockerServicesFactoryForTest(t, ds, &gotFactoryArgs)

	baseInv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	wantYAML, extraEnv, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", baseInv, true)
	if err != nil {
		t.Fatalf("buildInjectionOverride: %v", err)
	}

	gotStdout, err := captureStdout(t, func() error {
		inv, parseErr := ParseComposeInvocation([]string{"show-compose"})
		if parseErr != nil {
			return parseErr
		}
		return runShowCompose("vaka.yaml", inv, true, "")
	})
	if err != nil {
		t.Fatalf("runShowCompose: %v", err)
	}

	if gotStdout != wantYAML {
		t.Fatalf("stdout mismatch\n--- got ---\n%s\n--- want ---\n%s", gotStdout, wantYAML)
	}
	if strings.Contains(gotStdout, "VAKA_APP_CONF=") {
		t.Fatalf("stdout must not contain VAKA_APP_CONF assignment, got:\n%s", gotStdout)
	}
	if len(extraEnv) != 1 {
		t.Fatalf("unexpected extraEnv size: got %d, want 1", len(extraEnv))
	}
	kv := strings.SplitN(extraEnv[0], "=", 2)
	if len(kv) != 2 {
		t.Fatalf("malformed extraEnv entry: %q", extraEnv[0])
	}
	if strings.Contains(gotStdout, kv[1]) {
		t.Fatalf("stdout must not contain encoded policy payload")
	}
	if len(gotFactoryArgs) != 1 {
		t.Fatalf("newDockerServices called %d times, want 1", len(gotFactoryArgs))
	}
	wantFactoryArgs := []string{"show-compose"}
	if !reflect.DeepEqual(gotFactoryArgs[0], wantFactoryArgs) {
		t.Fatalf("runShowCompose factory args = %v, want %v", gotFactoryArgs[0], wantFactoryArgs)
	}
}

func TestRunShowComposeWritesOutputFile(t *testing.T) {
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
    image: alpine:3.20
    user: "1000:1000"
    entrypoint: ["sleep"]
    command: ["infinity"]
`
	writeFixtureFiles(t, dir, policyYAML, composeYAML)

	ds := &fakeBuilderDockerServices{}
	var gotFactoryArgs [][]string
	setDockerServicesFactoryForTest(t, ds, &gotFactoryArgs)

	baseInv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	wantYAML, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", baseInv, true)
	if err != nil {
		t.Fatalf("buildInjectionOverride: %v", err)
	}

	outPath := filepath.Join(dir, "override.yaml")
	inv, err := ParseComposeInvocation([]string{"show-compose"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	if err := runShowCompose("vaka.yaml", inv, true, outPath); err != nil {
		t.Fatalf("runShowCompose: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}

	if string(got) != wantYAML {
		t.Fatalf("file output mismatch\n--- got ---\n%s\n--- want ---\n%s", string(got), wantYAML)
	}
	if len(gotFactoryArgs) != 1 {
		t.Fatalf("newDockerServices called %d times, want 1", len(gotFactoryArgs))
	}
	wantFactoryArgs := []string{"show-compose"}
	if !reflect.DeepEqual(gotFactoryArgs[0], wantFactoryArgs) {
		t.Fatalf("runShowCompose factory args = %v, want %v", gotFactoryArgs[0], wantFactoryArgs)
	}
}
