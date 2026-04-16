// pkg/policy/types.go
package policy

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServicePolicy is the top-level config document (vaka.yaml).
type ServicePolicy struct {
	APIVersion string                    `yaml:"apiVersion"`
	Kind       string                    `yaml:"kind"`
	Services   map[string]*ServiceConfig `yaml:"services"`
}

// ServiceConfig holds per-service network and runtime policy.
type ServiceConfig struct {
	Network *NetworkConfig `yaml:"network,omitempty"`
	Runtime *RuntimeConfig `yaml:"runtime,omitempty"`
}

// NetworkConfig wraps the egress policy.
type NetworkConfig struct {
	Egress *EgressPolicy `yaml:"egress,omitempty"`
}

// EgressPolicy defines allowed/denied outbound traffic for one service.
type EgressPolicy struct {
	DefaultAction string `yaml:"defaultAction,omitempty"`
	// WithTCPReset controls whether reject emits a TCP RST for TCP traffic.
	// nil (omitted) and true both enable the two-rule tail:
	//   meta l4proto tcp reject with tcp reset
	//   reject with icmpx type admin-prohibited
	// Only valid when defaultAction is "reject" (or empty, which defaults to "reject").
	WithTCPReset  *bool               `yaml:"with_tcp_reset,omitempty"`
	Accept        []Rule              `yaml:"accept,omitempty"`
	Reject        []Rule              `yaml:"reject,omitempty"`
	Drop          []Rule              `yaml:"drop,omitempty"`
	BlockMetadata BlockMetadataConfig `yaml:"block_metadata,omitempty"`
}

// BlockMetadataConfig controls the verdict applied to cloud IMDS endpoints.
// Scalar form:  block_metadata: drop          (accept | drop | reject)
// Mapping form: block_metadata: {action: reject, with_tcp_reset: false}
// Omitting the field disables metadata blocking entirely.
type BlockMetadataConfig struct {
	Action       string `yaml:"action,omitempty"`
	WithTCPReset *bool  `yaml:"with_tcp_reset,omitempty"`
}

// UnmarshalYAML accepts the scalar form ("accept", "drop", "reject") or the
// mapping form ({action: reject, with_tcp_reset: false}). Boolean values
// (true/false) are not valid — omit the field to disable metadata blocking.
func (b *BlockMetadataConfig) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		switch value.Value {
		case "accept", "drop", "reject":
			b.Action = value.Value
		default:
			return fmt.Errorf("block_metadata: unknown value %q (expected accept, drop, or reject)", value.Value)
		}
		return nil
	case yaml.MappingNode:
		type shadow BlockMetadataConfig
		return value.Decode((*shadow)(b))
	}
	return fmt.Errorf("block_metadata: unexpected YAML node kind %v", value.Kind)
}

// MarshalYAML emits the scalar form when WithTCPReset is not set, and the
// mapping form when it is. A zero-value BlockMetadataConfig (Action == "")
// is omitted by the parent struct's omitempty tag.
func (b BlockMetadataConfig) MarshalYAML() (any, error) {
	if b.WithTCPReset != nil {
		return map[string]any{
			"action":        b.Action,
			"with_tcp_reset": *b.WithTCPReset,
		}, nil
	}
	return b.Action, nil
}

// Rule is one entry in an accept/reject/drop list.
// Exactly one of DNS or Proto/To/Ports/Type should be set.
type Rule struct {
	DNS   *DNSRule   `yaml:"dns,omitempty"`
	Proto string     `yaml:"proto,omitempty"`
	To    []string   `yaml:"to,omitempty"`
	Ports []PortSpec `yaml:"ports,omitempty"`
	Type  *ICMPSpec  `yaml:"type,omitempty"`
	// WithTCPReset controls whether this reject rule uses "reject with tcp reset".
	// Only valid in the reject list with proto: tcp.
	// nil (omitted) and true both enable tcp reset; false uses admin-prohibited.
	WithTCPReset *bool `yaml:"with_tcp_reset,omitempty"`
}

// DNSRule is the dns: {} shorthand. Servers overrides resolv.conf if set.
type DNSRule struct {
	Servers []string `yaml:"servers,omitempty"`
}

// PortSpec holds a single port (Single > 0, IsRange == false)
// or a range (IsRange == true, RangeStart, RangeEnd set).
type PortSpec struct {
	Single     int
	RangeStart int
	RangeEnd   int
	IsRange    bool
}

// UnmarshalYAML handles both integer and "N-M" string forms.
func (p *PortSpec) UnmarshalYAML(value *yaml.Node) error {
	// Try integer first.
	var single int
	if err := value.Decode(&single); err == nil {
		if single < 1 || single > 65535 {
			return fmt.Errorf("port %d out of range (1–65535)", single)
		}
		p.Single = single
		return nil
	}
	// Try "N-M" string.
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("port must be an integer or a range string \"N-M\"")
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid port range %q: expected \"N-M\"", s)
	}
	start, err1 := strconv.Atoi(parts[0])
	end, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("invalid port range %q: both values must be integers", s)
	}
	if start < 1 || end > 65535 || start >= end {
		return fmt.Errorf("invalid port range %q: values must be 1–65535 and start < end", s)
	}
	p.RangeStart = start
	p.RangeEnd = end
	p.IsRange = true
	return nil
}

// MarshalYAML serialises back to the canonical form accepted by UnmarshalYAML:
// an integer for a single port, or the string "N-M" for a range.
// Without this, yaml.Marshal emits the struct fields and the round-trip fails.
func (p PortSpec) MarshalYAML() (any, error) {
	if p.IsRange {
		return fmt.Sprintf("%d-%d", p.RangeStart, p.RangeEnd), nil
	}
	return p.Single, nil
}

// NftString returns the nft representation of this port spec.
func (p PortSpec) NftString() string {
	if p.IsRange {
		return fmt.Sprintf("%d-%d", p.RangeStart, p.RangeEnd)
	}
	return strconv.Itoa(p.Single)
}

// ICMPSpec holds an ICMP type as either a named string or an integer.
type ICMPSpec struct {
	Name  string
	Num   int
	IsNum bool
}

// UnmarshalYAML handles both string names and integer type codes.
func (i *ICMPSpec) UnmarshalYAML(value *yaml.Node) error {
	// YAML integers arrive as !!int nodes.
	var n int
	if err := value.Decode(&n); err == nil {
		if n < 0 || n > 255 {
			return fmt.Errorf("ICMP type %d out of range (0–255)", n)
		}
		i.Num = n
		i.IsNum = true
		return nil
	}
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("ICMP type must be a string name or integer 0–255")
	}
	// Numeric string (e.g. type: "8").
	if parsed, err := strconv.Atoi(s); err == nil {
		if parsed < 0 || parsed > 255 {
			return fmt.Errorf("ICMP type %d out of range (0–255)", parsed)
		}
		i.Num = parsed
		i.IsNum = true
		return nil
	}
	i.Name = s
	return nil
}

// MarshalYAML serialises back to the canonical form accepted by UnmarshalYAML:
// an integer for a numeric type code, or a string for a named type.
// Without this, yaml.Marshal emits the struct fields and the round-trip fails.
func (i ICMPSpec) MarshalYAML() (any, error) {
	if i.IsNum {
		return i.Num, nil
	}
	return i.Name, nil
}

// NftString returns the nft-ready type token.
func (i ICMPSpec) NftString() string {
	if i.IsNum {
		return strconv.Itoa(i.Num)
	}
	return i.Name
}

// RuntimeConfig holds capability and identity settings for vaka-init.
type RuntimeConfig struct {
	DropCaps []string     `yaml:"dropCaps,omitempty"`
	RunAs    *RunAsConfig `yaml:"runAs,omitempty"`
}

// RunAsConfig specifies the UID/GID to switch to after firewall setup.
type RunAsConfig struct {
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
}
