// cmd/vaka-init/main_test.go
//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/policy"
)

// encodePolicy marshals p to YAML and base64-encodes it, replicating exactly
// what vaka up does before setting the VAKA_<SERVICE>_CONF env var.
func encodePolicy(t *testing.T, p *policy.ServicePolicy) string {
	t.Helper()
	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// writeTmp writes content to a temp file and returns its path.
// The file is removed when the test ends.
func writeTmp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "vaka-secret-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestReadPolicy_roundtrip(t *testing.T) {
	want := &policy.ServicePolicy{
		APIVersion: "agent.vaka/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"svc": {
				Network: &policy.NetworkConfig{
					Egress: &policy.EgressPolicy{
						DefaultAction: "reject",
					},
				},
			},
		},
	}

	path := writeTmp(t, encodePolicy(t, want))

	got, err := readPolicy(path)
	if err != nil {
		t.Fatalf("readPolicy: %v", err)
	}

	if got.APIVersion != want.APIVersion {
		t.Errorf("apiVersion = %q, want %q", got.APIVersion, want.APIVersion)
	}
	svc, ok := got.Services["svc"]
	if !ok {
		t.Fatal("service 'svc' not found in parsed policy")
	}
	if svc.Network.Egress.DefaultAction != "reject" {
		t.Errorf("defaultAction = %q, want %q", svc.Network.Egress.DefaultAction, "reject")
	}
}

func TestReadPolicy_trailingNewline(t *testing.T) {
	// Docker compose appends a newline when writing env-var secrets.
	// TrimSpace must strip it before base64 decoding.
	p := &policy.ServicePolicy{
		APIVersion: "agent.vaka/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"svc": {
				Network: &policy.NetworkConfig{
					Egress: &policy.EgressPolicy{DefaultAction: "reject"},
				},
			},
		},
	}
	path := writeTmp(t, encodePolicy(t, p)+"\n")

	if _, err := readPolicy(path); err != nil {
		t.Fatalf("readPolicy with trailing newline: %v", err)
	}
}

func TestReadPolicy_notBase64(t *testing.T) {
	// Raw YAML (not base64-encoded) must be rejected — this would be the
	// behaviour if vaka-init were pointed at the old unencoded secret format.
	path := writeTmp(t, "apiVersion: agent.vaka/v1alpha1\nkind: ServicePolicy\n")

	if _, err := readPolicy(path); err == nil {
		t.Fatal("expected error for non-base64 content, got nil")
	}
}

func TestReadPolicy_missingFile(t *testing.T) {
	_, err := readPolicy("/nonexistent/vaka.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestParseCaps_knownNames(t *testing.T) {
	// parseCaps must resolve both short-form and CAP_-prefixed names.
	// The gocapability library returns names without the cap_ prefix (e.g.
	// "net_admin"), so the normalization must strip it before comparing.
	tests := []struct {
		input string
	}{
		{"NET_ADMIN"},
		{"net_admin"},
		{"CAP_NET_ADMIN"},
		{"cap_net_admin"},
		{"NET_RAW"},
		{"SETPCAP"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			caps, err := parseCaps([]string{tc.input})
			if err != nil {
				t.Errorf("parseCaps(%q) = error %v, want success", tc.input, err)
			}
			if len(caps) != 1 {
				t.Errorf("parseCaps(%q) returned %d caps, want 1", tc.input, len(caps))
			}
		})
	}
}

func TestParseCaps_unknownName(t *testing.T) {
	_, err := parseCaps([]string{"NOT_A_CAP"})
	if err == nil {
		t.Error("parseCaps(NOT_A_CAP) expected error, got nil")
	}
}

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		policy  string
		self    string
		wantErr bool
	}{
		{"v0.1.2", "v0.1.0", false},        // same major.minor, patch differs → ok
		{"v0.1.2", "v0.1.2", false},        // exact match → ok
		{"v0.1.0", "v0.2.0", true},         // minor mismatch → error
		{"v0.2.0", "v0.1.0", true},         // minor mismatch → error
		{"v1.0.0", "v0.1.0", true},         // major mismatch → error
		{"4178cc0", "4178cc0", false},      // git hash exact match → ok
		{"4178cc0", "4178cc0-dirty", true}, // git hash mismatch → error
		{"4178cc0-dirty", "4178cc0", true}, // git hash mismatch → error
		{"", "v0.1.0", true},               // missing → error
	}
	for _, tc := range tests {
		err := checkVersion(tc.policy, tc.self)
		if tc.wantErr && err == nil {
			t.Errorf("checkVersion(%q, %q): expected error, got nil", tc.policy, tc.self)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("checkVersion(%q, %q): unexpected error: %v", tc.policy, tc.self, err)
		}
	}
}

func TestNoArgExitsZero(t *testing.T) {
	// Subprocess trick: re-run this test binary as vaka-init with no "--".
	// When BE_VAKA_INIT=1 the subprocess calls main(); os.Args[1] will be
	// `-test.run=…` (not "--"), so main() hits the bad-args branch. Parent
	// asserts exit code 0 — the same lenient-on-misconfiguration contract
	// documented in the design. The true no-args (standalone) branch is
	// covered by the logic review + integration (`vaka up`) since the
	// subprocess trick can't produce a len(os.Args) == 1 invocation.
	if os.Getenv("BE_VAKA_INIT") == "1" {
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestNoArgExitsZero")
	cmd.Env = append(os.Environ(), "BE_VAKA_INIT=1")
	if err := cmd.Run(); err != nil {
		t.Errorf("vaka-init with no harness args: expected exit 0, got: %v", err)
	}
}

func TestBadArgsPrintsUsage(t *testing.T) {
	// When invoked with args that are not "--", vaka-init should print the
	// usage message to stderr and exit 0 (lenient on misconfiguration).
	if os.Getenv("BE_VAKA_INIT") == "1" {
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestBadArgsPrintsUsage", "notdashdash")
	cmd.Env = append(os.Environ(), "BE_VAKA_INIT=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("vaka-init with bad args: expected exit 0, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "usage: vaka-init -- <entrypoint>") {
		t.Errorf("vaka-init with bad args: expected usage message on stderr, got %q", stderr.String())
	}
}
