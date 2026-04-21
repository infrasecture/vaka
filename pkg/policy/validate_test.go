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
apiVersion: agent.vaka/v1alpha1
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
apiVersion: agent.vaka/v1alpha1
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
		APIVersion: "agent.vaka/v1alpha1",
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
apiVersion: agent.vaka/v1alpha1
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
apiVersion: agent.vaka/v1alpha1
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
apiVersion: agent.vaka/v1alpha1
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
apiVersion: agent.vaka/v1alpha1
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
apiVersion: agent.vaka/v1alpha1
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
apiVersion: agent.vaka/v1alpha1
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
		APIVersion: "agent.vaka/v1alpha1",
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
apiVersion: agent.vaka/v1alpha1
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
apiVersion: agent.vaka/v1alpha1
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

func TestValidateBlockMetadata(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		errFrag string
	}{
		{
			name: "drop is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        block_metadata: drop
`,
		},
		{
			name: "accept is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        block_metadata: accept
`,
		},
		{
			name: "reject is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        block_metadata: reject
`,
		},
		{
			name: "mapping form reject with with_tcp_reset false is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        block_metadata:
          action: reject
          with_tcp_reset: false
`,
		},
		{
			name:    "mapping form drop with with_tcp_reset is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        block_metadata:
          action: drop
          with_tcp_reset: true
`,
		},
		{
			name:    "mapping form accept with with_tcp_reset is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        block_metadata:
          action: accept
          with_tcp_reset: false
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mustParse(t, tc.yaml)
			errs := policy.Validate(p, nil)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatal("expected validation error, got none")
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Error(), tc.errFrag) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errFrag, errs)
				}
			} else {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got: %v", errs)
				}
			}
		})
	}
}

func TestValidateWithTCPReset(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		errFrag string // substring expected in the error message
	}{
		{
			name: "with_tcp_reset true on defaultAction reject is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        with_tcp_reset: true
`,
		},
		{
			name: "with_tcp_reset false on defaultAction reject is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        with_tcp_reset: false
`,
		},
		{
			name:    "with_tcp_reset on defaultAction accept is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: accept
        with_tcp_reset: true
`,
		},
		{
			name:    "with_tcp_reset on defaultAction drop is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: drop
        with_tcp_reset: true
`,
		},
		{
			name: "rule with_tcp_reset true in reject list with proto tcp is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        reject:
          - proto: tcp
            to: [10.0.0.1]
            ports: [22]
            with_tcp_reset: true
`,
		},
		{
			name: "rule with_tcp_reset false in reject list with proto tcp is valid",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        reject:
          - proto: tcp
            to: [10.0.0.1]
            ports: [22]
            with_tcp_reset: false
`,
		},
		{
			name:    "rule with_tcp_reset in reject list with proto udp is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        reject:
          - proto: udp
            to: [8.8.8.8]
            ports: [53]
            with_tcp_reset: true
`,
		},
		{
			name:    "rule with_tcp_reset in accept list is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        accept:
          - proto: tcp
            to: [10.0.0.1]
            ports: [443]
            with_tcp_reset: true
`,
		},
		{
			name:    "rule with_tcp_reset in drop list is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        drop:
          - proto: tcp
            to: [10.0.0.1]
            ports: [22]
            with_tcp_reset: true
`,
		},
		{
			name:    "rule with_tcp_reset in reject list with no proto is invalid",
			wantErr: true,
			errFrag: "with_tcp_reset",
			yaml: `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        reject:
          - to: [10.0.0.1]
            with_tcp_reset: true
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := mustParse(t, tc.yaml)
			errs := policy.Validate(p, nil)
			if tc.wantErr {
				if len(errs) == 0 {
					t.Fatal("expected validation error, got none")
				}
				found := false
				for _, e := range errs {
					if strings.Contains(e.Error(), tc.errFrag) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected error containing %q, got: %v", tc.errFrag, errs)
				}
			} else {
				if len(errs) != 0 {
					t.Fatalf("expected no errors, got: %v", errs)
				}
			}
		})
	}
}

func TestValidateRejectsOldAPIVersion(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for old apiVersion vaka.dev/v1alpha1, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "apiVersion") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning apiVersion, got: %v", errs)
	}
}

func TestValidateVakaVersionForbiddenInUserYAML(t *testing.T) {
	p := mustParse(t, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
vakaVersion: v0.1.0
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for user-supplied vakaVersion, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "vakaVersion") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning vakaVersion, got: %v", errs)
	}
}

func TestValidateRejectsServiceUserInHostYAML(t *testing.T) {
	p := mustParse(t, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    user: "1000:1000"
`)
	errs := policy.ValidateHost(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for user-supplied services.<name>.user in host vaka.yaml, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), ".user") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning services.<name>.user, got: %v", errs)
	}
}

func TestValidateInjectedAllowsServiceUserAndRequiresVakaVersion(t *testing.T) {
	ok := mustParse(t, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
vakaVersion: v0.1.2
services:
  s:
    user: "1000:1000"
`)
	if errs := policy.ValidateInjected(ok); len(errs) != 0 {
		t.Fatalf("expected injected policy with generated user to validate, got: %v", errs)
	}

	missingVersion := mustParse(t, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    user: "1000:1000"
`)
	errs := policy.ValidateInjected(missingVersion)
	if len(errs) == 0 {
		t.Fatal("expected error for missing vakaVersion in injected policy, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "vakaVersion") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning vakaVersion, got: %v", errs)
	}
}
