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
// Sources: AWS, GCP, Azure, DigitalOcean, Hetzner, OCI, Linode all use
// 169.254.169.254/32. Alibaba Cloud uses 100.100.100.200/32. AWS and GCP
// expose IPv6 IMDS endpoints on Nitro/IPv6-only instances.
var metadataRanges = []string{
	"ip  daddr 169.254.169.254/32", // AWS, GCP, Azure, DO, Hetzner, OCI, Linode
	"ip  daddr 100.100.100.200/32", // Alibaba Cloud
	"ip6 daddr fd00:ec2::254/128",  // AWS IPv6 IMDS (Nitro instances)
	"ip6 daddr fd20:ce::254/128",   // GCP IPv6 IMDS (IPv6-only instances)
}

// expandMetadataRules pre-renders nft rule strings for IMDS endpoints.
// Returns nil when cfg is disabled (Action == "").
func expandMetadataRules(cfg policy.BlockMetadataConfig) []string {
	if cfg.Action == "" {
		return nil
	}
	switch cfg.Action {
	case "accept", "drop":
		rules := make([]string, len(metadataRanges))
		for i, r := range metadataRanges {
			rules[i] = r + " " + cfg.Action
		}
		return rules
	case "reject":
		var rules []string
		for _, r := range metadataRanges {
			if withTCPReset(cfg.WithTCPReset) {
				rules = append(rules, r+" meta l4proto tcp reject with tcp reset")
			}
			rules = append(rules, r+" reject with icmpx type admin-prohibited")
		}
		return rules
	}
	return nil
}

// Generate renders the nft ruleset for e.
// If a to: entry is a hostname (not IP/CIDR), it is rendered as a comment
// with a stub — suitable for vaka show output. vaka-init always passes
// pre-resolved policies.
func Generate(e *policy.EgressPolicy) (string, error) {
	data := RulesetData{
		MetadataRules:       expandMetadataRules(e.BlockMetadata),
		DropRules:           expandRules(e.Drop, "drop"),
		RejectRules:         expandRules(e.Reject, "reject"),
		AcceptRules:         expandRules(e.Accept, "accept"),
		DefaultVerdictLines: defaultVerdictLines(e.DefaultAction, withTCPReset(e.WithTCPReset)),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render nft template: %w", err)
	}
	return buf.String(), nil
}

// withTCPReset returns true when b is nil (default) or explicitly true.
func withTCPReset(b *bool) bool { return b == nil || *b }

// defaultVerdictLines returns the terminal verdict lines for the default action.
// When action is "reject" and reset is true (the default), two lines are emitted:
// TCP connections receive an in-protocol RST; other protocols receive the
// semantically correct ICMP admin-prohibited.
func defaultVerdictLines(action string, reset bool) []string {
	switch action {
	case "drop":
		return []string{"drop"}
	case "accept":
		return []string{"accept"}
	default: // "reject" and the empty default
		if reset {
			return []string{
				"meta l4proto tcp reject with tcp reset",
				"reject with icmpx type admin-prohibited",
			}
		}
		return []string{"reject with icmpx type admin-prohibited"}
	}
}

// rejectVerdict returns the nft verdict for a rule in the reject list.
// TCP rules use "reject with tcp reset" by default; all other protocols use
// "reject with icmpx type admin-prohibited".
func rejectVerdict(r policy.Rule) string {
	if r.Proto == "tcp" && withTCPReset(r.WithTCPReset) {
		return "reject with tcp reset"
	}
	return "reject with icmpx type admin-prohibited"
}

// expandRules converts a list of policy Rules into pre-rendered nft rule strings.
func expandRules(rules []policy.Rule, verdict string) []string {
	var out []string
	for _, r := range rules {
		v := verdict
		if verdict == "reject" {
			v = rejectVerdict(r)
		}
		out = append(out, expandRule(r, v)...)
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
		return []string{fmt.Sprintf("meta l4proto icmp %s%s", typeClause, verdict)}
	}
	// r.Proto == "icmpv6" — the only other value that reaches this function.
	// nft uses "ipv6-icmp" as the meta l4proto keyword; "icmpv6" is the match keyword.
	return []string{fmt.Sprintf("meta l4proto ipv6-icmp %s%s", typeClause, verdict)}
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
