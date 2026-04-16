// pkg/nft/generate_test.go
package nft_test

import (
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/nft"
	"vaka.dev/vaka/pkg/policy"
)

func egressWithAccept(rules ...policy.Rule) *policy.EgressPolicy {
	return &policy.EgressPolicy{
		DefaultAction: "reject",
		Accept:        rules,
	}
}

func TestGenerateImplicitInvariantsAlwaysFirst(t *testing.T) {
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "reject"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	estIdx := strings.Index(out, "ct state established,related accept")
	loIdx := strings.Index(out, "oif \"lo\" accept")
	if estIdx < 0 {
		t.Error("established,related rule missing")
	}
	if loIdx < 0 {
		t.Error("oif lo rule missing")
	}
	if estIdx > loIdx {
		t.Error("established,related must come before oif lo")
	}
}

func TestGenerateTCPRuleIPv4(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"192.168.1.10"},
		Ports: []policy.PortSpec{{Single: 443}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := "ip  daddr { 192.168.1.10 } tcp dport { 443 } accept"
	if !strings.Contains(out, want) {
		t.Errorf("output does not contain %q\ngot:\n%s", want, out)
	}
	// Must NOT generate an ip6 rule for an IPv4 address.
	if strings.Contains(out, "ip6 daddr { 192.168.1.10 }") {
		t.Error("should not generate ip6 rule for IPv4 address")
	}
}

func TestGenerateTCPRuleIPv6CIDR(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"2001:db8::/32"},
		Ports: []policy.PortSpec{{Single: 443}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := "ip6 daddr { 2001:db8::/32 } tcp dport { 443 } accept"
	if !strings.Contains(out, want) {
		t.Errorf("output does not contain %q\ngot:\n%s", want, out)
	}
	if strings.Contains(out, "ip  daddr { 2001:db8::/32 }") {
		t.Error("should not generate ip4 rule for IPv6 CIDR")
	}
}

func TestGeneratePortRange(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"10.0.0.1"},
		Ports: []policy.PortSpec{{Single: 443}, {IsRange: true, RangeStart: 8080, RangeEnd: 8090}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "443, 8080-8090") {
		t.Errorf("expected port set with range, got:\n%s", out)
	}
}

func TestGenerateDropRuleBeforeAcceptRule(t *testing.T) {
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Drop:          []policy.Rule{{Proto: "icmp", Type: &policy.ICMPSpec{Name: "echo-request"}}},
		Accept:        []policy.Rule{{Proto: "tcp", To: []string{"10.0.0.1"}, Ports: []policy.PortSpec{{Single: 443}}}},
	}
	out, err := nft.Generate(e)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dropIdx := strings.Index(out, "drop")
	acceptIdx := strings.Index(out, "accept")
	// The first "accept" will be from implicit invariants; find the user accept rule.
	userAcceptIdx := strings.Index(out, "10.0.0.1")
	if dropIdx > userAcceptIdx {
		t.Error("explicit drop rule must appear before user accept rule")
	}
	_ = acceptIdx
}

func TestGenerateBlockMetadata(t *testing.T) {
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		BlockMetadata: true,
	}
	out, err := nft.Generate(e)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, want := range []string{
		"ip  daddr 169.254.169.254/32 drop",
		"ip  daddr 100.100.100.200/32 drop",
		"ip6 daddr fd00:ec2::254/128 drop",
		"ip6 daddr fd20:ce::254/128 drop",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block_metadata: expected %q in output\ngot:\n%s", want, out)
		}
	}
}

func TestGenerateDefaultActionReject(t *testing.T) {
	// Default reject emits two terminal lines: TCP RST for TCP, admin-prohibited for all.
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "reject"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "meta l4proto tcp reject with tcp reset") {
		t.Errorf("expected tcp reset line in default reject output, got:\n%s", out)
	}
	if !strings.Contains(out, "reject with icmpx type admin-prohibited") {
		t.Errorf("expected admin-prohibited line in default reject output, got:\n%s", out)
	}
	if strings.Contains(out, "port-unreachable") {
		t.Errorf("port-unreachable must not appear in default reject output:\n%s", out)
	}
}

func TestGenerateDefaultActionDrop(t *testing.T) {
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "drop"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// The default action line should be just "drop" (not inside a rule).
	if !strings.Contains(out, "\n    drop\n") {
		t.Errorf("expected bare 'drop' verdict line, got:\n%s", out)
	}
}

func TestGenerateDefaultActionAccept(t *testing.T) {
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "accept"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "\n    accept\n") {
		t.Errorf("expected bare 'accept' verdict line, got:\n%s", out)
	}
}

func TestGenerateUnresolvedHostnameComment(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"llm-gateway"},
		Ports: []policy.PortSpec{{Single: 443}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "# unresolved: llm-gateway") {
		t.Errorf("expected unresolved comment, got:\n%s", out)
	}
}

// proto with no ports must emit "meta l4proto <proto>" — not be silently dropped.
func TestGenerateProtoTCPNoPortsEmitsL4ProtoMatch(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"10.0.0.1"},
		// Ports deliberately omitted.
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "meta l4proto tcp") {
		t.Errorf("expected 'meta l4proto tcp' for proto-only TCP rule, got:\n%s", out)
	}
	// Must not silently accept all protocols (bare "accept" without proto match).
	if strings.Contains(out, "daddr { 10.0.0.1 } accept") {
		t.Errorf("rule must not drop the protocol restriction:\n%s", out)
	}
}

// proto: udp with no to: and no ports → bare protocol-only rule.
func TestGenerateProtoUDPNoToNoPortsBareRule(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "udp",
		// To and Ports both omitted.
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "meta l4proto udp accept") {
		t.Errorf("expected 'meta l4proto udp accept' for protocol-only rule, got:\n%s", out)
	}
}

// ICMP rule syntax tests — guard against the double-protocol-keyword bug.
func TestGenerateICMPRuleSyntax(t *testing.T) {
	tests := []struct {
		name    string
		rule    policy.Rule
		wantIn  string
		wantOut string
	}{
		{
			name:   "icmp named type",
			rule:   policy.Rule{Proto: "icmp", Type: &policy.ICMPSpec{Name: "echo-request"}},
			wantIn: "meta l4proto icmp icmp type echo-request drop",
		},
		{
			name:   "icmp numeric type",
			rule:   policy.Rule{Proto: "icmp", Type: &policy.ICMPSpec{Num: 8, IsNum: true}},
			wantIn: "meta l4proto icmp icmp type 8 drop",
		},
		{
			name:   "icmp no type",
			rule:   policy.Rule{Proto: "icmp"},
			wantIn: "meta l4proto icmp drop",
		},
		{
			name:   "icmpv6 named type",
			rule:   policy.Rule{Proto: "icmpv6", Type: &policy.ICMPSpec{Name: "nd-neighbor-solicit"}},
			wantIn: "meta l4proto ipv6-icmp icmpv6 type nd-neighbor-solicit drop",
		},
		{
			name:   "icmpv6 no type",
			rule:   policy.Rule{Proto: "icmpv6"},
			wantIn: "meta l4proto ipv6-icmp drop",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &policy.EgressPolicy{
				DefaultAction: "reject",
				Drop:          []policy.Rule{tc.rule},
			}
			out, err := nft.Generate(e)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if !strings.Contains(out, tc.wantIn) {
				t.Errorf("expected %q in output\ngot:\n%s", tc.wantIn, out)
			}
			// Ensure no duplicate protocol keyword (the original bug).
			for _, bad := range []string{"icmp  icmp", "icmpv6 icmpv6", "l4proto icmpv6"} {
				if strings.Contains(out, bad) {
					t.Errorf("duplicate/wrong protocol keyword %q in output:\n%s", bad, out)
				}
			}
		})
	}
}

// proto+ports path must continue to emit dport set (regression guard).
func TestGenerateProtoAndPortsEmitsDportSet(t *testing.T) {
	out, err := nft.Generate(egressWithAccept(policy.Rule{
		Proto: "tcp",
		To:    []string{"10.0.0.1"},
		Ports: []policy.PortSpec{{Single: 443}, {Single: 80}},
	}))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "tcp dport { 443, 80 } accept") {
		t.Errorf("expected dport set for tcp+ports rule, got:\n%s", out)
	}
	// meta l4proto must NOT appear as part of the accept rule itself —
	// dport already encodes the protocol restriction. The default verdict
	// legitimately emits "meta l4proto tcp reject with tcp reset", so check
	// specifically for the wrongly-generated accept form.
	if strings.Contains(out, "meta l4proto tcp accept") {
		t.Errorf("dport rule should not also emit meta l4proto:\n%s", out)
	}
}

// --- with_tcp_reset tests ---

func TestGenerateDefaultRejectWithTCPResetFalse(t *testing.T) {
	f := false
	out, err := nft.Generate(&policy.EgressPolicy{DefaultAction: "reject", WithTCPReset: &f})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(out, "tcp reset") {
		t.Errorf("tcp reset must not appear when with_tcp_reset: false:\n%s", out)
	}
	if !strings.Contains(out, "reject with icmpx type admin-prohibited") {
		t.Errorf("expected admin-prohibited verdict, got:\n%s", out)
	}
}

func TestGenerateRejectRuleTCPUsesReset(t *testing.T) {
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Reject: []policy.Rule{
			{Proto: "tcp", To: []string{"10.0.0.1"}, Ports: []policy.PortSpec{{Single: 22}}},
		},
	}
	out, err := nft.Generate(e)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "ip  daddr { 10.0.0.1 } tcp dport { 22 } reject with tcp reset") {
		t.Errorf("TCP reject rule must use tcp reset by default:\n%s", out)
	}
}

func TestGenerateRejectRuleTCPResetFalse(t *testing.T) {
	f := false
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Reject: []policy.Rule{
			{Proto: "tcp", To: []string{"10.0.0.1"}, Ports: []policy.PortSpec{{Single: 22}}, WithTCPReset: &f},
		},
	}
	out, err := nft.Generate(e)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "ip  daddr { 10.0.0.1 } tcp dport { 22 } reject with icmpx type admin-prohibited") {
		t.Errorf("expected admin-prohibited for tcp rule with with_tcp_reset: false:\n%s", out)
	}
	if strings.Contains(out, "tcp dport { 22 } reject with tcp reset") {
		t.Errorf("tcp reset must not appear when with_tcp_reset: false on rule:\n%s", out)
	}
}

func TestGenerateRejectRuleUDPUsesAdminProhibited(t *testing.T) {
	e := &policy.EgressPolicy{
		DefaultAction: "reject",
		Reject: []policy.Rule{
			{Proto: "udp", To: []string{"8.8.8.8"}, Ports: []policy.PortSpec{{Single: 53}}},
		},
	}
	out, err := nft.Generate(e)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// UDP never uses tcp reset.
	if strings.Contains(out, "udp dport { 53 } reject with tcp reset") {
		t.Errorf("UDP rule must not produce tcp reset:\n%s", out)
	}
	if !strings.Contains(out, "udp dport { 53 } reject with icmpx type admin-prohibited") {
		t.Errorf("expected admin-prohibited for UDP rule:\n%s", out)
	}
}
