// pkg/policy/validate.go
package policy

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

var (
	validProtos    = map[string]bool{"tcp": true, "udp": true, "icmp": true, "icmpv6": true}
	validActions   = map[string]bool{"accept": true, "reject": true, "drop": true}
	hostnameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9\-]*[a-z0-9])?)*$`)
	// svcNameRegexp matches valid Docker Compose service names (DNS label + underscore).
	svcNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

	// knownICMPTypes is the set of named ICMP type keywords recognised by nft.
	knownICMPTypes = map[string]bool{
		"echo-reply":              true,
		"echo-request":            true,
		"destination-unreachable": true,
		"parameter-problem":       true,
		"redirect":                true,
		"router-advertisement":    true,
		"router-solicitation":     true,
		"time-exceeded":           true,
		"timestamp-reply":         true,
		"timestamp-request":       true,
		// ICMPv6
		"nd-neighbor-solicit": true,
		"nd-neighbor-advert":  true,
		"nd-router-solicit":   true,
		"nd-router-advert":    true,
		"mld-listener-query":  true,
		"mld-listener-report": true,
	}

	// knownLinuxCaps is the set of short-form capability names defined in
	// include/uapi/linux/capability.h through Linux 5.x / 6.x.
	// Short-form means without the "CAP_" prefix.
	knownLinuxCaps = map[string]bool{
		"CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true,
		"FOWNER": true, "FSETID": true, "KILL": true, "SETGID": true,
		"SETUID": true, "SETPCAP": true, "LINUX_IMMUTABLE": true,
		"NET_BIND_SERVICE": true, "NET_BROADCAST": true, "NET_ADMIN": true,
		"NET_RAW": true, "IPC_LOCK": true, "IPC_OWNER": true,
		"SYS_MODULE": true, "SYS_RAWIO": true, "SYS_CHROOT": true,
		"SYS_PTRACE": true, "SYS_PACCT": true, "SYS_ADMIN": true,
		"SYS_BOOT": true, "SYS_NICE": true, "SYS_RESOURCE": true,
		"SYS_TIME": true, "SYS_TTY_CONFIG": true, "MKNOD": true,
		"LEASE": true, "AUDIT_WRITE": true, "AUDIT_CONTROL": true,
		"SETFCAP": true, "MAC_OVERRIDE": true, "MAC_ADMIN": true,
		"SYSLOG": true, "WAKE_ALARM": true, "BLOCK_SUSPEND": true,
		"AUDIT_READ": true, "PERFMON": true, "BPF": true,
		"CHECKPOINT_RESTORE": true,
	}
)

// Validate checks p against all §6.3 rules.
// networkModes maps service name → network_mode for every service declared in
// docker-compose.yaml (pass nil to skip compose cross-checks in unit tests).
// Returns all validation errors found (not just the first).
func Validate(p *ServicePolicy, networkModes map[string]string) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if p.APIVersion != "vaka.dev/v1alpha1" {
		add("apiVersion: must be \"vaka.dev/v1alpha1\", got %q", p.APIVersion)
	}
	if p.Kind != "ServicePolicy" {
		add("kind: must be \"ServicePolicy\", got %q", p.Kind)
	}

	for name, svc := range p.Services {
		prefix := "services." + name

		// Service name must be a valid DNS label (Docker Compose naming rules).
		if !svcNameRegexp.MatchString(name) {
			add("%s: invalid service name %q (must match [a-z0-9][a-z0-9_-]*)", prefix, name)
		}

		// Service must exist in docker-compose.yaml when compose data is available.
		if networkModes != nil {
			mode, ok := networkModes[name]
			if !ok {
				add("%s: service %q not found in docker-compose.yaml", prefix, name)
			} else if mode == "host" {
				add("%s: network_mode: host — vaka cannot isolate a container sharing the host network namespace", prefix)
			}
		}

		if svc == nil {
			continue
		}

		// Validate network egress rules.
		if svc.Network != nil && svc.Network.Egress != nil {
			e := svc.Network.Egress
			ep := prefix + ".network.egress"

			// defaultAction
			if e.DefaultAction != "" && !validActions[e.DefaultAction] {
				add("%s.defaultAction: unknown value %q (expected accept, reject, drop)", ep, e.DefaultAction)
			}

			// with_tcp_reset is only valid when defaultAction is "reject" (or empty,
			// which Parse normalises to "reject").
			if e.WithTCPReset != nil && e.DefaultAction != "reject" && e.DefaultAction != "" {
				add("%s.with_tcp_reset: only valid when defaultAction is \"reject\" (got %q)", ep, e.DefaultAction)
			}

			// Validate all rule lists.
			for listName, rules := range map[string][]Rule{
				"accept": e.Accept,
				"reject": e.Reject,
				"drop":   e.Drop,
			} {
				for i, rule := range rules {
					rp := fmt.Sprintf("%s.%s[%d]", ep, listName, i)
					errs = append(errs, validateRule(rp, listName, rule)...)
				}
			}
		}

		// Validate runtime config regardless of whether network config is present.
		if svc.Runtime != nil {
			rp := prefix + ".runtime"
			if svc.Runtime.RunAs != nil {
				if svc.Runtime.RunAs.UID < 0 {
					add("%s.runAs.uid: must be >= 0", rp)
				}
				if svc.Runtime.RunAs.GID < 0 {
					add("%s.runAs.gid: must be >= 0", rp)
				}
			}
			for i, cap := range svc.Runtime.DropCaps {
				// Strip CAP_ prefix (accepted per spec; normalise in place).
				short := strings.TrimPrefix(cap, "CAP_")
				svc.Runtime.DropCaps[i] = short
				if !knownLinuxCaps[short] {
					add("%s.dropCaps[%d]: unknown capability %q", rp, i, cap)
				}
			}
		}
	}

	return errs
}

func validateRule(prefix string, listName string, r Rule) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if r.DNS != nil {
		// dns: {} is always valid syntactically; nothing else to check here.
		return nil
	}

	if r.Proto != "" && !validProtos[r.Proto] {
		add("%s.proto: unknown value %q (expected tcp, udp, icmp, icmpv6)", prefix, r.Proto)
	}

	// Ports without proto are ambiguous — the kernel needs a transport protocol
	// to interpret port numbers. Require proto whenever ports are set.
	if len(r.Ports) > 0 && r.Proto == "" {
		add("%s: ports specified without proto (proto: tcp or proto: udp is required when ports are set)", prefix)
	}

	for j, entry := range r.To {
		ep := fmt.Sprintf("%s.to[%d]", prefix, j)
		if err := validateToEntry(entry); err != nil {
			add("%s: %v", ep, err)
		}
	}

	for j, port := range r.Ports {
		ep := fmt.Sprintf("%s.ports[%d]", prefix, j)
		if !port.IsRange {
			if port.Single < 1 || port.Single > 65535 {
				add("%s: port %d out of range (1–65535)", ep, port.Single)
			}
		} else {
			if port.RangeStart < 1 || port.RangeEnd > 65535 || port.RangeStart >= port.RangeEnd {
				add("%s: port range %d-%d invalid", ep, port.RangeStart, port.RangeEnd)
			}
		}
	}

	if r.Type != nil && !r.Type.IsNum {
		// Named ICMP types — validate against known nft names.
		if !knownICMPType(r.Type.Name) {
			add("%s.type: unknown ICMP type name %q", prefix, r.Type.Name)
		}
	}

	// with_tcp_reset is only valid for proto: tcp rules in the reject list.
	if r.WithTCPReset != nil {
		if listName != "reject" {
			add("%s.with_tcp_reset: only valid in the reject list (found in %q list)", prefix, listName)
		} else if r.Proto != "tcp" {
			proto := r.Proto
			if proto == "" {
				proto = "(none)"
			}
			add("%s.with_tcp_reset: only valid for proto: tcp (got %q)", prefix, proto)
		}
	}

	return errs
}

func validateToEntry(s string) error {
	// Valid IPv4/IPv6 address.
	if ip := net.ParseIP(s); ip != nil {
		return nil
	}
	// Valid CIDR.
	if _, _, err := net.ParseCIDR(s); err == nil {
		return nil
	}
	// Hostname.
	if hostnameRegexp.MatchString(strings.ToLower(s)) {
		return nil
	}
	return fmt.Errorf("invalid address, CIDR, or hostname: %q", s)
}

// knownICMPType returns true if name is a valid nft ICMP type keyword.
func knownICMPType(name string) bool {
	return knownICMPTypes[name]
}
