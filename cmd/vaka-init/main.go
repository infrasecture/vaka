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
	"runtime"
	"sort"
	"strings"
	"syscall"

	mobyuser "github.com/moby/sys/user"
	"github.com/syndtr/gocapability/capability"
	"golang.org/x/sys/unix"
	"vaka.dev/vaka/internal/runtimebundle"
	"vaka.dev/vaka/pkg/nft"
	"vaka.dev/vaka/pkg/policy"
)

var runtimeVersion = runtimebundle.Version()

const nftBin = "/opt/vaka/sbin/nft"
const passwdPath = "/etc/passwd"
const groupPath = "/etc/group"

var (
	setgroupsFn = unix.Setgroups
	setresgidFn = unix.Setresgid
	setresuidFn = unix.Setresuid
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stdout, runtimeVersion)
		return
	}

	mode, harness, execUser, err := parseInvocation(os.Args[1:])
	if err != nil {
		fatal("%v", err)
	}
	if mode == modeNoop {
		return
	}
	// Linux capabilities and credentials are per-thread. Keep every security
	// transition and the final execve on one OS thread so the Go scheduler
	// cannot move verification to a thread with the container's stored sets.
	runtime.LockOSThread()

	// Step 1: Read the injected policy from the immutable container
	// configuration inherited by startup, healthchecks, and exec processes.
	p, err := readPolicyValue(os.Getenv(runtimebundle.PolicyEnvironment))
	if err != nil {
		fatal("%v", err)
	}
	expectedRevision := strings.TrimSpace(os.Getenv(runtimebundle.PolicyRevisionEnvironment))
	actualRevision, err := policy.Revision(p)
	if err != nil {
		fatal("compute injected policy revision: %v", err)
	}
	if expectedRevision == "" || actualRevision != expectedRevision {
		fatal("injected policy revision mismatch: expected %q, got %q", expectedRevision, actualRevision)
	}
	if errs := policy.ValidateInjected(p); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		fatal("validate policy: %s", strings.Join(msgs, "; "))
	}
	if len(p.Services) != 1 {
		fatal("policy must contain exactly one service, got %d", len(p.Services))
	}
	if err := checkRuntimeVersion(p.RequiredRuntimeVersion, runtimeVersion); err != nil {
		fatal("%v", err)
	}
	var svcName string
	var svc *policy.ServiceConfig
	for k, v := range p.Services {
		svcName, svc = k, v
	}
	if mode == modeStartup {
		if svc.Network == nil || svc.Network.Egress == nil {
			fatal("service %s: no network.egress policy", svcName)
		}
		egress := svc.Network.Egress

		// Resolve dynamic values only during container startup. Exec mode uses
		// the rules already installed in the shared network namespace.
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

		ruleset, err := nft.Generate(resolved)
		if err != nil {
			fatal("generate ruleset: %v", err)
		}

		// Apply ruleset atomically via nft -f /dev/stdin.
		nftCmd := exec.Command(nftBin, "-f", "/dev/stdin")
		nftCmd.Stdin = strings.NewReader(ruleset)
		nftCmd.Stdout = os.Stderr // nft writes diagnostics to stdout
		nftCmd.Stderr = os.Stderr
		if err := nftCmd.Run(); err != nil {
			fatal("nft -f /dev/stdin: %v\nruleset:\n%s", err, ruleset)
		}
	} else {
		// Docker can accept execs and begin healthchecks as soon as the
		// container is running, before its entrypoint has finished installing
		// policy. The nftables table is a kernel-owned readiness marker that the
		// unprivileged workload cannot forge. Fail closed rather than running a
		// command in that startup window.
		check := exec.Command(nftBin, "list", "table", "inet", "vaka")
		check.Stdout = io.Discard
		check.Stderr = io.Discard
		if err := check.Run(); err != nil {
			fatal("egress policy is not installed yet; retry the command after service startup: %v", err)
		}
	}

	// Step 5: Resolve target service user (compose-compatible syntax).
	targetUserSpec := svc.User
	if mode == modeExec && execUser != "" {
		targetUserSpec = execUser
	}
	targetUser, err := resolveExecUser(targetUserSpec, passwdPath, groupPath)
	if err != nil {
		fatal("resolve service user %q: %v", targetUserSpec, err)
	}
	targetUser, err = withAdditionalGroups(targetUser, svc.GroupAdd, groupPath)
	if err != nil {
		fatal("resolve Compose group_add %v: %v", svc.GroupAdd, err)
	}

	// Step 6: Apply optional runtime.chown ownership-fix actions. Paths are
	// restricted to writable non-root mounted filesystems.
	chownActions := []policy.ChownAction{}
	dropCaps := []string{}
	if svc.Runtime != nil {
		chownActions = svc.Runtime.Chown
		dropCaps = svc.Runtime.DropCaps
	}
	if mode == modeStartup {
		if err := applyChownActions(chownActions, targetUser, passwdPath, groupPath); err != nil {
			fatal("apply runtime.chown: %v", err)
		}
	}

	// Step 7: Drop capabilities. For identity transitions, SETUID/SETGID can be
	// deferred from Effective/Permitted until after switch while still dropping
	// from Bounding/Inheritable up front.
	switchNeeded, deferSetUID, deferSetGID := transitionCapabilities(targetUser)

	if len(dropCaps) > 0 {
		if deferSetUID || deferSetGID {
			if err := dropCapsPreservingTransition(dropCaps, deferSetUID, deferSetGID); err != nil {
				fatal("drop caps (pre-switch): %v", err)
			}
		} else {
			if err := dropCapsFully(dropCaps); err != nil {
				fatal("drop caps: %v", err)
			}
		}
	}

	// Step 8: Restore original service identity when needed.
	if switchNeeded {
		if err := switchIdentity(targetUser); err != nil {
			fatal("switch identity to user %q: %v", targetUserSpec, err)
		}
		// On 0->nonzero UID transitions, the kernel clears Effective/Permitted/
		// Ambient automatically. When UID remains 0 (e.g. user "0:1000"),
		// deferred SETUID/SETGID still need explicit Effective/Permitted drop.
		if len(dropCaps) > 0 && targetUser.UID == 0 {
			deferred, err := deferredTransitionCapNames(dropCaps, deferSetUID, deferSetGID)
			if err != nil {
				fatal("resolve deferred capability drops: %v", err)
			}
			if len(deferred) > 0 {
				if err := dropCapsFully(deferred); err != nil {
					fatal("drop deferred caps after identity switch: %v", err)
				}
			}
		}
	}
	if len(dropCaps) > 0 {
		if err := verifyCapsAbsent(dropCaps, nil); err != nil {
			fatal("verify dropped capabilities before exec: %v", err)
		}
	}

	// Step 9: execve — replace vaka-init with the harness.
	argv0, err := exec.LookPath(harness[0])
	if err != nil {
		fatal("look up %s: %v", harness[0], err)
	}
	if err := syscall.Exec(argv0, harness, os.Environ()); err != nil {
		fatal("execve %s: %v", argv0, err)
	}
}

type invocationMode int

const (
	modeNoop invocationMode = iota
	modeStartup
	modeExec
)

// parseInvocation preserves the historical no-op for empty/unknown probes,
// while making malformed supported modes fail closed. Exec mode is used for
// processes Docker starts after the container entrypoint (Compose exec and
// healthchecks), which otherwise inherit the container's temporary startup
// privileges instead of vaka-init's dropped process state.
func parseInvocation(args []string) (invocationMode, []string, string, error) {
	if len(args) == 0 {
		return modeNoop, nil, "", nil
	}
	switch args[0] {
	case "--":
		if len(args) == 1 {
			return modeStartup, nil, "", fmt.Errorf("no harness command after --")
		}
		return modeStartup, args[1:], "", nil
	case "exec":
		i := 1
		execUser := ""
		if i < len(args) && args[i] == "--user" {
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return modeExec, nil, "", fmt.Errorf("vaka-init exec --user requires a value")
			}
			execUser = args[i+1]
			i += 2
		}
		if i >= len(args) || args[i] != "--" {
			return modeExec, nil, "", fmt.Errorf("usage: vaka-init exec [--user <user>] -- <command> [args...]")
		}
		i++
		if i >= len(args) {
			return modeExec, nil, "", fmt.Errorf("no command after exec --")
		}
		return modeExec, args[i:], execUser, nil
	default:
		fmt.Fprintln(os.Stderr, "vaka-init: usage: vaka-init -- <entrypoint> [args...] | vaka-init exec -- <command> [args...]")
		return modeNoop, nil, "", nil
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

func readPolicyValue(encoded string) (*policy.ServicePolicy, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, fmt.Errorf("%s is missing from the container configuration", runtimebundle.PolicyEnvironment)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("base64-decode %s: %w", runtimebundle.PolicyEnvironment, err)
	}
	p, err := policy.Parse(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	return p, nil
}

func dropCapsFully(caps []string) error {
	capNums, err := parseCaps(caps)
	if err != nil {
		return err
	}
	return applyCapDrop(capNums, nil)
}

func dropCapsPreservingTransition(caps []string, deferSetUID, deferSetGID bool) error {
	capNums, err := parseCaps(caps)
	if err != nil {
		return err
	}
	deferred := map[capability.Cap]bool{}
	if deferSetUID {
		deferred[capability.CAP_SETUID] = true
	}
	if deferSetGID {
		deferred[capability.CAP_SETGID] = true
	}
	return applyCapDrop(capNums, deferred)
}

func applyCapDrop(capNums []capability.Cap, deferred map[capability.Cap]bool) error {
	capNums = uniqueCaps(capNums)
	c, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("capability.NewPid2: %w", err)
	}
	if err := c.Load(); err != nil {
		return fmt.Errorf("load caps: %w", err)
	}

	// gocapability silently skips bounding-set removal when CAP_SETPCAP is not
	// effective. Use the kernel operation directly and fail before changing any
	// other set if a requested bounding drop cannot be guaranteed.
	for _, cap := range capNums {
		if !c.Get(capability.BOUNDS, cap) {
			continue
		}
		if !c.Get(capability.EFFECTIVE, capability.CAP_SETPCAP) {
			return fmt.Errorf("drop %s from bounding set: CAP_SETPCAP is not effective", strings.ToUpper(cap.String()))
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(cap), 0, 0, 0); err != nil {
			return fmt.Errorf("drop %s from bounding set: %w", strings.ToUpper(cap.String()), err)
		}
	}

	for _, cap := range capNums {
		c.Unset(capability.INHERITABLE, cap)
		if deferred == nil || !deferred[cap] {
			c.Unset(capability.EFFECTIVE, cap)
			c.Unset(capability.PERMITTED, cap)
		}
	}

	// Inheritable must be applied before clearing Ambient (kernel requires
	// that an Ambient cap is present in Inheritable; clearing I first ensures
	// the Ambient clear-all succeeds cleanly).
	if err := c.Apply(capability.INHERITABLE | capability.EFFECTIVE | capability.PERMITTED); err != nil {
		return fmt.Errorf("apply effective, permitted, and inheritable capabilities: %w", err)
	}

	// Clear all Ambient caps (must be done after Inheritable is updated).
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl ambient clear: %w", err)
	}

	return verifyCapNumsAbsent(capNums, deferred)
}

func uniqueCaps(caps []capability.Cap) []capability.Cap {
	seen := map[capability.Cap]bool{}
	out := make([]capability.Cap, 0, len(caps))
	for _, cap := range caps {
		if !seen[cap] {
			seen[cap] = true
			out = append(out, cap)
		}
	}
	return out
}

func verifyCapsAbsent(names []string, deferred map[capability.Cap]bool) error {
	capNums, err := parseCaps(names)
	if err != nil {
		return err
	}
	return verifyCapNumsAbsent(uniqueCaps(capNums), deferred)
}

func verifyCapNumsAbsent(capNums []capability.Cap, deferred map[capability.Cap]bool) error {
	c, err := capability.NewPid2(0)
	if err != nil {
		return fmt.Errorf("capability.NewPid2: %w", err)
	}
	if err := c.Load(); err != nil {
		return fmt.Errorf("load caps for verification: %w", err)
	}
	types := []struct {
		kind capability.CapType
		name string
	}{
		{capability.EFFECTIVE, "effective"},
		{capability.PERMITTED, "permitted"},
		{capability.INHERITABLE, "inheritable"},
		{capability.AMBIENT, "ambient"},
		{capability.BOUNDS, "bounding"},
	}
	for _, cap := range capNums {
		for _, set := range types {
			if deferred != nil && deferred[cap] && (set.kind == capability.EFFECTIVE || set.kind == capability.PERMITTED) {
				continue
			}
			if c.Get(set.kind, cap) {
				return fmt.Errorf("%s remains in %s set", strings.ToUpper(cap.String()), set.name)
			}
		}
	}
	return nil
}

type execIdentity struct {
	UID               int
	GID               int
	SupplementaryGIDs []int
}

func resolveExecUser(userSpec, passwd, group string) (*execIdentity, error) {
	spec := strings.TrimSpace(userSpec)
	if spec == "" {
		return nil, nil
	}
	execUser, err := mobyuser.GetExecUserPath(spec, nil, passwd, group)
	if err != nil {
		return nil, err
	}
	ids := append([]int(nil), execUser.Sgids...)
	sort.Ints(ids)
	dedup := ids[:0]
	for _, gid := range ids {
		if len(dedup) == 0 || dedup[len(dedup)-1] != gid {
			dedup = append(dedup, gid)
		}
	}
	return &execIdentity{
		UID:               execUser.Uid,
		GID:               execUser.Gid,
		SupplementaryGIDs: dedup,
	}, nil
}

func withAdditionalGroups(identity *execIdentity, groupAdd []string, group string) (*execIdentity, error) {
	if len(groupAdd) == 0 {
		return identity, nil
	}
	uniqueSpecs := make([]string, 0, len(groupAdd))
	seenSpecs := make(map[string]bool, len(groupAdd))
	for _, spec := range groupAdd {
		if !seenSpecs[spec] {
			seenSpecs[spec] = true
			uniqueSpecs = append(uniqueSpecs, spec)
		}
	}
	additional, err := mobyuser.GetAdditionalGroupsPath(uniqueSpecs, group)
	if err != nil {
		return nil, err
	}
	if identity == nil {
		identity = &execIdentity{}
	} else {
		identity = &execIdentity{
			UID:               identity.UID,
			GID:               identity.GID,
			SupplementaryGIDs: append([]int(nil), identity.SupplementaryGIDs...),
		}
	}
	identity.SupplementaryGIDs = append(identity.SupplementaryGIDs, additional...)
	sort.Ints(identity.SupplementaryGIDs)
	dedup := identity.SupplementaryGIDs[:0]
	for _, gid := range identity.SupplementaryGIDs {
		if len(dedup) == 0 || dedup[len(dedup)-1] != gid {
			dedup = append(dedup, gid)
		}
	}
	identity.SupplementaryGIDs = dedup
	return identity, nil
}

func needsIdentitySwitch(u *execIdentity) bool {
	if u == nil {
		return false
	}
	return u.UID != 0 || u.GID != 0 || len(u.SupplementaryGIDs) > 0
}

// transitionCapabilities reports which identity-changing capabilities must
// remain effective and permitted until switchIdentity completes. setgroups(2)
// requires SETGID even when it only clears supplementary groups and the target
// primary GID remains 0.
func transitionCapabilities(u *execIdentity) (switchNeeded, deferSetUID, deferSetGID bool) {
	switchNeeded = needsIdentitySwitch(u)
	return switchNeeded, switchNeeded && u.UID != 0, switchNeeded
}

func switchIdentity(u *execIdentity) error {
	if u == nil {
		return nil
	}
	if u.UID < 0 || u.GID < 0 {
		return fmt.Errorf("uid/gid must be non-negative, got uid=%d gid=%d", u.UID, u.GID)
	}
	if err := setgroupsFn(u.SupplementaryGIDs); err != nil {
		return fmt.Errorf("setgroups(%v): %w", u.SupplementaryGIDs, err)
	}
	if err := setresgidFn(u.GID, u.GID, u.GID); err != nil {
		return fmt.Errorf("setresgid(%d): %w", u.GID, err)
	}
	if err := setresuidFn(u.UID, u.UID, u.UID); err != nil {
		return fmt.Errorf("setresuid(%d): %w", u.UID, err)
	}
	return nil
}

func deferredTransitionCapNames(dropCaps []string, deferSetUID, deferSetGID bool) ([]string, error) {
	caps, err := parseCaps(dropCaps)
	if err != nil {
		return nil, err
	}
	dropSetUID, dropSetGID := false, false
	for _, cap := range caps {
		dropSetUID = dropSetUID || cap == capability.CAP_SETUID
		dropSetGID = dropSetGID || cap == capability.CAP_SETGID
	}
	out := []string{}
	if deferSetUID && dropSetUID {
		out = append(out, "SETUID")
	}
	if deferSetGID && dropSetGID {
		out = append(out, "SETGID")
	}
	return out, nil
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

// checkRuntimeVersion requires an exact runtime-bundle match. The bundle
// version covers vaka-init, nft, their filesystem layout, and the injected
// policy contract, so range compatibility would weaken the fail-closed check.
func checkRuntimeVersion(required, actual string) error {
	if required == "" {
		return fmt.Errorf("requiredRuntimeVersion: missing")
	}
	if required == actual {
		return nil
	}
	return fmt.Errorf("requiredRuntimeVersion: policy requires %s, but vaka-init is %s", required, actual)
}
