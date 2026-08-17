// cmd/vaka/intercept.go
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/internal/runtimebundle"
	"vaka.dev/vaka/pkg/compose"
	"vaka.dev/vaka/pkg/policy"
)

// vakaInitLabel is the compose service label that signals the image already
// ships the vaka-init binaries at /opt/vaka/sbin/. When present, the service
// does not depend on the __vaka-init volume helper container.
const vakaInitLabel = "agent.vaka.init"

// vakaInitBaseImage is the image repository for the container runtime bundle.
const vakaInitBaseImage = "emsi/vaka-init"

// Test hooks: overridden in unit tests to avoid real Docker side effects.
var (
	newDockerServices    = NewDockerServices
	execDockerComposeFn  = execDockerCompose
	runtimeBundleVersion = runtimebundle.Version()
)

func vakaInitImageReference() string {
	return vakaInitBaseImage + ":" + runtimebundle.ImageTagPrefix + runtimeBundleVersion
}

// defaultDockerCaps is the set of capabilities present in a default Docker
// container (no cap_drop, no cap_add). NET_ADMIN is notably absent.
var defaultDockerCaps = map[string]bool{
	"CAP_CHOWN": true, "CAP_DAC_OVERRIDE": true, "CAP_FOWNER": true,
	"CAP_FSETID": true, "CAP_KILL": true, "CAP_SETGID": true,
	"CAP_SETUID": true, "CAP_SETPCAP": true, "CAP_NET_BIND_SERVICE": true,
	"CAP_NET_RAW": true, "CAP_SYS_CHROOT": true, "CAP_MKNOD": true,
	"CAP_AUDIT_WRITE": true, "CAP_SETFCAP": true,
}

// composeVerbClass classifies how a compose subcommand is handled.
type composeVerbClass int

const (
	verbRender    composeVerbClass = iota // full policy injection
	verbReference                         // metadata-only override
	verbMetadata                          // direct proxy; no project or override required
	verbExec                              // live-target inspection + runtime trampoline
	verbUnknown                           // unreviewed Compose surface; fail closed
)

type composeCommandSpec struct {
	class         composeVerbClass
	ensureRuntime bool
}

// composeCommandSpecs is the security boundary for Compose dispatch. Commands
// known not to create containers use the lightweight reference path. Commands
// that can create or recreate containers require a full policy render.
var composeCommandSpecs = map[string]composeCommandSpec{
	"attach":  {class: verbReference},
	"bridge":  {class: verbMetadata},
	"build":   {class: verbReference},
	"commit":  {class: verbReference},
	"config":  {class: verbReference},
	"cp":      {class: verbReference},
	"create":  {class: verbRender},
	"down":    {class: verbReference},
	"events":  {class: verbReference},
	"exec":    {class: verbExec},
	"export":  {class: verbReference},
	"help":    {class: verbMetadata},
	"images":  {class: verbReference},
	"kill":    {class: verbReference},
	"logs":    {class: verbReference},
	"ls":      {class: verbMetadata},
	"pause":   {class: verbReference},
	"port":    {class: verbReference},
	"ps":      {class: verbReference},
	"publish": {class: verbReference},
	"pull":    {class: verbReference, ensureRuntime: true},
	"push":    {class: verbReference},
	"restart": {class: verbReference},
	"rm":      {class: verbReference},
	"run":     {class: verbRender},
	"scale":   {class: verbRender},
	"start":   {class: verbReference},
	"stats":   {class: verbReference},
	"stop":    {class: verbReference},
	"top":     {class: verbReference},
	"unpause": {class: verbReference},
	"up":      {class: verbRender},
	"version": {class: verbMetadata},
	"volumes": {class: verbReference},
	"wait":    {class: verbReference},
	"watch":   {class: verbRender},
}

// composeCommandSpecFor fails closed on unknown future verbs. Full rendering
// protects container startup, but cannot make an unreviewed verb safe if it
// creates an exec process inside an existing container.
func composeCommandSpecFor(verb string) composeCommandSpec {
	if spec, ok := composeCommandSpecs[verb]; ok {
		return spec
	}
	return composeCommandSpec{class: verbUnknown}
}

func classifyComposeVerb(verb string) composeVerbClass {
	return composeCommandSpecFor(verb).class
}

// execDockerCompose executes docker compose with the given args.
// When overrideYAML is non-empty it is injected via -f /dev/fd/3 (with default
// compose files also passed via -f so compose merges them correctly). The YAML
// bytes are streamed through an inherited pipe FD so stdin remains attached to
// the user's terminal.
// extraEnv, when non-nil, is appended to the inherited environment.
func execDockerCompose(inv *ComposeInvocation, overrideYAML string, extraEnv []string) error {
	var dockerArgs []string
	if overrideYAML != "" {
		defaults := []string{}
		if len(inv.GlobalFiles) == 0 {
			resolved, err := resolveComposeInput(inv)
			if err != nil {
				if classifyComposeVerb(inv.Subcommand) == verbReference {
					return fmt.Errorf("reference command requires compose configuration (%w); run from the project directory or pass -f/--project-directory", err)
				}
				return err
			}
			defaults = resolved.Files
		}
		dockerArgs = injectFDOverride(inv, defaults)
	} else {
		dockerArgs = inv.dockerComposeArgs()
	}
	c := exec.Command("docker", dockerArgs...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if extraEnv != nil {
		c.Env = append(os.Environ(), extraEnv...)
	}
	if overrideYAML == "" {
		return c.Run()
	}

	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create compose override pipe: %w", err)
	}
	c.ExtraFiles = []*os.File{r} // ExtraFiles[0] becomes child FD 3.

	if err := c.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return err
	}
	_ = r.Close()

	writeErrCh := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(w, overrideYAML)
		_ = w.Close()
		writeErrCh <- writeErr
	}()

	waitErr := c.Wait()
	writeErr := <-writeErrCh
	if waitErr != nil {
		return waitErr
	}
	if writeErr != nil {
		return fmt.Errorf("stream compose override: %w", writeErr)
	}
	return nil
}

// runFull handles every full-render Compose command. It loads and validates
// vaka.yaml, resolves the runtime image when needed, builds the full Compose
// override, and delegates to execDockerCompose.
func runFull(vakaFile string, inv *ComposeInvocation, vakaInitPresent bool, pullPolicy PullPolicy) error {
	ctx := context.Background()
	ds, err := newDockerServices(inv, pullPolicy)
	if err != nil {
		return err
	}
	if err := ds.CheckRuntimeCompatibility(ctx); err != nil {
		return err
	}
	overrideYAML, extraEnv, err := buildInjectionOverride(ctx, ds, vakaFile, inv, vakaInitPresent)
	if err != nil {
		return err
	}
	legacyState, captureErr := captureLegacyRuntime(ctx, ds, inv.ResolvedProjectName)
	if captureErr != nil {
		fmt.Fprintf(os.Stderr, "vaka: warning: cannot inspect legacy runtime state: %v\n", captureErr)
	}
	if !legacyState.Empty() {
		extraEnv = append(extraEnv, "COMPOSE_IGNORE_ORPHANS=true")
	}
	execErr := execDockerComposeFn(inv, overrideYAML, extraEnv)
	cleanupLegacyRuntime(ctx, ds, legacyState)
	return execErr
}

// buildInjectionOverride builds the compose override and per-service secret env
// payload from the same shared path used by full injection commands.
//
// Side effects are intentional and shared with runFull: pre-build and exact
// runtime-image resolution happen here so show-compose cannot drift.
func buildInjectionOverride(
	ctx context.Context,
	ds DockerServices,
	vakaFile string,
	inv *ComposeInvocation,
	vakaInitPresent bool,
) (overrideYAML string, extraEnv []string, err error) {
	composeInput, err := resolveComposeInput(inv)
	if err != nil {
		return "", nil, err
	}
	p, project, err := loadAndValidateResolved(vakaFile, composeInput)
	if err != nil {
		return "", nil, err
	}
	if err := validateManagedExecutionSurfaces(p, project); err != nil {
		return "", nil, err
	}
	if vakaInitPresent && len(p.Services) > 0 {
		return "", nil, fmt.Errorf("--vaka-init-present is not supported for managed services: the exec security boundary requires Vaka's verified read-only runtime mount")
	}
	if inv.Subcommand == "run" {
		if err := validateRunInvocation(inv, p); err != nil {
			return "", nil, err
		}
	}
	if inv.Subcommand == "up" || inv.Subcommand == "create" {
		noRecreate, err := composeBoolOptionEnabled(inv.PostSubcommand, "--no-recreate", "")
		if err != nil {
			return "", nil, err
		}
		if noRecreate && len(p.Services) > 0 {
			return "", nil, fmt.Errorf("compose %s --no-recreate can reuse containers with an older unsafe runtime; remove --no-recreate so managed services are recreated", inv.Subcommand)
		}
	}
	if inv.Subcommand == "watch" {
		noUp, err := composeBoolOptionEnabled(inv.PostSubcommand, "--no-up", "")
		if err != nil {
			return "", nil, err
		}
		if noUp && len(p.Services) > 0 {
			return "", nil, fmt.Errorf("compose watch --no-up can reuse containers with an older unsafe runtime; remove --no-up so managed services are recreated")
		}
	}
	inv.ResolvedProjectName = project.Name

	// Consume image-refresh operations before inspection. The final Compose
	// invocation receives exact image IDs, so it must not rebuild or repull a
	// different image after Vaka has validated healthchecks and image volumes.
	pullValue, pullRequested, err := composePullOption(inv)
	if err != nil {
		return "", nil, err
	}
	forceRebuild, err := composeBuildRequested(inv)
	if err != nil {
		return "", nil, err
	}
	noBuild := false
	if inv.Subcommand != "run" {
		noBuild, err = composeBoolOptionEnabled(inv.PostSubcommand, "--no-build", "")
		if err != nil {
			return "", nil, err
		}
	}
	if forceRebuild && noBuild {
		return "", nil, fmt.Errorf("compose options --build and --no-build cannot both be enabled")
	}
	applyManagedCLI := true
	if inv.Subcommand == "run" {
		parsed, parseErr := parseRun(inv.PostSubcommand)
		if parseErr != nil {
			return "", nil, parseErr
		}
		selected, selectErr := selectRunServices(project, parsed)
		if selectErr != nil {
			return "", nil, fmt.Errorf("select Compose run services for image preparation: %w", selectErr)
		}
		applyManagedCLI = false
		for name := range selected.Services {
			if _, managed := p.Services[name]; managed {
				applyManagedCLI = true
				break
			}
		}
	}
	managedPullRequested := pullRequested && applyManagedCLI
	managedForceRebuild := forceRebuild && applyManagedCLI
	preparation, err := planManagedImagePreparation(ctx, ds, p.Services, project, pullValue, managedPullRequested, managedForceRebuild, noBuild)
	if err != nil {
		return "", nil, err
	}
	unmanaged, err := planSelectedUnmanagedRefresh(project, p.Services, inv, pullValue, pullRequested && applyManagedCLI, forceRebuild && applyManagedCLI, noBuild)
	if err != nil {
		return "", nil, err
	}
	preparation.pullAlways = append(preparation.pullAlways, unmanaged.pullAlways...)
	preparation.pullOrBuild = append(preparation.pullOrBuild, unmanaged.pullOrBuild...)
	for name := range unmanaged.forceBuild {
		preparation.forceBuild[name] = true
	}
	for _, pull := range []struct {
		policy   string
		services []string
	}{
		{policy: "always", services: preparation.pullAlways},
		{policy: "missing", services: preparation.pullMissing},
	} {
		if len(pull.services) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "vaka: pre-pulling services with policy %s before image preparation: %v\n", pull.policy, pull.services)
		pullArgs := append([]string{}, inv.ComposeGlobals...)
		pullArgs = append(pullArgs, "pull", "--policy", pull.policy)
		pullArgs = append(pullArgs, pull.services...)
		if err := execDockerComposeFn(&ComposeInvocation{Args: pullArgs}, "", nil); err != nil {
			return "", nil, fmt.Errorf("pre-pull (%s): %w", pull.policy, err)
		}
	}
	for _, pull := range preparation.pullOrBuild {
		fmt.Fprintf(os.Stderr, "vaka: pre-pulling buildable service %s with policy %s before image preparation\n", pull.service, pull.policy)
		pullArgs := append([]string{}, inv.ComposeGlobals...)
		pullArgs = append(pullArgs, "pull", "--policy", pull.policy, pull.service)
		if err := execDockerComposeFn(&ComposeInvocation{Args: pullArgs}, "", nil); err != nil {
			if noBuild {
				return "", nil, fmt.Errorf("pre-pull %s (%s): %w", pull.service, pull.policy, err)
			}
			fmt.Fprintf(os.Stderr, "vaka: pull failed for buildable service %s; building it before inspection\n", pull.service)
			preparation.forceBuild[pull.service] = true
		}
	}

	// Build-only managed services must always be built so they have an exact
	// inspectable image. --build is consumed for every buildable managed service
	// before inspection; this preserves final-image identity without touching
	// unrelated unmanaged services.
	toBuild, err := servicesNeedingPrebuild(ctx, ds, p.Services, project, managedForceRebuild, preparation.forceBuild, noBuild)
	if err != nil {
		return "", nil, err
	}
	if len(toBuild) > 0 {
		fmt.Fprintf(os.Stderr, "vaka: pre-building services to resolve entrypoints: %v\n", toBuild)
		buildArgs := append([]string{}, inv.ComposeGlobals...)
		buildArgs = append(buildArgs, "build")
		buildArgs = append(buildArgs, toBuild...)
		buildInv := &ComposeInvocation{
			Args: buildArgs,
		}
		if err := execDockerComposeFn(buildInv, "", nil); err != nil {
			return "", nil, fmt.Errorf("pre-build: %w", err)
		}
	}

	var entries []compose.ServiceEntry
	extraEnv = nil

	for svcName, svc := range p.Services {
		composeSvc, ok := project.Services[svcName]
		if !ok {
			return "", nil, fmt.Errorf("service %q: not found in compose files %v", svcName, composeInput.Files)
		}
		if strings.TrimSpace(composeSvc.Image) == "" && composeSvc.Build != nil {
			composeSvc.Image = project.Name + "-" + svcName
		}
		composeSvc.PullPolicy = preparation.effectivePullPolicy[svcName]

		rt, err := ds.ResolveRuntime(ctx, svcName, composeSvc)
		if err != nil {
			return "", nil, err
		}

		if svc.Runtime == nil {
			svc.Runtime = &policy.RuntimeConfig{}
		}
		restoreUser := strings.TrimSpace(composeSvc.User)
		if restoreUser == "" {
			restoreUser = strings.TrimSpace(rt.ImageUser)
		}
		capPlan := computeCapabilityPlan(composeSvc, svc.Runtime, restoreUser)
		svc.Runtime.DropCaps = capPlan.Drop
		fmt.Fprintf(os.Stderr, "vaka: service %s: dropCaps: %v\n", svcName, svc.Runtime.DropCaps)

		sliced, err := policy.SliceService(p, svcName)
		if err != nil {
			return "", nil, err
		}
		sliced.GeneratedBy = "vaka/" + version
		sliced.RequiredRuntimeVersion = runtimeBundleVersion
		sliced.Services[svcName].User = restoreUser
		sliced.Services[svcName].GroupAdd = append([]string(nil), composeSvc.GroupAdd...)
		policyRevision, err := policy.Revision(sliced)
		if err != nil {
			return "", nil, fmt.Errorf("compute policy revision for %s: %w", svcName, err)
		}

		raw, err := yaml.Marshal(sliced)
		if err != nil {
			return "", nil, fmt.Errorf("marshal policy for %s: %w", svcName, err)
		}

		envKey := policyPayloadEnvironmentName(svcName)
		extraEnv = append(extraEnv, envKey+"="+base64.StdEncoding.EncodeToString(raw))

		entries = append(entries, compose.ServiceEntry{
			Name:             svcName,
			ImageID:          rt.ImageID,
			Entrypoint:       rt.Entrypoint,
			Command:          rt.Command,
			CapDelta:         capPlan.Add,
			EnvVarName:       envKey,
			PolicyRevision:   policyRevision,
			Healthcheck:      rt.Healthcheck,
			HealthcheckShell: rt.HealthcheckShell,
			OptOut:           false,
		})
	}

	// Resolve the runtime image only when at least one service needs it. The
	// mutable tag is used for lookup and repair; Compose receives the exact local
	// image ID or its compatibility prefix, never that tag.
	needsInjection := len(entries) > 0
	runtimeMount := compose.RuntimeMount{Version: runtimeBundleVersion}
	if needsInjection {
		resolvedImage, err := ds.ResolveRuntimeImage(ctx, vakaInitImageReference(), runtimeBundleVersion, true)
		if err != nil {
			return "", nil, err
		}
		runtimeMount.ImageID = resolvedImage.ID
		runtimeMount.Source = resolvedImage.MountSource
	}

	overrideYAML, err = compose.BuildOverride(entries, runtimeMount, unmanaged.prepared...)
	if err != nil {
		return "", nil, fmt.Errorf("build override: %w", err)
	}
	consumeBuild := forceRebuild && (inv.Subcommand != "run" || applyManagedCLI)
	pullMode := pullValue
	consumePull := pullRequested && (pullMode == composetypes.PullPolicyAlways || pullMode == composetypes.PullPolicyBuild) && (inv.Subcommand != "run" || applyManagedCLI)
	if consumeBuild || consumePull {
		if err := consumeComposeImageRefreshOptions(inv, consumeBuild, consumePull); err != nil {
			return "", nil, err
		}
	}
	return overrideYAML, extraEnv, nil
}

// policyPayloadEnvironmentName encodes the exact service name instead of
// normalizing it. Compose permits names such as foo-bar and foo_bar in the
// same project, so normalization would make their policy payloads collide.
func policyPayloadEnvironmentName(service string) string {
	return "VAKA_SERVICE_" + strings.ToUpper(hex.EncodeToString([]byte(service))) + "_CONF"
}

// referenceOverrideYAML returns the metadata-only override used by Compose
// commands that do not create containers.
func referenceOverrideYAML() (string, error) {
	return compose.BuildReferenceOverride(runtimeBundleVersion)
}

// runReference handles all reference commands with the same FD-injected
// Compose path but without evaluating service policy.
func runReference(inv *ComposeInvocation) error {
	overrideYAML, err := referenceOverrideYAML()
	if err != nil {
		return fmt.Errorf("build compose reference override: %w", err)
	}
	if inv.Subcommand != "down" {
		return execDockerComposeFn(inv, overrideYAML, nil)
	}

	ctx := context.Background()
	projectName, projectErr := resolveComposeProjectName(ctx, inv)
	ds, dockerErr := newDockerServices(inv, PullNever)
	if projectErr != nil || dockerErr != nil {
		return execDockerComposeFn(inv, overrideYAML, nil)
	}
	legacyState, captureErr := captureLegacyRuntime(ctx, ds, projectName)
	if captureErr != nil {
		fmt.Fprintf(os.Stderr, "vaka: warning: cannot inspect legacy runtime state: %v\n", captureErr)
	}
	extraEnv := []string(nil)
	if !legacyState.Empty() {
		extraEnv = append(extraEnv, "COMPOSE_IGNORE_ORPHANS=true")
	}
	execErr := execDockerComposeFn(inv, overrideYAML, extraEnv)
	cleanupLegacyRuntime(ctx, ds, legacyState)
	return execErr
}

// ensureReferenceRuntime preserves `vaka compose pull` as an offline-prefetch
// workflow without loading policy or inspecting service images. The runtime is
// resolved on the same Docker target as the subsequent Compose invocation.
func ensureReferenceRuntime(inv *ComposeInvocation) error {
	ctx := context.Background()
	ds, err := newDockerServices(inv, PullNever)
	if err != nil {
		return err
	}
	if err := ds.CheckRuntimeCompatibility(ctx); err != nil {
		return err
	}
	_, err = ds.ResolveRuntimeImage(ctx, vakaInitImageReference(), runtimeBundleVersion, true)
	return err
}

// servicesNeedingPrebuild returns the sorted list of services whose image must
// be built before ResolveRuntime can inspect it. A service qualifies when:
//   - it needs image defaults for entrypoint/cmd and/or user fallback, AND
//   - the compose definition has a build: section (we can build it), AND
//   - the resolved image is not already available locally OR forceRebuild is set.
//
// forceRebuild is true when the user passed --build to the final compose
// command. In that case the existing local image is about to be replaced by a
// fresh build, so inspecting the stale copy for ENTRYPOINT/CMD/USER would produce
// incorrect command vectors. Prebuilding every eligible managed service ensures
// ResolveRuntime sees the post-build image.
func servicesNeedingPrebuild(
	ctx context.Context,
	ds DockerServices,
	policySvcs map[string]*policy.ServiceConfig,
	project *composetypes.Project,
	forceRebuild bool,
	forceServices map[string]bool,
	noBuild bool,
) ([]string, error) {
	var out []string
	candidates := map[string]bool{}
	for svcName := range policySvcs {
		candidates[svcName] = true
	}
	for svcName := range forceServices {
		candidates[svcName] = true
	}
	for svcName := range candidates {
		composeSvc, ok := project.Services[svcName]
		if !ok {
			continue
		}
		if composeSvc.Build == nil {
			continue
		}
		forced := forceRebuild || forceServices[svcName]
		if !forced {
			imageRef := strings.TrimSpace(composeSvc.Image)
			if imageRef == "" {
				imageRef = project.Name + "-" + svcName
			}
			exists, err := ds.ImageExists(ctx, imageRef)
			if err != nil {
				return nil, err
			}
			if exists {
				continue
			}
		}
		if noBuild {
			return nil, fmt.Errorf("service %s requires an image build, but --no-build is enabled", svcName)
		}
		out = append(out, svcName)
	}
	sort.Strings(out)
	return out, nil
}

type managedImagePreparation struct {
	pullAlways          []string
	pullMissing         []string
	pullOrBuild         []managedPullOrBuild
	forceBuild          map[string]bool
	effectivePullPolicy map[string]string
}

type managedPullOrBuild struct {
	service string
	policy  string
}

type unmanagedImagePreparation struct {
	pullAlways  []string
	pullOrBuild []managedPullOrBuild
	forceBuild  map[string]bool
	prepared    []string
}

// planSelectedUnmanagedRefresh preserves native Compose --build/--pull
// behavior when Vaka must consume a project-wide option to protect managed
// services. It prepares only selected unmanaged services, including run
// dependencies. A run graph containing no managed service keeps native Compose
// handling and reaches this function with refresh options disabled.
func planSelectedUnmanagedRefresh(
	project *composetypes.Project,
	policySvcs map[string]*policy.ServiceConfig,
	inv *ComposeInvocation,
	pullValue string,
	pullRequested, forceBuild, noBuild bool,
) (unmanagedImagePreparation, error) {
	plan := unmanagedImagePreparation{forceBuild: map[string]bool{}}
	consumePull := pullRequested && (pullValue == composetypes.PullPolicyAlways || pullValue == composetypes.PullPolicyBuild)
	if !forceBuild && !consumePull {
		return plan, nil
	}
	var selected *composetypes.Project
	var err error
	switch inv.Subcommand {
	case "up", "create":
		targets, scanErr := scanCreateServiceTargets(inv)
		if scanErr != nil {
			return unmanagedImagePreparation{}, scanErr
		}
		selected, err = selectComposeServices(project, targets, inv.PostSubcommand)
	case "run":
		parsed, parseErr := parseRun(inv.PostSubcommand)
		if parseErr != nil {
			return unmanagedImagePreparation{}, parseErr
		}
		selected, err = selectRunServices(project, parsed)
	default:
		return plan, nil
	}
	if err != nil {
		return unmanagedImagePreparation{}, fmt.Errorf("select Compose services for image preparation: %w", err)
	}
	prepared := map[string]bool{}
	for name, svc := range selected.Services {
		if _, managed := policySvcs[name]; managed {
			continue
		}
		if forceBuild && svc.Build != nil {
			plan.forceBuild[name] = true
			prepared[name] = true
			// Compose applies --build after --pull and therefore gives a
			// buildable service build precedence. Do not also pull the service
			// image; image-only services still follow the requested pull policy.
			continue
		}
		switch pullValue {
		case composetypes.PullPolicyAlways:
			if strings.TrimSpace(svc.Image) == "" {
				continue
			}
			prepared[name] = true
			if svc.Build != nil {
				plan.pullOrBuild = append(plan.pullOrBuild, managedPullOrBuild{service: name, policy: "always"})
			} else {
				plan.pullAlways = append(plan.pullAlways, name)
			}
		case composetypes.PullPolicyBuild:
			prepared[name] = true
			if svc.Build != nil && !noBuild {
				plan.forceBuild[name] = true
			}
		}
	}
	if noBuild && len(plan.forceBuild) > 0 {
		return unmanagedImagePreparation{}, fmt.Errorf("--no-build conflicts with a requested build for selected unmanaged services")
	}
	for name := range prepared {
		plan.prepared = append(plan.prepared, name)
	}
	sort.Strings(plan.pullAlways)
	sort.Slice(plan.pullOrBuild, func(i, j int) bool { return plan.pullOrBuild[i].service < plan.pullOrBuild[j].service })
	sort.Strings(plan.prepared)
	return plan, nil
}

func selectComposeServices(project *composetypes.Project, targets, args []string) (*composetypes.Project, error) {
	enabled, err := project.WithServicesEnabled(targets...)
	if err != nil {
		return nil, err
	}
	noDeps, err := composeBoolOptionEnabled(args, "--no-deps", "")
	if err != nil {
		return nil, err
	}
	if noDeps {
		return enabled.WithSelectedServices(targets, composetypes.IgnoreDependencies)
	}
	return enabled.WithSelectedServices(targets)
}

var upOptionsWithValue = map[string]bool{
	"--attach": true, "--exit-code-from": true, "--no-attach": true,
	"--pull": true, "--scale": true, "--timeout": true, "-t": true,
	"--wait-timeout": true,
}

var upBooleanOptions = map[string]bool{
	"--abort-on-container-exit": true, "--abort-on-container-failure": true,
	"--always-recreate-deps": true, "--attach-dependencies": true,
	"--build": true, "-d": true, "--detach": true, "--dry-run": true,
	"--force-recreate": true, "--menu": true, "--no-build": true,
	"--no-color": true, "--no-deps": true, "--no-log-prefix": true,
	"--no-recreate": true, "--no-start": true, "--quiet-build": true,
	"--quiet-pull": true, "--remove-orphans": true, "-V": true,
	"--renew-anon-volumes": true, "--timestamps": true, "--wait": true,
	"-w": true, "--watch": true, "-y": true, "--yes": true,
}

var createOptionsWithValue = map[string]bool{
	"--pull": true, "--scale": true,
}

var createBooleanOptions = map[string]bool{
	"--build": true, "--dry-run": true, "--force-recreate": true,
	"--no-build": true, "--no-recreate": true, "--quiet-pull": true,
	"--remove-orphans": true, "-y": true, "--yes": true,
}

// scanCreateServiceTargets is deliberately command-local. It identifies only
// up/create SERVICE operands and fails before preparation when a new option is
// ambiguous; it is not a replacement Compose parser.
func scanCreateServiceTargets(inv *ComposeInvocation) ([]string, error) {
	valueOptions, booleanOptions := upOptionsWithValue, upBooleanOptions
	shortBooleans := "dVwy"
	if inv.Subcommand == "create" {
		valueOptions, booleanOptions = createOptionsWithValue, createBooleanOptions
		shortBooleans = "y"
	}
	var targets []string
	for i := 0; i < len(inv.PostSubcommand); {
		tok := inv.PostSubcommand[i]
		if tok == "--" {
			targets = append(targets, inv.PostSubcommand[i+1:]...)
			break
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			targets = append(targets, tok)
			i++
			continue
		}
		if flag, value, consumed, _, ok := parseValueTakingToken(inv.PostSubcommand, i, valueOptions); ok {
			if value == "" {
				return nil, fmt.Errorf("compose %s: %s requires a value", inv.Subcommand, flag)
			}
			if flag == "-t" || flag == "--timeout" || flag == "--wait-timeout" {
				if _, err := strconv.Atoi(value); err != nil {
					return nil, fmt.Errorf("compose %s: %s requires an integer, got %q", inv.Subcommand, flag, value)
				}
			}
			i += consumed
			continue
		}
		if booleanOptions[tok] {
			i++
			continue
		}
		if strings.HasPrefix(tok, "--") {
			name, _, hasValue := strings.Cut(tok, "=")
			if booleanOptions[name] && hasValue {
				i++
				continue
			}
		}
		if len(tok) > 2 && tok[0] == '-' && tok[1] != '-' {
			if inv.Subcommand == "up" && strings.HasPrefix(tok, "-t") {
				if _, err := strconv.Atoi(tok[2:]); err != nil {
					return nil, fmt.Errorf("compose up: -t requires an integer, got %q", tok[2:])
				}
				i++
				continue
			}
			validCluster := true
			for _, flag := range tok[1:] {
				if !strings.ContainsRune(shortBooleans, flag) {
					validCluster = false
					break
				}
			}
			if validCluster {
				i++
				continue
			}
		}
		return nil, fmt.Errorf("compose %s: unknown option %q before image preparation; upgrade Vaka if this is a new Docker Compose option", inv.Subcommand, tok)
	}
	return targets, nil
}

// planManagedImagePreparation applies explicit Compose pull policies only to
// services governed by vaka.yaml. An absent pull_policy keeps Vaka's existing
// --vaka-pull behavior; the final exact-ID override prevents Compose from
// performing an uninspected refresh later.
func planManagedImagePreparation(
	ctx context.Context,
	ds DockerServices,
	policySvcs map[string]*policy.ServiceConfig,
	project *composetypes.Project,
	cliPull string,
	cliPullSet bool,
	forceBuildAll bool,
	noBuild bool,
) (managedImagePreparation, error) {
	plan := managedImagePreparation{
		forceBuild:          map[string]bool{},
		effectivePullPolicy: map[string]string{},
	}
	if cliPullSet && cliPull != composetypes.PullPolicyAlways && cliPull != composetypes.PullPolicyMissing && cliPull != composetypes.PullPolicyIfNotPresent && cliPull != composetypes.PullPolicyNever && cliPull != composetypes.PullPolicyBuild {
		return managedImagePreparation{}, fmt.Errorf("unsupported Compose --pull value %q for Vaka-managed services", cliPull)
	}
	if noBuild && forceBuildAll {
		return managedImagePreparation{}, fmt.Errorf("--no-build conflicts with a requested build for Vaka-managed services")
	}
	names := make([]string, 0, len(policySvcs))
	for name := range policySvcs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		svc, ok := project.Services[name]
		if !ok {
			continue
		}
		effective := strings.ToLower(strings.TrimSpace(svc.PullPolicy))
		if cliPullSet {
			effective = cliPull
		}
		// Compose applies --build after --pull, replacing the effective pull
		// policy with build for services that have a build definition.
		if forceBuildAll && svc.Build != nil {
			effective = composetypes.PullPolicyBuild
		}
		plan.effectivePullPolicy[name] = effective
		switch {
		case effective == "":
			// Preserve Vaka's explicit --vaka-pull policy for services without a
			// Compose pull_policy.
		case effective == composetypes.PullPolicyNever:
		case effective == composetypes.PullPolicyAlways:
			if strings.TrimSpace(svc.Image) != "" {
				if svc.Build != nil {
					plan.pullOrBuild = append(plan.pullOrBuild, managedPullOrBuild{service: name, policy: "always"})
				} else {
					plan.pullAlways = append(plan.pullAlways, name)
				}
			}
		case effective == composetypes.PullPolicyMissing || effective == composetypes.PullPolicyIfNotPresent:
			if strings.TrimSpace(svc.Image) == "" {
				continue
			}
			if !imageUsesLatestTag(svc.Image) {
				exists, err := ds.ImageExists(ctx, svc.Image)
				if err != nil {
					return managedImagePreparation{}, err
				}
				if exists {
					continue
				}
			}
			if svc.Build != nil {
				plan.pullOrBuild = append(plan.pullOrBuild, managedPullOrBuild{service: name, policy: "missing"})
			} else {
				plan.pullMissing = append(plan.pullMissing, name)
			}
		case effective == composetypes.PullPolicyBuild:
			if svc.Build != nil {
				if noBuild {
					imageRef := strings.TrimSpace(svc.Image)
					if imageRef == "" {
						imageRef = project.Name + "-" + name
					}
					exists, err := ds.ImageExists(ctx, imageRef)
					if err != nil {
						return managedImagePreparation{}, err
					}
					if !exists {
						return managedImagePreparation{}, fmt.Errorf("service %s requires an image build, but --no-build is enabled", name)
					}
				} else {
					plan.forceBuild[name] = true
				}
			}
		case effective == "daily" || effective == "weekly" || strings.HasPrefix(effective, "every_"):
			return managedImagePreparation{}, fmt.Errorf("service %s uses pull_policy %q, which Vaka cannot apply before exact-image inspection; pull the image explicitly and use pull_policy: never, missing, always, or build", name, svc.PullPolicy)
		default:
			return managedImagePreparation{}, fmt.Errorf("service %s uses unsupported pull_policy %q", name, svc.PullPolicy)
		}
	}

	return plan, nil
}

func imageUsesLatestTag(image string) bool {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return false
	}
	if _, immutable := named.(reference.Digested); immutable {
		return false
	}
	tagged, ok := named.(reference.Tagged)
	return !ok || tagged.Tag() == "latest"
}

func composePullAlwaysRequested(inv *ComposeInvocation) bool {
	pull, present, _ := composePullOption(inv)
	return present && pull == "always"
}

func composePullOption(inv *ComposeInvocation) (string, bool, error) {
	if inv.Subcommand == "run" {
		parsed, err := parseRun(inv.PostSubcommand)
		if err != nil {
			return "", false, err
		}
		if err := validateComposePullValue(inv.Subcommand, parsed.pull, parsed.pullSet); err != nil {
			return "", false, err
		}
		return parsed.pull, parsed.pullSet, nil
	}
	pull := ""
	present := false
	for i := 0; i < len(inv.PostSubcommand); i++ {
		tok := inv.PostSubcommand[i]
		if tok == "--" {
			break
		}
		if value, ok := strings.CutPrefix(tok, "--pull="); ok {
			pull = value
			present = true
			continue
		}
		if tok == "--pull" {
			if i+1 >= len(inv.PostSubcommand) {
				return "", false, fmt.Errorf("compose %s: --pull requires a value", inv.Subcommand)
			}
			pull = inv.PostSubcommand[i+1]
			present = true
			i++
		}
	}
	if err := validateComposePullValue(inv.Subcommand, pull, present); err != nil {
		return "", false, err
	}
	return pull, present, nil
}

func validateComposePullValue(subcommand, value string, present bool) error {
	if !present {
		return nil
	}
	valid := value == composetypes.PullPolicyAlways || value == composetypes.PullPolicyMissing || value == composetypes.PullPolicyIfNotPresent || value == composetypes.PullPolicyNever
	if subcommand == "up" || subcommand == "create" || subcommand == "run" {
		valid = valid || value == composetypes.PullPolicyBuild
	}
	if !valid {
		return fmt.Errorf("compose %s: invalid --pull value %q", subcommand, value)
	}
	return nil
}

func composeBuildRequested(inv *ComposeInvocation) (bool, error) {
	if inv.Subcommand == "run" {
		parsed, err := parseRun(inv.PostSubcommand)
		if err != nil {
			return false, err
		}
		return parsed.build, nil
	}
	return composeBoolOptionEnabled(inv.PostSubcommand, "--build", "")
}

func consumeComposeImageRefreshOptions(inv *ComposeInvocation, consumeBuild, consumePull bool) error {
	args := append([]string{}, inv.Args[:inv.SubcommandIdx+1]...)
	post := inv.PostSubcommand
	limit := len(post)
	if inv.Subcommand == "run" {
		parsed, err := parseRun(post)
		if err != nil {
			return err
		}
		limit = parsed.serviceIndex
	}
	for i := 0; i < len(post); i++ {
		if i >= limit {
			args = append(args, post[i:]...)
			break
		}
		tok := post[i]
		if tok == "--" {
			args = append(args, post[i:]...)
			break
		}
		if consumeBuild && (tok == "--build" || strings.HasPrefix(tok, "--build=")) {
			continue
		}
		if consumePull && strings.HasPrefix(tok, "--pull=") {
			continue
		}
		if consumePull && tok == "--pull" && i+1 < len(post) {
			i++
			continue
		}
		args = append(args, tok)
	}
	resolvedProjectName := inv.ResolvedProjectName
	reparsed, err := ParseComposeInvocation(args)
	if err != nil {
		return fmt.Errorf("rewrite consumed Compose image options: %w", err)
	}
	reparsed.ResolvedProjectName = resolvedProjectName
	*inv = *reparsed
	return nil
}

type capabilityPlan struct {
	// Add is the minimum set Vaka must add to the container configuration for
	// initialization. Drop contains the policy-requested capabilities plus every
	// capability in Add, so none of Vaka's temporary privileges reach the
	// workload.
	Add  []string
	Drop []string
}

// computeCapabilityPlan derives Vaka's temporary capabilities from the merged
// Compose service. Docker applies cap_drop first and cap_add second, including
// the ALL token, so an explicit add wins. Capabilities already granted by the
// service remain intentional unless runtime.dropCaps requests their removal.
func computeCapabilityPlan(svc composetypes.ServiceConfig, runtime *policy.RuntimeConfig, _ string) capabilityPlan {
	drop := []string{}
	if runtime != nil {
		drop = append(drop, runtime.DropCaps...)
	}
	drop = uniqueCapabilityNames(drop)

	// SETUID and SETGID are also temporary exec-trampoline requirements. The
	// container's startup identity cannot predict a later `vaka exec --user`,
	// so ensure both are present in the stored container configuration. Any
	// capability Vaka restores is added to Drop below and never reaches the
	// workload.
	required := []string{"NET_ADMIN", "SETUID", "SETGID"}
	if runtime != nil && len(runtime.Chown) > 0 {
		required = append(required, "CHOWN", "DAC_OVERRIDE")
	}

	add := []string{}
	for _, cap := range required {
		if containerHasCapability(svc, cap) {
			continue
		}
		add = append(add, cap)
		drop = appendUniqueCapability(drop, cap)
	}

	// Bounding-set removal requires CAP_SETPCAP in the effective set. Docker's
	// cap_drop may remove it (including via ALL), so add it temporarily whenever
	// any post-initialization capability removal is required.
	if len(drop) > 0 && !containerHasCapabilityWithAdds(svc, add, "SETPCAP") {
		add = append(add, "SETPCAP")
		drop = appendUniqueCapability(drop, "SETPCAP")
	}

	return capabilityPlan{Add: add, Drop: drop}
}

func normalizeCapabilityName(name string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "CAP_")
}

func uniqueCapabilityNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		normalized := normalizeCapabilityName(name)
		if normalized != "" {
			out = appendUniqueCapability(out, normalized)
		}
	}
	return out
}

func appendUniqueCapability(names []string, name string) []string {
	normalized := normalizeCapabilityName(name)
	for _, existing := range names {
		if normalizeCapabilityName(existing) == normalized {
			return names
		}
	}
	return append(names, normalized)
}

func containerHasCapability(svc composetypes.ServiceConfig, name string) bool {
	want := normalizeCapabilityName(name)
	for _, added := range svc.CapAdd {
		cap := normalizeCapabilityName(added)
		if cap == "ALL" || cap == want {
			return true
		}
	}
	for _, dropped := range svc.CapDrop {
		cap := normalizeCapabilityName(dropped)
		if cap == "ALL" || cap == want {
			return false
		}
	}
	return defaultDockerCaps["CAP_"+want]
}

func containerHasCapabilityWithAdds(svc composetypes.ServiceConfig, adds []string, name string) bool {
	want := normalizeCapabilityName(name)
	for _, added := range adds {
		if normalizeCapabilityName(added) == want {
			return true
		}
	}
	return containerHasCapability(svc, want)
}
