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
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  agent:
    network:
      egress:
        defaultAction: reject
        block_metadata: drop
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

func TestBlockMetadataScalarRoundTrip(t *testing.T) {
	input := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        defaultAction: reject
        block_metadata: drop
`
	p, err := policy.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p2, err := policy.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("re-parse after marshal: %v\nYAML:\n%s", err, raw)
	}
	bm := p2.Services["s"].Network.Egress.BlockMetadata
	if bm.Action != "drop" {
		t.Errorf("BlockMetadata.Action = %q, want \"drop\"", bm.Action)
	}
	if bm.WithTCPReset != nil {
		t.Errorf("BlockMetadata.WithTCPReset = %v, want nil", bm.WithTCPReset)
	}
}

func TestBlockMetadataMappingRoundTrip(t *testing.T) {
	input := `
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
`
	p, err := policy.Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, err := yaml.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p2, err := policy.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("re-parse after marshal: %v\nYAML:\n%s", err, raw)
	}
	bm := p2.Services["s"].Network.Egress.BlockMetadata
	if bm.Action != "reject" {
		t.Errorf("BlockMetadata.Action = %q, want \"reject\"", bm.Action)
	}
	if bm.WithTCPReset == nil || *bm.WithTCPReset != false {
		t.Errorf("BlockMetadata.WithTCPReset = %v, want *false", bm.WithTCPReset)
	}
}

func TestBlockMetadataBoolParseError(t *testing.T) {
	for _, input := range []string{
		`
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        block_metadata: true
`,
		`
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        block_metadata: false
`,
	} {
		_, err := policy.Parse(strings.NewReader(input))
		if err == nil {
			t.Errorf("expected parse error for bool block_metadata in:\n%s", input)
		}
	}
}

func TestBlockMetadataMappingUnknownKeyIsError(t *testing.T) {
	input := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        block_metadata:
          action: reject
          with_tcp_rst: false
`
	_, err := policy.Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected parse error for unknown key with_tcp_rst, got nil")
	}
}

func TestBlockMetadataMappingDuplicateKeyIsError(t *testing.T) {
	input := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        block_metadata:
          action: reject
          action: drop
`
	_, err := policy.Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected parse error for duplicate action key, got nil")
	}
}

func TestBlockMetadataMappingMissingActionIsError(t *testing.T) {
	input := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  s:
    network:
      egress:
        block_metadata:
          with_tcp_reset: false
`
	_, err := policy.Parse(strings.NewReader(input))
	if err == nil {
		t.Error("expected parse error for mapping form without action, got nil")
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
