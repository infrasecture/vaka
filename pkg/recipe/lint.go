package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"vaka.dev/vaka/pkg/policy"
)

// Risk flags (design §7). The set matches the registry CI lint
// (scripts/validate_recipe.py in vaka-registry); vaka always recomputes
// them locally from the actual files — the index copy is advisory.
const (
	FlagPrivileged        = "privileged"
	FlagCapAddBroad       = "cap-add-broad"
	FlagDockerSocketMount = "docker-socket-mount"
	FlagHostNetwork       = "host-network"
	FlagHostPID           = "host-pid"
	FlagHostIPC           = "host-ipc"
	FlagBroadBindMount    = "broad-bind-mount"
	FlagEgressDefaultAcc  = "egress-default-accept"
	FlagNoPolicyForSvc    = "no-policy-for-service"
	FlagDisablesVakaInit  = "disables-vaka-init"
)

var broadMountSources = map[string]bool{
	"/": true, "/home": true, "/root": true, "/etc": true,
	"/usr": true, "/var": true, "/proc": true, "/sys": true,
}

var broadCaps = map[string]bool{"SYS_ADMIN": true, "ALL": true}

const (
	vakaInitLabel    = "agent.vaka.init"
	dockerSocketPath = "/var/run/docker.sock"
)

// LocalPolicySummary is the locally computed policy digest of a recipe
// directory: the authoritative counterpart of the index's advisory copy.
type LocalPolicySummary struct {
	// DefaultActions maps every compose service to its vaka.yaml egress
	// defaultAction ("none" when the service has no policy entry).
	DefaultActions map[string]string
	// RiskFlags are "service:flag" strings, sorted and unique.
	RiskFlags []string
}

// composeFileNames are recognized compose file names in docker's precedence
// order (compose.* wins over docker-compose.*).
var composeFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// findComposeFile returns the recipe's canonical compose file within dir.
func findComposeFile(dir string) (string, error) {
	for _, name := range composeFileNames {
		p := filepath.Join(dir, name)
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no compose file in %s (looked for %s)", dir, strings.Join(composeFileNames, ", "))
}

// LintDir loads the recipe's own compose file and vaka.yaml and computes the
// policy summary and risk flags from those files.
//
// The compose project is pinned to the recipe's canonical compose file with a
// controlled (empty) interpolation environment: the lint deliberately does
// not honor COMPOSE_FILE, the ambient OS environment, or a local .env, so it
// analyzes the recipe as shipped (with compose defaults) and cannot be
// steered to a different project or external files by the caller's
// environment.
func LintDir(ctx context.Context, dir string) (*LocalPolicySummary, error) {
	composeFile, err := findComposeFile(dir)
	if err != nil {
		return nil, err
	}
	opts, err := composecli.NewProjectOptions(
		[]string{composeFile},
		composecli.WithWorkingDirectory(dir),
		composecli.WithName("vaka-lint"),
		composecli.WithEnv(nil),
		composecli.WithInterpolation(true),
	)
	if err != nil {
		return nil, fmt.Errorf("compose project options: %w", err)
	}
	project, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("load compose project: %w", err)
	}

	f, err := os.Open(filepath.Join(dir, "vaka.yaml"))
	if err != nil {
		return nil, fmt.Errorf("recipe policy: %w", err)
	}
	pol, err := policy.Parse(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("recipe policy: %w", err)
	}

	summary := &LocalPolicySummary{DefaultActions: map[string]string{}}
	flags := map[string]bool{}
	flag := func(svc, name string) { flags[svc+":"+name] = true }

	for svcName, svc := range project.Services {
		lintService(svcName, svc, flag)

		var polSvc *policy.ServiceConfig
		if pol.Services != nil {
			polSvc = pol.Services[svcName]
		}
		if polSvc == nil {
			flag(svcName, FlagNoPolicyForSvc)
			summary.DefaultActions[svcName] = "none"
			continue
		}
		action := "none"
		if polSvc.Network != nil && polSvc.Network.Egress != nil && polSvc.Network.Egress.DefaultAction != "" {
			action = polSvc.Network.Egress.DefaultAction
		}
		summary.DefaultActions[svcName] = action
		if action == "accept" {
			flag(svcName, FlagEgressDefaultAcc)
		}
	}

	summary.RiskFlags = sortedKeys(flags)
	return summary, nil
}

func lintService(svcName string, svc composetypes.ServiceConfig, flag func(svc, name string)) {
	if svc.Privileged {
		flag(svcName, FlagPrivileged)
	}
	for _, cap := range svc.CapAdd {
		if broadCaps[strings.TrimPrefix(strings.ToUpper(cap), "CAP_")] {
			flag(svcName, FlagCapAddBroad)
			break
		}
	}
	if svc.NetworkMode == "host" {
		flag(svcName, FlagHostNetwork)
	}
	if svc.Pid == "host" {
		flag(svcName, FlagHostPID)
	}
	if svc.Ipc == "host" {
		flag(svcName, FlagHostIPC)
	}
	for _, vol := range svc.Volumes {
		if vol.Type != composetypes.VolumeTypeBind {
			continue
		}
		src := filepath.Clean(vol.Source)
		switch {
		case src == dockerSocketPath || vol.Target == dockerSocketPath:
			flag(svcName, FlagDockerSocketMount)
		case broadMountSources[src]:
			flag(svcName, FlagBroadBindMount)
		}
	}
	if svc.Labels[vakaInitLabel] == "present" {
		flag(svcName, FlagDisablesVakaInit)
	}
}

// CompareWithIndex checks the locally computed summary against the index's
// advisory policy block. An absent index block yields nothing; a differing
// one yields loud warnings (design §4) — never a failure, because lint
// versions legitimately differ between a registry's CI and this vaka, and
// the digest already guarantees the artifact is what the index promised.
func CompareWithIndex(local *LocalPolicySummary, indexActions map[string]string, indexFlags []string, recipeRef string) []string {
	if indexActions == nil && indexFlags == nil {
		return nil
	}
	var warnings []string
	stale := func(what string) {
		warnings = append(warnings, fmt.Sprintf(
			"registry metadata is stale/inaccurate for %s: %s differs from the locally computed policy (trust the local result shown here)",
			recipeRef, what))
	}

	if len(indexActions) != len(local.DefaultActions) {
		stale("the default-action table")
	} else {
		for svc, act := range local.DefaultActions {
			if indexActions[svc] != act {
				stale("the default-action table")
				break
			}
		}
	}

	localFlags := append([]string{}, local.RiskFlags...)
	idxFlags := append([]string{}, indexFlags...)
	sort.Strings(localFlags)
	sort.Strings(idxFlags)
	if strings.Join(localFlags, ",") != strings.Join(idxFlags, ",") {
		stale("the risk-flag list")
	}
	return warnings
}
