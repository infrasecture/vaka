// pkg/nft/resolve.go
package nft

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"vaka.dev/vaka/pkg/policy"
)

// ParseResolvConf extracts nameserver addresses from a resolv.conf reader.
// Returns an error if no nameservers are found.
func ParseResolvConf(r io.Reader) ([]string, error) {
	var servers []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "nameserver") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		servers = append(servers, fields[1])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read resolv.conf: %w", err)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("resolv.conf contains no nameserver entries")
	}
	return servers, nil
}

// ExpandDNS returns pre-rendered nft accept rule strings for UDP/53 and
// TCP/53 to each of the given nameserver addresses.
func ExpandDNS(servers []string) ([]string, error) {
	var rules []string
	for _, srv := range servers {
		family, err := addrFamily(srv)
		if err != nil {
			return nil, fmt.Errorf("invalid nameserver address %q: %w", srv, err)
		}
		for _, proto := range []string{"udp", "tcp"} {
			rules = append(rules,
				fmt.Sprintf("%s daddr { %s } %s dport { 53 } accept", family, srv, proto),
			)
		}
	}
	return rules, nil
}

// addrFamily returns "ip " or "ip6" for IPv4 or IPv6 addresses respectively.
func addrFamily(addr string) (string, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return "", fmt.Errorf("not a valid IP address")
	}
	if ip.To4() != nil {
		return "ip ", nil
	}
	return "ip6", nil
}

// Resolver is the interface used for DNS lookups, injectable for testing.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// ResolvePolicy resolves all DNS names and dns: {} entries in e, replacing
// hostnames in rule To: lists with their resolved IPs. Returns a new
// EgressPolicy; the original is not modified.
//
// resolvConf is only read when at least one dns: rule has no explicit servers
// (i.e. NeedsResolvConf returns true). If all dns: rules carry explicit
// servers, resolvConf may safely be nil.
func ResolvePolicy(ctx context.Context, e *policy.EgressPolicy, resolvConf io.Reader, r Resolver) (*policy.EgressPolicy, error) {
	if r == nil {
		r = net.DefaultResolver
	}

	// Read nameservers only for dns: {} rules that don't override with explicit
	// servers. If all dns: rules have explicit servers, resolv.conf is not
	// accessed and resolvConf may be nil.
	var nameservers []string
	if NeedsResolvConf(e) {
		if resolvConf == nil {
			return nil, fmt.Errorf("dns: {} requires /etc/resolv.conf but no reader was provided")
		}
		ns, err := ParseResolvConf(resolvConf)
		if err != nil {
			return nil, fmt.Errorf("dns: {}: %w", err)
		}
		nameservers = ns
	}

	resolved := &policy.EgressPolicy{
		DefaultAction: e.DefaultAction,
		WithTCPReset:  e.WithTCPReset,
		BlockMetadata: e.BlockMetadata,
	}

	var err error
	resolved.Accept, err = resolveRules(ctx, e.Accept, nameservers, r)
	if err != nil {
		return nil, fmt.Errorf("accept rules: %w", err)
	}
	resolved.Reject, err = resolveRules(ctx, e.Reject, nameservers, r)
	if err != nil {
		return nil, fmt.Errorf("reject rules: %w", err)
	}
	resolved.Drop, err = resolveRules(ctx, e.Drop, nameservers, r)
	if err != nil {
		return nil, fmt.Errorf("drop rules: %w", err)
	}

	return resolved, nil
}

// NeedsResolvConf returns true only when at least one dns: rule has no
// explicit servers set, meaning /etc/resolv.conf must be parsed.
// A dns: rule with explicit servers does NOT require resolv.conf.
// Exported so vaka-init can decide whether to open /etc/resolv.conf before
// calling ResolvePolicy.
func NeedsResolvConf(e *policy.EgressPolicy) bool {
	for _, rules := range [][]policy.Rule{e.Accept, e.Reject, e.Drop} {
		for _, r := range rules {
			if r.DNS != nil && len(r.DNS.Servers) == 0 {
				return true
			}
		}
	}
	return false
}

// resolveRules expands dns: {} and resolves hostnames in each rule.
func resolveRules(ctx context.Context, rules []policy.Rule, nameservers []string, r Resolver) ([]policy.Rule, error) {
	var out []policy.Rule
	for _, rule := range rules {
		if rule.DNS != nil {
			// Expand dns: {} into synthetic TCP+UDP accept rules with pre-resolved IPs.
			servers := nameservers
			if len(rule.DNS.Servers) > 0 {
				servers = rule.DNS.Servers
			}
			// dns: {} becomes a set of concrete rules (one per server+proto).
			// We inject them as to: [ip1, ip2...] port: [53] entries for both TCP and UDP.
			dnsRule := policy.Rule{
				To:    servers,
				Ports: []policy.PortSpec{{Single: 53}},
			}
			for _, proto := range []string{"udp", "tcp"} {
				dr := dnsRule
				dr.Proto = proto
				out = append(out, dr)
			}
			continue
		}

		// Resolve hostnames in To:.
		resolved := rule
		resolved.To = nil
		for _, addr := range rule.To {
			if classifyAddr(addr) != "hostname" {
				resolved.To = append(resolved.To, addr)
				continue
			}
			addrs, err := r.LookupHost(ctx, addr)
			if err != nil {
				return nil, fmt.Errorf("resolve %q: %w", addr, err)
			}
			resolved.To = append(resolved.To, addrs...)
		}
		out = append(out, resolved)
	}
	return out, nil
}
