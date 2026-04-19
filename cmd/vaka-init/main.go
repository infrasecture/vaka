// cmd/vaka-init/main.go
//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/syndtr/gocapability/capability"
	"golang.org/x/sys/unix"
	"vaka.dev/vaka/pkg/nft"
	"vaka.dev/vaka/pkg/policy"
)

// version is set at build time via -ldflags="-X main.version=<tag>".
var version = "dev"

const secretPath = "/run/secrets/vaka.yaml"
const nftBin = "/usr/local/sbin/nft"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "--" {
		fatal("usage: vaka-init -- <entrypoint> [args...]")
	}
	harness := os.Args[2:]
	if len(harness) == 0 {
		fatal("vaka-init: no harness command after --")
	}

	// Step 1: Read and parse per-service policy from Docker secret.
	// The secret file contains the base64-encoded policy YAML written by
	// docker compose from the VAKA_<SERVICE>_CONF environment variable.
	p, err := readPolicy(secretPath)
	if err != nil {
		fatal("%v", err)
	}
	if len(p.Services) != 1 {
		fatal("policy must contain exactly one service, got %d", len(p.Services))
	}
	if p.APIVersion != "agent.vaka/v1alpha1" {
		fatal("unsupported apiVersion: %s", p.APIVersion)
	}
	var svcName string
	var svc *policy.ServiceConfig
	for k, v := range p.Services {
		svcName, svc = k, v
	}
	if svc.Network == nil || svc.Network.Egress == nil {
		fatal("service %s: no network.egress policy", svcName)
	}
	egress := svc.Network.Egress

	// Step 2: Resolve dynamic values.
	// Open /etc/resolv.conf only when the policy contains a dns: {} rule with
	// no explicit servers. Policies that use dns.servers exclusively work fine
	// without resolv.conf (e.g. scratch containers).
	var resolveConf io.Reader
	if nft.NeedsResolvConf(egress) {
		f, err := os.Open("/etc/resolv.conf")
		if err != nil {
			fatal("open /etc/resolv.conf: %v", err)
		}
		defer f.Close()
		resolveConf = f
	}
	resolved, err := nft.ResolvePolicy(context.Background(), egress, resolveConf, nil)
	if err != nil {
		fatal("resolve policy: %v", err)
	}

	// Step 3: Generate nft ruleset.
	ruleset, err := nft.Generate(resolved)
	if err != nil {
		fatal("generate ruleset: %v", err)
	}

	// Step 4: Apply ruleset atomically via nft -f /dev/stdin.
	nftCmd := exec.Command(nftBin, "-f", "/dev/stdin")
	nftCmd.Stdin = strings.NewReader(ruleset)
	nftCmd.Stdout = os.Stderr // nft writes diagnostics to stdout
	nftCmd.Stderr = os.Stderr
	if err := nftCmd.Run(); err != nil {
		fatal("nft -f /dev/stdin: %v\nruleset:\n%s", err, ruleset)
	}

	// Step 5: Drop capabilities listed in dropCaps.
	if svc.Runtime != nil && len(svc.Runtime.DropCaps) > 0 {
		if err := dropCaps(svc.Runtime.DropCaps); err != nil {
			fatal("drop caps: %v", err)
		}
	}

	// Step 6: Apply runAs (setresgid then setresuid).
	if svc.Runtime != nil && svc.Runtime.RunAs != nil {
		uid := svc.Runtime.RunAs.UID
		gid := svc.Runtime.RunAs.GID
		// Validate before touching syscalls.
		if uid < 0 || gid < 0 {
			fatal("runAs uid/gid must be non-negative, got uid=%d gid=%d", uid, gid)
		}
		// GID must be changed before UID.
		if err := unix.Setresgid(gid, gid, gid); err != nil {
			fatal("setresgid(%d): %v", gid, err)
		}
		if err := unix.Setresuid(uid, uid, uid); err != nil {
			fatal("setresuid(%d): %v", uid, err)
		}
		// Kernel clears E+P automatically on 0→nonzero UID transition.
	}

	// Step 7: execve — replace vaka-init with the harness.
	argv0, err := exec.LookPath(harness[0])
	if err != nil {
		fatal("look up %s: %v", harness[0], err)
	}
	if err := syscall.Exec(argv0, harness, os.Environ()); err != nil {
		fatal("execve %s: %v", argv0, err)
	}
}

// readPolicy reads the base64-encoded policy YAML from path, decodes it, and
// parses it. Docker compose writes the VAKA_<SERVICE>_CONF env var value
// directly into the secret file, so the file contains base64 text, not raw YAML.
func readPolicy(path string) (*policy.ServicePolicy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	raw, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("base64-decode %s: %w", path, err)
	}
	p, err := policy.Parse(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	return p, nil
}

func dropCaps(caps []string) error {
	capNums, err := parseCaps(caps)
	if err != nil {
		return err
	}

	c, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("capability.NewPid2: %w", err)
	}
	if err := c.Load(); err != nil {
		return fmt.Errorf("load caps: %w", err)
	}

	for _, cap := range capNums {
		c.Unset(capability.INHERITABLE, cap)
		c.Unset(capability.BOUNDS, cap)
		c.Unset(capability.EFFECTIVE, cap)
		c.Unset(capability.PERMITTED, cap)
	}

	// Inheritable must be applied before clearing Ambient (kernel requires
	// that an Ambient cap is present in Inheritable; clearing I first ensures
	// the Ambient clear-all succeeds cleanly).
	if err := c.Apply(capability.INHERITABLE | capability.BOUNDS | capability.EFFECTIVE | capability.PERMITTED); err != nil {
		return fmt.Errorf("apply caps (requires CAP_SETPCAP in Effective — is CAP_SETPCAP present?): %w", err)
	}

	// Clear all Ambient caps (must be done after Inheritable is updated).
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl ambient clear: %w", err)
	}

	return nil
}

// parseCaps converts short-form capability names to capability.Cap values.
// Accepts both "NET_ADMIN" and "CAP_NET_ADMIN".
func parseCaps(names []string) ([]capability.Cap, error) {
	var result []capability.Cap
	for _, name := range names {
		normalized := strings.ToLower(strings.TrimPrefix(strings.ToUpper(name), "CAP_"))
		found := false
		for c := capability.Cap(0); c <= capability.CAP_LAST_CAP; c++ {
			if c.String() == normalized {
				result = append(result, c)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown capability: %q", name)
		}
	}
	return result, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vaka-init: "+format+"\n", args...)
	os.Exit(1)
}
