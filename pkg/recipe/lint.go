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
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/policy"
)

// requiredRecipeFiles must be present for a directory to be a well-formed
// recipe. README is documentation and is not required for the recipe to run.
var requiredRecipeFiles = []string{"recipe.yaml", "vaka.yaml"}

// ValidateStaged checks that dir is a well-formed, runnable recipe: the
// required files are present, recipe.yaml parses and schema-validates (and,
// when want is set, matches the resolved index entry's name/version), the
// compose project (base + override) loads, vaka.yaml parses, and the egress
// policy validates against the compose services. It is run on the freshly
// extracted staging tree before an install or update commits, so a malformed,
// mislabeled, or invalid published artifact is refused (fail closed) instead
// of replacing a working recipe and being reported as a successful update.
func ValidateStaged(ctx context.Context, dir string, want ExpectedIdentity) error {
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
	// Parse the manifest through the confinement root (an escaping symlink or a
	// directory named recipe.yaml is refused, and the read is bounded). Merely
	// existing is not enough — a mistyped or mislabeled manifest must fail.
	manifestData, err := root.ReadFileLimited("recipe.yaml", maxManifestBytes)
	root.Close()
	if err != nil {
		return fmt.Errorf("recipe manifest: %w", err)
	}
	manifest, err := ParseManifest(manifestData)
	if err != nil {
		return err
	}
	if err := manifest.CheckIdentity(want); err != nil {
		return err
	}

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
	// FlagUnpinnedImage marks a service whose image is not pinned by digest
	// (implicit latest or a mutable tag), so the recipe's verified tarball
	// digest does not determine the container code that runs. Advisory: it is
	// surfaced but does not fail registry CI (mutable tags are common).
	FlagUnpinnedImage = "unpinned-image"
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

// firstExisting returns the first name in names that exists under dir, or ""
// — matching compose-go's findFiles winner selection exactly: os.Stat
// (follows symlinks, so a dangling symlink is skipped like runtime) and any
// successful stat wins (a directory is selected, then fails to load, just as
// compose-go would). Using Lstat / excluding directories here would diverge
// from what `vaka up` loads.
func firstExisting(dir string, names []string) string {
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
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
	// Preflight: reject compose files that reference host files outside the
	// recipe directory (env_file / extends.file / include / configs / secrets)
	// BEFORE compose-go loads them — LoadProject would otherwise read those
	// absolute or parent-relative paths, and a risk flag emitted afterwards
	// would already be too late.
	if err := checkComposeReferences(dir, files); err != nil {
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

// composeRef is one host-file path a compose document references, plus whether
// that path is itself a compose file (include / extends.file) that must be
// scanned recursively.
type composeRef struct {
	path    string
	compose bool
}

// maxComposeScanDepth bounds recursion into included/extended compose files.
// Real recipes nest at most a level or two; the bound plus the visited set
// guarantee termination on cyclic or adversarial include graphs.
const maxComposeScanDepth = 16

// checkComposeReferences rejects compose files that reference host files
// outside the recipe directory. compose-go resolves env_file, label_file,
// extends.file, include (path / env_file / project_directory), and
// configs/secrets `file:` as ordinary reads (absolute or parent-relative paths
// included), so an unconfined recipe could make vaka read arbitrary host files
// during `get`, before the user runs anything.
//
// Every referenced path is checked against the recipe root, resolved relative
// to the directory of the file that names it. In-tree include/extends targets
// are scanned recursively (bounded, cycle-guarded), because compose-go loads
// them too and they can carry their own external references. Any path that
// still contains a `$` is refused: it is interpolated at load time
// (WithInterpolation), so its real target cannot be confined statically, and
// recipes have no need to compute file-reference paths from the environment.
func checkComposeReferences(dir string, files []string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	var bad []string
	visited := make(map[string]bool)
	// queue holds absolute compose-file paths still to scan; depth caps
	// recursion independently of the visited set.
	queue := make([]string, 0, len(files))
	for _, f := range files {
		if abs, err := filepath.Abs(f); err == nil {
			queue = append(queue, abs)
		}
	}
	for depth := 0; len(queue) > 0 && depth <= maxComposeScanDepth; depth++ {
		next := queue
		queue = nil
		for _, absF := range next {
			if visited[absF] {
				continue
			}
			visited[absF] = true
			data, err := os.ReadFile(absF)
			if err != nil {
				// A missing recursively-referenced file surfaces as a
				// compose-go load error later; top-level files were already
				// found by recipeComposeFiles. Don't fail confinement on it.
				continue
			}
			var doc map[string]any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				continue // let compose-go surface the real parse error
			}
			base := filepath.Dir(absF)
			label := displayPath(root, absF)
			for _, ref := range collectComposeFileRefs(doc) {
				if reason := externalReason(root, base, ref.path); reason != "" {
					bad = append(bad, fmt.Sprintf("%s: %q (%s)", label, ref.path, reason))
					continue
				}
				if ref.compose {
					resolved := filepath.Clean(filepath.Join(base, ref.path))
					if fi, err := os.Stat(resolved); err == nil && !fi.IsDir() {
						queue = append(queue, resolved)
					}
				}
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf(
			"recipe compose references files outside the recipe directory (env_file/label_file/extends/include/configs/secrets); this is not allowed:\n\t%s",
			strings.Join(bad, "\n\t"))
	}
	return nil
}

// externalReason returns "" if p (interpreted relative to base) is a safe
// in-tree reference, or a short reason why it is refused.
func externalReason(root, base, p string) string {
	switch {
	case p == "":
		return ""
	case strings.Contains(p, "$"):
		return "interpolated path"
	case filepath.IsAbs(p):
		return "absolute path"
	}
	resolved := filepath.Clean(filepath.Join(base, p))
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "escapes the recipe directory"
	}
	return ""
}

// displayPath renders absF relative to root for error messages, falling back
// to the base name.
func displayPath(root, absF string) string {
	if rel, err := filepath.Rel(root, absF); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return filepath.Base(absF)
}

// collectComposeFileRefs extracts every host-file path a compose document
// references via env_file, label_file, extends.file, include (path / env_file
// / project_directory), and configs/secrets file.
func collectComposeFileRefs(doc map[string]any) []composeRef {
	var refs []composeRef
	add := func(compose bool, ss ...string) {
		for _, s := range ss {
			if s != "" {
				refs = append(refs, composeRef{path: s, compose: compose})
			}
		}
	}

	for _, inc := range asList(doc["include"]) {
		switch v := inc.(type) {
		case string:
			add(true, v)
		case map[string]any:
			add(true, stringOrList(v["path"])...)
			add(false, envFilePaths(v["env_file"])...)
			if pd, ok := v["project_directory"].(string); ok {
				add(false, pd)
			}
		}
	}

	if services, ok := doc["services"].(map[string]any); ok {
		for _, svcAny := range services {
			svc, ok := svcAny.(map[string]any)
			if !ok {
				continue
			}
			add(false, envFilePaths(svc["env_file"])...)
			add(false, envFilePaths(svc["label_file"])...)
			if ext, ok := svc["extends"].(map[string]any); ok {
				if f, ok := ext["file"].(string); ok {
					add(true, f)
				}
			}
		}
	}

	for _, section := range []string{"configs", "secrets"} {
		if m, ok := doc[section].(map[string]any); ok {
			for _, itemAny := range m {
				if item, ok := itemAny.(map[string]any); ok {
					if f, ok := item["file"].(string); ok {
						add(false, f)
					}
				}
			}
		}
	}
	return refs
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// stringOrList flattens a value that may be a string or a list of strings.
func stringOrList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// envFilePaths handles env_file's shapes: string, list of strings, or list of
// {path: <string>} objects.
func envFilePaths(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			switch ev := e.(type) {
			case string:
				out = append(out, ev)
			case map[string]any:
				if p, ok := ev["path"].(string); ok {
					out = append(out, p)
				}
			}
		}
		return out
	}
	return nil
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
	// A service that pulls an image (as opposed to building one) should pin it
	// by digest; without @sha256: the tag is mutable and the recipe's tarball
	// digest does not determine the code that runs.
	if svc.Image != "" && !strings.Contains(svc.Image, "@sha256:") {
		flag(svcName, FlagUnpinnedImage)
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
