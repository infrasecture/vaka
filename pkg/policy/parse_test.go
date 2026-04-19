// pkg/policy/parse_test.go
package policy_test

import (
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/policy"
)

const minimalValid = `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        defaultAction: reject
        accept:
          - proto: tcp
            to: [10.0.0.1]
            ports: [443]
`

func TestParseMinimalValid(t *testing.T) {
	p, err := policy.Parse(strings.NewReader(minimalValid))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.APIVersion != "agent.vaka/v1alpha1" {
		t.Errorf("APIVersion = %q, want %q", p.APIVersion, "agent.vaka/v1alpha1")
	}
	if p.Kind != "ServicePolicy" {
		t.Errorf("Kind = %q, want %q", p.Kind, "ServicePolicy")
	}
	svc, ok := p.Services["codex"]
	if !ok {
		t.Fatal("services.codex not found")
	}
	if svc.Network.Egress.DefaultAction != "reject" {
		t.Errorf("defaultAction = %q, want %q", svc.Network.Egress.DefaultAction, "reject")
	}
}

func TestParseUnknownFieldRejected(t *testing.T) {
	input := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        defaultAction: reject
        typo_field: bad
`
	_, err := policy.Parse(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
	if !strings.Contains(err.Error(), "typo_field") {
		t.Errorf("error %q does not mention the unknown field", err.Error())
	}
}

func TestParseDefaultActionDefaultsToReject(t *testing.T) {
	input := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  codex:
    network:
      egress:
        accept:
          - proto: tcp
            to: [10.0.0.1]
            ports: [443]
`
	p, err := policy.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Services["codex"].Network.Egress.DefaultAction != "reject" {
		t.Errorf("defaultAction = %q, want %q (default)",
			p.Services["codex"].Network.Egress.DefaultAction, "reject")
	}
}

func TestParsePortRange(t *testing.T) {
	input := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  svc:
    network:
      egress:
        accept:
          - proto: tcp
            to: [10.0.0.1]
            ports: [443, "8080-8090"]
`
	p, err := policy.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ports := p.Services["svc"].Network.Egress.Accept[0].Ports
	if len(ports) != 2 {
		t.Fatalf("len(ports) = %d, want 2", len(ports))
	}
	if ports[0].Single != 443 {
		t.Errorf("ports[0].Single = %d, want 443", ports[0].Single)
	}
	if !ports[1].IsRange || ports[1].RangeStart != 8080 || ports[1].RangeEnd != 8090 {
		t.Errorf("ports[1] = %+v, want IsRange=true RangeStart=8080 RangeEnd=8090", ports[1])
	}
}

func TestParseSliceSingleService(t *testing.T) {
	p, _ := policy.Parse(strings.NewReader(minimalValid))
	sliced, err := policy.SliceService(p, "codex")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sliced.Services) != 1 {
		t.Errorf("len(services) = %d, want 1", len(sliced.Services))
	}
	if _, ok := sliced.Services["codex"]; !ok {
		t.Error("expected service 'codex' in sliced policy")
	}
}

func TestSliceServiceNotFound(t *testing.T) {
	p, _ := policy.Parse(strings.NewReader(minimalValid))
	_, err := policy.SliceService(p, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing service, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error %q does not mention the service name", err.Error())
	}
}
