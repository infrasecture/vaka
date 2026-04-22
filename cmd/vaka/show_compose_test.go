package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseShowComposeFlags(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantOutput      string
		wantPassthrough []string
		wantErr         string
	}{
		{
			name:            "stdout default",
			args:            []string{"show-compose"},
			wantOutput:      "",
			wantPassthrough: []string{"show-compose"},
		},
		{
			name:            "short output flag",
			args:            []string{"-f", "prod.yml", "show-compose", "-o", "ovr.yaml"},
			wantOutput:      "ovr.yaml",
			wantPassthrough: []string{"-f", "prod.yml", "show-compose"},
		},
		{
			name:            "long output equals form",
			args:            []string{"show-compose", "--output=ovr.yaml"},
			wantOutput:      "ovr.yaml",
			wantPassthrough: []string{"show-compose"},
		},
		{
			name:            "build flag forwarded",
			args:            []string{"show-compose", "--build"},
			wantOutput:      "",
			wantPassthrough: []string{"show-compose", "--build"},
		},
		{
			name:    "unknown show-compose flag",
			args:    []string{"show-compose", "--wat"},
			wantErr: "unknown show-compose flag",
		},
		{
			name:    "missing output value",
			args:    []string{"show-compose", "-o"},
			wantErr: "requires a value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOutput, gotPassthrough, err := parseShowComposeFlags(tc.args)
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
				t.Fatalf("unexpected error: %v", err)
			}
			if gotOutput != tc.wantOutput {
				t.Fatalf("output = %q, want %q", gotOutput, tc.wantOutput)
			}
			if !reflect.DeepEqual(gotPassthrough, tc.wantPassthrough) {
				t.Fatalf("passthrough = %v, want %v", gotPassthrough, tc.wantPassthrough)
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
	setDockerServicesFactoryForTest(t, ds)

	wantYAML, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", []string{"show-compose"}, true)
	if err != nil {
		t.Fatalf("buildInjectionOverride: %v", err)
	}

	gotStdout, err := captureStdout(t, func() error {
		return runShowCompose("vaka.yaml", []string{"show-compose"}, true)
	})
	if err != nil {
		t.Fatalf("runShowCompose: %v", err)
	}

	if gotStdout != wantYAML {
		t.Fatalf("stdout mismatch\n--- got ---\n%s\n--- want ---\n%s", gotStdout, wantYAML)
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
	setDockerServicesFactoryForTest(t, ds)

	wantYAML, _, err := buildInjectionOverride(context.Background(), ds, "vaka.yaml", []string{"show-compose"}, true)
	if err != nil {
		t.Fatalf("buildInjectionOverride: %v", err)
	}

	outPath := filepath.Join(dir, "override.yaml")
	if err := runShowCompose("vaka.yaml", []string{"show-compose", "-o", outPath}, true); err != nil {
		t.Fatalf("runShowCompose: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}

	if string(got) != wantYAML {
		t.Fatalf("file output mismatch\n--- got ---\n%s\n--- want ---\n%s", string(got), wantYAML)
	}
}
