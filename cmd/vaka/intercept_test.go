// cmd/vaka/intercept_test.go
package main

import (
	"strings"
	"testing"
)

func TestClassifySubcmd(t *testing.T) {
	tests := []struct {
		subcmd string
		want   subcmdPath
	}{
		{"up", pathFull},
		{"run", pathFull},
		{"create", pathFull},
		{"down", pathLifecycle},
		{"stop", pathLifecycle},
		{"kill", pathLifecycle},
		{"rm", pathLifecycle},
		{"validate", pathCobra},
		{"show", pathCobra},
		{"version", pathCobra},
		{"", pathCobra},
		{"logs", pathPassthrough},
		{"ps", pathPassthrough},
		{"exec", pathPassthrough},
		{"pull", pathPassthrough},
	}
	for _, tc := range tests {
		if got := classifySubcmd(tc.subcmd); got != tc.want {
			t.Errorf("classifySubcmd(%q) = %v, want %v", tc.subcmd, got, tc.want)
		}
	}
}

func TestLifecycleOverrideYAMLPassthrough(t *testing.T) {
	yaml, err := lifecycleOverrideYAML(true, "emsi/vaka-init:v0.1.0")
	if err != nil {
		t.Fatalf("passthrough: unexpected error: %v", err)
	}
	if yaml != "" {
		t.Errorf("passthrough: expected empty string, got:\n%s", yaml)
	}
}

func TestLifecycleOverrideYAMLInjectsContainer(t *testing.T) {
	yaml, err := lifecycleOverrideYAML(false, "emsi/vaka-init:v0.1.0")
	if err != nil {
		t.Fatalf("injection: unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "__vaka-init") {
		t.Errorf("injection: expected __vaka-init in YAML, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "emsi/vaka-init:v0.1.0") {
		t.Errorf("injection: expected image ref in YAML, got:\n%s", yaml)
	}
}

func TestExtractVakaFlagsBool(t *testing.T) {
	// --vaka-init-present is a boolean flag: no value token consumed.
	flags, rest := extractVakaFlags([]string{"up", "--vaka-init-present", "--remove-orphans"})
	if flags["--vaka-init-present"] != "true" {
		t.Errorf("expected --vaka-init-present=true, got %q", flags["--vaka-init-present"])
	}
	want := []string{"up", "--remove-orphans"}
	if strings.Join(rest, " ") != strings.Join(want, " ") {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}
