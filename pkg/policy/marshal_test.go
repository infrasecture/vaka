// pkg/policy/marshal_test.go
package policy_test

// Round-trip tests: Parse → yaml.Marshal → Parse must reproduce the same
// values. Without MarshalYAML on PortSpec and ICMPSpec the intermediate
// YAML contains raw struct fields that UnmarshalYAML cannot re-parse.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/policy"
)

const roundTripInput = `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  agent:
    network:
      egress:
        defaultAction: reject
        block_metadata: true
        accept:
          - dns: {}
          - proto: tcp
            to: [llm-gateway, 10.20.0.0/16]
            ports: [443, 80]
          - proto: tcp
            ports: ["8080-8090"]
        drop:
          - proto: icmp
            type: echo-request
          - proto: icmp
            type: 8
`

func TestPortSpecMarshalRoundTrip(t *testing.T) {
	p, err := policy.Parse(strings.NewReader(roundTripInput))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	p2, err := policy.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("re-parse after marshal: %v\nmarshalled YAML:\n%s", err, raw)
	}

	egress := p2.Services["agent"].Network.Egress

	// Verify single-port round-trip.
	tcpRule := egress.Accept[1]
	if len(tcpRule.Ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(tcpRule.Ports))
	}
	if tcpRule.Ports[0].Single != 443 {
		t.Errorf("port[0] = %d, want 443", tcpRule.Ports[0].Single)
	}
	if tcpRule.Ports[1].Single != 80 {
		t.Errorf("port[1] = %d, want 80", tcpRule.Ports[1].Single)
	}

	// Verify range port round-trip.
	rangeRule := egress.Accept[2]
	if !rangeRule.Ports[0].IsRange {
		t.Errorf("expected IsRange=true for 8080-8090")
	}
	if rangeRule.Ports[0].RangeStart != 8080 || rangeRule.Ports[0].RangeEnd != 8090 {
		t.Errorf("range = %d-%d, want 8080-8090", rangeRule.Ports[0].RangeStart, rangeRule.Ports[0].RangeEnd)
	}
}

func TestICMPSpecMarshalRoundTrip(t *testing.T) {
	p, err := policy.Parse(strings.NewReader(roundTripInput))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	p2, err := policy.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("re-parse after marshal: %v\nmarshalled YAML:\n%s", err, raw)
	}

	drop := p2.Services["agent"].Network.Egress.Drop

	// Named ICMP type round-trip.
	if drop[0].Type == nil || drop[0].Type.Name != "echo-request" {
		t.Errorf("drop[0] ICMP type = %+v, want name=echo-request", drop[0].Type)
	}

	// Numeric ICMP type round-trip.
	if drop[1].Type == nil || !drop[1].Type.IsNum || drop[1].Type.Num != 8 {
		t.Errorf("drop[1] ICMP type = %+v, want num=8", drop[1].Type)
	}
}
