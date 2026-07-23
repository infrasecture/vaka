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
	// Authoritative minVakaVersion enforcement against the digest-bound
	// manifest (the index copy is only an advisory pre-download fast-fail).
	if err := manifest.CheckMinVakaVersion(want.VakaVersion); err != nil {
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
// does not honor COMPOSE_FILE, does not import the ambient OS environment, does
// not load the top-level .env (WithEnvFiles/WithDotEnv are never called), and
// does not walk above dir, so it analyzes the recipe as shipped and cannot be
// steered to a different project by the caller's environment.
//
// One exception is intrinsic to compose-go and cannot be disabled through
// project options: when a recipe uses `include:` and the include declares no
// env_file, compose-go auto-loads <project_directory>/.env for that included
// model. checkComposeReferences confines project_directory to the recipe tree,
// so this read stays in-tree, but it does mean a recipe that uses include is
// not fully .env-independent (design §7 records the carve-out).
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

// composeReferences groups the host-file paths a single compose document
// references, by how compose-go resolves each. leaves are ordinary file reads
// resolved against the document's working directory; extends and includes are
// themselves compose files that compose-go loads recursively, each establishing
// a working directory for its own content.
type composeReferences struct {
	leaves   []string       // env_file, label_file, configs/secrets file, include env_file
	extends  []string       // extends.file targets
	includes []includeEntry // include entries (path list + project_directory)
}

// includeEntry is one `include:` item. compose-go resolves the included
// content — and auto-loads <project_directory>/.env — relative to
// projectDirectory (or, when unset, the directory of the included file), NOT
// the directory of the including file.
type includeEntry struct {
	paths            []string
	projectDirectory string // "" when unset
}

// scanUnit is a compose file to scan plus the working directory its own
// references resolve against and whether it is one of the recipe's top-level
// files. topLevel gates includes: compose-go resolves a *nested* include's
// env_file / project_directory against a relative working dir that ends up
// interpreted against the process CWD (verified against compose-go v2.10.2),
// which a static in-tree check cannot model — so includes are permitted only in
// top-level files. The (file, workDir, topLevel) triple is the visited key.
type scanUnit struct {
	file     string
	workDir  string
	topLevel bool
}

// maxComposeScanUnits bounds the include/extends graph traversal. The visited
// set already dedups scan units over a finite extracted tree; this cap fails
// CLOSED on a pathological graph rather than ever exiting with files left
// unscanned (an earlier depth cap could exit while work remained).
const maxComposeScanUnits = 1024

// checkComposeReferences rejects compose files that reference host files
// outside the recipe directory. compose-go resolves env_file, label_file,
// extends.file, include (path / env_file / project_directory), and
// configs/secrets `file:` as ordinary reads (absolute or parent-relative paths
// included), so an unconfined recipe could make vaka read arbitrary host files
// during `get`, before the user runs anything.
//
// Each referenced path is checked against the recipe root, resolved relative to
// the *working directory* compose-go would use for the file that names it — the
// recipe root for the top-level files, the extended file's own directory for an
// extends.file target, and the included file's own directory for a top-level
// include. Referenced in-tree compose files are scanned recursively so their
// own references are confined too.
//
// Two classes are refused outright because static confinement cannot model
// them safely: a path containing `$` (interpolated at load time, so its real
// target is unknown here), and includes that are nested, multi-path, or carry
// project_directory (compose-go resolves those against a relative/CWD-relative
// working directory — see scanUnit). Recipes use only top-level, single-path
// includes; deeper composition is a future feature (design §10).
//
// The check is also symlink-aware: a lexically in-tree path whose real
// (symlink-resolved) location escapes the recipe is refused, since compose-go
// follows symlinks while a lexical clean/prefix test would not.
func checkComposeReferences(dir string, files []string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	// realRoot resolves symlinks in the recipe path itself (e.g. /tmp -> /private
	// on macOS) so the containment test below compares like with like.
	realRoot := root
	if rr, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = rr
	}

	var bad []string
	var unsupported []string
	visited := make(map[string]bool)
	// Top-level files resolve relative to the project working directory (root).
	queue := make([]scanUnit, 0, len(files))
	for _, f := range files {
		if abs, err := filepath.Abs(f); err == nil {
			queue = append(queue, scanUnit{file: abs, workDir: root, topLevel: true})
		}
	}
	scanned := 0
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		key := fmt.Sprintf("%s\x00%s\x00%t", u.file, u.workDir, u.topLevel)
		if visited[key] {
			continue
		}
		visited[key] = true
		scanned++
		if scanned > maxComposeScanUnits {
			return fmt.Errorf("recipe compose include/extends graph is too large to validate (> %d files); refusing", maxComposeScanUnits)
		}
		// Real-path-check the compose file itself before reading it: a compose
		// file that is (or is reached through) a symlink escaping the recipe
		// must not be read, even though recipeComposeFiles selected it via a
		// symlink-following os.Stat. os.ReadFile below would otherwise follow
		// the link out of the tree.
		if real, err := filepath.EvalSymlinks(u.file); err == nil {
			if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
				bad = append(bad, fmt.Sprintf("%s (compose file resolves outside the recipe directory)", displayPath(root, u.file)))
				continue
			}
		}
		data, err := os.ReadFile(u.file)
		if err != nil {
			// A missing recursively-referenced file surfaces as a compose-go
			// load error later; top-level files were found by
			// recipeComposeFiles. Don't fail confinement on it.
			continue
		}
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			continue // let compose-go surface the real parse error
		}
		label := displayPath(root, u.file)
		// external records p if it escapes the recipe (lexically, or via a
		// symlink whose real target escapes); returns true when it did.
		external := func(p string) bool {
			if reason := externalReason(root, u.workDir, p); reason != "" {
				bad = append(bad, fmt.Sprintf("%s: %q (%s)", label, p, reason))
				return true
			}
			resolved := filepath.Clean(filepath.Join(u.workDir, p))
			if real, err := filepath.EvalSymlinks(resolved); err == nil {
				if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
					bad = append(bad, fmt.Sprintf("%s: %q (symlink escapes the recipe directory)", label, p))
					return true
				}
			}
			return false
		}
		enqueue := func(target, workDir string, topLevel bool) {
			if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
				queue = append(queue, scanUnit{file: target, workDir: workDir, topLevel: topLevel})
			}
		}

		refs := collectComposeFileRefs(doc)
		for _, p := range refs.leaves {
			external(p)
		}
		for _, ex := range refs.extends {
			if external(ex) {
				continue
			}
			// The extended file's content resolves relative to its own dir.
			target := filepath.Clean(filepath.Join(u.workDir, ex))
			enqueue(target, filepath.Dir(target), false)
		}
		for _, inc := range refs.includes {
			switch {
			case !u.topLevel:
				unsupported = append(unsupported, fmt.Sprintf(
					"%s: nested include (an include may appear only in the recipe's top-level compose files)", label))
				continue
			case inc.projectDirectory != "":
				unsupported = append(unsupported, fmt.Sprintf(
					"%s: include with project_directory", label))
				continue
			case len(inc.paths) != 1:
				unsupported = append(unsupported, fmt.Sprintf(
					"%s: multi-file include (use one path per include entry)", label))
				continue
			}
			p := inc.paths[0]
			if external(p) {
				continue
			}
			// A top-level, single-path include resolves its content against the
			// included file's own (absolute, in-tree) directory — the case
			// compose-go models without CWD ambiguity.
			target := filepath.Clean(filepath.Join(u.workDir, p))
			enqueue(target, filepath.Dir(target), false)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return fmt.Errorf(
			"recipe compose uses unsupported include features (recipes use only top-level, single-path includes; see design §10):\n\t%s",
			strings.Join(unsupported, "\n\t"))
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

// collectComposeFileRefs extracts the host-file paths a compose document
// references, grouped by how compose-go resolves them: leaf reads (env_file,
// label_file, configs/secrets file, include env_file), extends.file targets,
// and include entries (each carrying its path list and project_directory).
func collectComposeFileRefs(doc map[string]any) composeReferences {
	var refs composeReferences
	addLeaf := func(ss ...string) {
		for _, s := range ss {
			if s != "" {
				refs.leaves = append(refs.leaves, s)
			}
		}
	}

	for _, inc := range asList(doc["include"]) {
		switch v := inc.(type) {
		case string:
			refs.includes = append(refs.includes, includeEntry{paths: []string{v}})
		case map[string]any:
			entry := includeEntry{paths: stringOrList(v["path"])}
			if pd, ok := v["project_directory"].(string); ok {
				entry.projectDirectory = pd
			}
			refs.includes = append(refs.includes, entry)
			// An include's own env_file resolves against the including file's
			// working dir, so it is a leaf of this document.
			addLeaf(envFilePaths(v["env_file"])...)
		}
	}

	if services, ok := doc["services"].(map[string]any); ok {
		for _, svcAny := range services {
			svc, ok := svcAny.(map[string]any)
			if !ok {
				continue
			}
			addLeaf(envFilePaths(svc["env_file"])...)
			addLeaf(envFilePaths(svc["label_file"])...)
			if ext, ok := svc["extends"].(map[string]any); ok {
				if f, ok := ext["file"].(string); ok && f != "" {
					refs.extends = append(refs.extends, f)
				}
			}
		}
	}

	for _, section := range []string{"configs", "secrets"} {
		if m, ok := doc[section].(map[string]any); ok {
			for _, itemAny := range m {
				if item, ok := itemAny.(map[string]any); ok {
					if f, ok := item["file"].(string); ok {
						addLeaf(f)
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
