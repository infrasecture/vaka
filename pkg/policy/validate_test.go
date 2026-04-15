// pkg/policy/validate_test.go
package policy_test

import (
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/policy"
)

func mustParse(t *testing.T, s string) *policy.ServicePolicy {
	t.Helper()
	p, err := policy.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestValidateAPIVersion(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v2
kind: ServicePolicy
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for wrong apiVersion")
	}
	if !strings.Contains(errs[0].Error(), "apiVersion") {
		t.Errorf("error %q does not mention apiVersion", errs[0])
	}
}

func TestValidateKind(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: BadKind
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "kind") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected error mentioning 'kind', got: %v", errs)
	}
}

func TestValidateUnknownProto(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        accept:
          - proto: udpp
            to: [10.0.0.1]
            ports: [53]
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for unknown proto")
	}
	if !strings.Contains(errs[0].Error(), "udpp") {
		t.Errorf("error %q does not mention 'udpp'", errs[0])
	}
}

func TestValidatePortOutOfRange(t *testing.T) {
	// PortSpec.UnmarshalYAML catches this at parse time, but validate checks it too.
	p := &policy.ServicePolicy{
		APIVersion: "vaka.dev/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"s": {Network: &policy.NetworkConfig{Egress: &policy.EgressPolicy{
				DefaultAction: "reject",
				Accept: []policy.Rule{{
					Proto: "tcp",
					To:    []string{"10.0.0.1"},
					Ports: []policy.PortSpec{{Single: 0}}, // 0 is invalid
				}},
			}}},
		},
	}
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for port 0")
	}
}

func TestValidateNetworkModeHost(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
`)
	compose := map[string]string{"s": "host"} // service → network_mode
	errs := policy.Validate(p, compose)
	if len(errs) == 0 {
		t.Fatal("expected error for network_mode: host")
	}
	if !strings.Contains(errs[0].Error(), "network_mode: host") {
		t.Errorf("error %q does not mention network_mode: host", errs[0])
	}
}

func TestValidateServiceNotInCompose(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  ghost:
    network:
      egress:
        defaultAction: reject
`)
	// networkModes represents the compose file — "ghost" is absent.
	errs := policy.Validate(p, map[string]string{"other": ""})
	if len(errs) == 0 {
		t.Fatal("expected error for service not in docker-compose.yaml")
	}
	if !strings.Contains(errs[0].Error(), "ghost") {
		t.Errorf("error %q does not mention service name 'ghost'", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "docker-compose.yaml") {
		t.Errorf("error %q does not mention docker-compose.yaml", errs[0])
	}
}

func TestValidateInvalidServiceName(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  "My Service!!":
    network:
      egress:
        defaultAction: reject
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for invalid service name")
	}
	if !strings.Contains(errs[0].Error(), "invalid service name") {
		t.Errorf("error %q does not mention 'invalid service name'", errs[0])
	}
}

func TestValidateValidPolicyPassesClean(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        defaultAction: reject
        accept:
          - dns: {}
          - proto: tcp
            to: [10.0.0.1, 10.20.0.0/16]
            ports: [443]
`)
	errs := policy.Validate(p, map[string]string{"codex": ""})
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
}

func TestValidateDefaultActionAcceptIsAllowed(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: accept
`)
	// accept is valid (with a warning printed by the CLI) — validate must NOT error.
	errs := policy.Validate(p, nil)
	if len(errs) != 0 {
		t.Errorf("expected no errors for defaultAction: accept, got: %v", errs)
	}
}

func TestValidateInvalidHostname(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        accept:
          - proto: tcp
            to: ["not a valid hostname!!"]
            ports: [443]
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for invalid hostname")
	}
}

func TestValidatePortsWithoutProtoIsError(t *testing.T) {
	p := &policy.ServicePolicy{
		APIVersion: "vaka.dev/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"s": {Network: &policy.NetworkConfig{Egress: &policy.EgressPolicy{
				DefaultAction: "reject",
				Accept: []policy.Rule{{
					// No proto — ports alone are ambiguous (TCP? UDP?).
					To:    []string{"10.0.0.1"},
					Ports: []policy.PortSpec{{Single: 443}},
				}},
			}}},
		},
	}
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for ports without proto")
	}
	if !strings.Contains(errs[0].Error(), "proto") {
		t.Errorf("error %q does not mention 'proto'", errs[0])
	}
}

func TestValidateDropCapsUnknownNameIsError(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    runtime:
      dropCaps: [NET_ADMON]
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for unknown capability name")
	}
	if !strings.Contains(errs[0].Error(), "NET_ADMON") {
		t.Errorf("error %q does not mention the bad capability name", errs[0])
	}
}

func TestValidateDropCapsCapPrefixAcceptedAndStripped(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s:
    runtime:
      dropCaps: [CAP_NET_ADMIN, SYS_PTRACE]
`)
	errs := policy.Validate(p, nil)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
	// After validation, CAP_ prefix must be stripped.
	svc := p.Services["s"]
	if svc.Runtime.DropCaps[0] != "NET_ADMIN" {
		t.Errorf("expected CAP_ prefix stripped, got %q", svc.Runtime.DropCaps[0])
	}
	if svc.Runtime.DropCaps[1] != "SYS_PTRACE" {
		t.Errorf("expected SYS_PTRACE unchanged, got %q", svc.Runtime.DropCaps[1])
	}
}
