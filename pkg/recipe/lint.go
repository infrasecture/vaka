package recipe

import (
	"bytes"
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

// requiredRecipeFiles must be present for a directory to be a well-formed
// recipe. README is documentation and is not required for the recipe to run.
var requiredRecipeFiles = []string{"recipe.yaml", "vaka.yaml"}

// ValidateStaged checks that dir is a well-formed, runnable recipe: the
// required files are present, the compose project (base + override) loads,
// vaka.yaml parses, and the egress policy validates against the compose
// services. It is run on the freshly extracted staging tree before an install
// or update commits, so a malformed or invalid published artifact is refused
// (fail closed) instead of replacing a working recipe and being reported as a
// successful update.
func ValidateStaged(ctx context.Context, dir string) error {
	root, err := OpenSafeRoot(dir)
	if err != nil {
		return err
	}
	for _, name := range requiredRecipeFiles {
		if _, err := root.Lstat(name); err != nil {
			root.Close()
			return fmt.Errorf("recipe is missing %s", name)
		}
	}
	root.Close()

	project, err := loadRecipeProject(ctx, dir)
	if err != nil {
		return err
	}
	pol, err := readRecipePolicy(dir)
	if err != nil {
		return err
	}
	networkModes := make(map[string]string, len(project.Services))
	for name, svc := range project.Services {
		networkModes[name] = svc.NetworkMode
	}
	if errs := policy.ValidateHost(pol, networkModes); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return fmt.Errorf("recipe policy is invalid:\n\t%s", strings.Join(msgs, "\n\t"))
	}
	return nil
}

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

// maxPolicyBytes bounds the vaka.yaml read (a small document).
const maxPolicyBytes = 4 << 20

// firstExisting returns the first name in names that exists (as a non-dir)
// under dir, or "" — matching compose-go's findFiles winner selection.
func firstExisting(dir string, names []string) string {
	for _, name := range names {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && !fi.IsDir() {
			return name
		}
	}
	return ""
}

// recipeComposeFiles returns the explicit compose file list vaka would run
// for the recipe: the base file plus its override, chosen with EXACTLY
// compose-go's precedence (composecli.DefaultFileNames /
// DefaultOverrideFileNames — base and override selected independently, .yml
// before .yaml for docker-compose, override family-independent). Reusing the
// library's own ordered lists — rather than hand-rolled rules — is what
// guarantees the lint analyzes the same files `vaka up` loads. The lint still
// lists them explicitly so it never honors COMPOSE_FILE and never walks above
// dir (which compose-go's own discovery would).
func recipeComposeFiles(dir string) ([]string, error) {
	base := firstExisting(dir, composecli.DefaultFileNames)
	if base == "" {
		return nil, fmt.Errorf("no compose file in %s (looked for %s)", dir, strings.Join(composecli.DefaultFileNames, ", "))
	}
	files := []string{filepath.Join(dir, base)}
	if override := firstExisting(dir, composecli.DefaultOverrideFileNames); override != "" {
		files = append(files, filepath.Join(dir, override))
	}
	return files, nil
}

// loadRecipeProject loads the recipe's compose project from its own files
// (base + override) with a controlled, empty interpolation environment: it
// deliberately does not honor COMPOSE_FILE, the ambient OS environment, or a
// local .env, and does not walk above dir, so it analyzes the recipe as
// shipped and cannot be steered to a different project by the caller's
// environment.
func loadRecipeProject(ctx context.Context, dir string) (*composetypes.Project, error) {
	files, err := recipeComposeFiles(dir)
	if err != nil {
		return nil, err
	}
	opts, err := composecli.NewProjectOptions(
		files,
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
	return project, nil
}

// readRecipePolicy reads and parses the recipe's vaka.yaml through a
// confinement root (so an escaping symlink is refused, and the read is
// bounded).
func readRecipePolicy(dir string) (*policy.ServicePolicy, error) {
	root, err := OpenSafeRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("recipe policy: %w", err)
	}
	defer root.Close()
	data, err := root.ReadFileLimited("vaka.yaml", maxPolicyBytes)
	if err != nil {
		return nil, fmt.Errorf("recipe policy: %w", err)
	}
	pol, err := policy.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("recipe policy: %w", err)
	}
	return pol, nil
}

// LintDir loads the recipe's own compose project (base + override) and
// vaka.yaml and computes the policy summary and risk flags from those files.
func LintDir(ctx context.Context, dir string) (*LocalPolicySummary, error) {
	project, err := loadRecipeProject(ctx, dir)
	if err != nil {
		return nil, err
	}
	pol, err := readRecipePolicy(dir)
	if err != nil {
		return nil, err
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
