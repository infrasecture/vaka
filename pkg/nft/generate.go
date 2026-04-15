// pkg/nft/generate.go
package nft

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"strings"
	"text/template"

	"vaka.dev/vaka/pkg/policy"
)

//go:embed templates/egress.nft.tmpl
var egressTmpl string

var tmpl = template.Must(template.New("egress").Parse(egressTmpl))

// metadataRanges are the cloud instance metadata endpoints to block.
var metadataRanges = []string{
	"ip  daddr 169.254.0.0/16",
	"ip  daddr 100.100.100.200/32",
	"ip6 daddr fd00:ec2::254/128",
}

// Generate renders the nft ruleset for e.
// If a to: entry is a hostname (not IP/CIDR), it is rendered as a comment
// with a stub — suitable for vaka show output. vaka-init always passes
// pre-resolved policies.
func Generate(e *policy.EgressPolicy) (string, error) {
	data := RulesetData{
		BlockMetadata:  e.BlockMetadata,
		MetadataRanges: metadataRanges,
		DropRules:      expandRules(e.Drop, "drop"),
		RejectRules:    expandRules(e.Reject, "reject"),
		AcceptRules:    expandRules(e.Accept, "accept"),
		DefaultVerdict: defaultVerdict(e.DefaultAction),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render nft template: %w", err)
	}
	return buf.String(), nil
}

// defaultVerdict returns the nft terminal verdict for the default action.
func defaultVerdict(action string) string {
	switch action {
	case "drop":
		return "drop"
	case "accept":
		return "accept"
	default: // "reject" and the defaulted value
		return "reject with icmpx type port-unreachable"
	}
}

// expandRules converts a list of policy Rules into pre-rendered nft rule strings.
func expandRules(rules []policy.Rule, verdict string) []string {
	var out []string
	for _, r := range rules {
		out = append(out, expandRule(r, verdict)...)
	}
	return out
}

// expandRule converts one Rule into one or more nft rule strings.
func expandRule(r policy.Rule, verdict string) []string {
	if r.DNS != nil {
		// DNS rules are expanded by resolve.go before reaching here;
		// a dns: {} that slipped through produces a stub comment.
		return []string{"# dns: {} — not yet resolved"}
	}

	// ICMP rules: generate one rule per applicable family.
	if r.Proto == "icmp" || r.Proto == "icmpv6" {
		return expandICMPRule(r, verdict)
	}

	// Build the combined protocol+port clause.
	// proto=tcp|udp + ports  → "tcp dport { 443 } "
	// proto=tcp|udp, no ports → "meta l4proto tcp " (bare nft protocol matcher)
	// proto empty             → "" (no protocol restriction; validation rejects ports+no-proto)
	protoAndPortClause := ""
	if r.Proto != "" {
		if len(r.Ports) > 0 {
			protoAndPortClause = r.Proto + " dport { " + portList(r.Ports) + " } "
		} else {
			protoAndPortClause = "meta l4proto " + r.Proto + " "
		}
	}

	if len(r.To) == 0 {
		// No destination constraint — bare protocol or verdict-only rule.
		return []string{protoAndPortClause + verdict}
	}

	// Split To entries by IP family.
	var v4, v6, unresolved []string
	for _, addr := range r.To {
		switch classifyAddr(addr) {
		case "ipv4":
			v4 = append(v4, addr)
		case "ipv6":
			v6 = append(v6, addr)
		default:
			unresolved = append(unresolved, addr)
		}
	}

	var lines []string

	if len(unresolved) > 0 {
		for _, name := range unresolved {
			lines = append(lines, fmt.Sprintf("# unresolved: %s", name))
		}
	}

	if len(v4) > 0 {
		line := fmt.Sprintf("ip  daddr { %s } ", strings.Join(v4, ", "))
		line += protoAndPortClause + verdict
		lines = append(lines, line)
	}
	if len(v6) > 0 {
		line := fmt.Sprintf("ip6 daddr { %s } ", strings.Join(v6, ", "))
		line += protoAndPortClause + verdict
		lines = append(lines, line)
	}

	return lines
}

// expandICMPRule handles proto: icmp and proto: icmpv6 rules.
func expandICMPRule(r policy.Rule, verdict string) []string {
	typeClause := ""
	if r.Type != nil {
		typeClause = fmt.Sprintf("icmp type %s ", r.Type.NftString())
		if r.Proto == "icmpv6" {
			typeClause = fmt.Sprintf("icmpv6 type %s ", r.Type.NftString())
		}
	}

	if r.Proto == "icmp" {
		return []string{fmt.Sprintf("meta l4proto icmp  icmp   %s%s", typeClause, verdict)}
	}
	if r.Proto == "icmpv6" {
		return []string{fmt.Sprintf("meta l4proto icmpv6 icmpv6 %s%s", typeClause, verdict)}
	}
	// proto omitted — emit both families.
	return []string{
		fmt.Sprintf("meta l4proto icmp  icmp   %s%s", typeClause, verdict),
		fmt.Sprintf("meta l4proto icmpv6 icmpv6 %s%s", typeClause, verdict),
	}
}

// portList renders a list of PortSpec into a comma-separated nft set string.
func portList(ports []policy.PortSpec) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = p.NftString()
	}
	return strings.Join(parts, ", ")
}

// classifyAddr returns "ipv4", "ipv6", or "hostname".
func classifyAddr(s string) string {
	// Bare IP?
	if ip := net.ParseIP(s); ip != nil {
		if ip.To4() != nil {
			return "ipv4"
		}
		return "ipv6"
	}
	// CIDR?
	if _, ipNet, err := net.ParseCIDR(s); err == nil {
		if ipNet.IP.To4() != nil {
			return "ipv4"
		}
		return "ipv6"
	}
	return "hostname"
}
