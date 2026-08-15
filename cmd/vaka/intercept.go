// cmd/vaka/intercept.go
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
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
	if inv.Subcommand == "up" && hasComposeOption(inv.PostSubcommand, "--no-recreate") && len(p.Services) > 0 {
		return "", nil, fmt.Errorf("compose up --no-recreate can reuse containers with an older unsafe runtime; remove --no-recreate so managed services are recreated")
	}
	if inv.Subcommand == "watch" && hasComposeOption(inv.PostSubcommand, "--no-up") && len(p.Services) > 0 {
		return "", nil, fmt.Errorf("compose watch --no-up can reuse containers with an older unsafe runtime; remove --no-up so managed services are recreated")
	}
	inv.ResolvedProjectName = project.Name

	// Pre-build any service whose image must be inspected for ENTRYPOINT/CMD
	// and/or USER fallback but isn't available locally and has a build: section.
	// Without this,
	// `vaka up --build` fails for services that rely on Dockerfile defaults.
	// When the user passes --build, every service with a build: section is
	// prebuilt so ResolveRuntime inspects the fresh image, not a stale copy.
	forceRebuild := inv.BuildRequested
	toBuild, err := servicesNeedingPrebuild(ctx, ds, p.Services, project, forceRebuild)
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
		if composeSvc.Labels[vakaInitLabel] == "present" {
			return "", nil, fmt.Errorf("service %s: label %s=present is not supported: the exec security boundary requires Vaka's verified read-only runtime mount", svcName, vakaInitLabel)
		}

		rt, err := ds.ResolveRuntime(ctx, svcName, composeSvc)
		if err != nil {
			return "", nil, err
		}

		delta := computeCapDelta(composeSvc)
		if svc.Runtime == nil {
			svc.Runtime = &policy.RuntimeConfig{}
		}
		if len(svc.Runtime.DropCaps) == 0 {
			svc.Runtime.DropCaps = delta
		}
		fmt.Fprintf(os.Stderr, "vaka: service %s: dropCaps: %v\n", svcName, svc.Runtime.DropCaps)

		sliced, err := policy.SliceService(p, svcName)
		if err != nil {
			return "", nil, err
		}
		sliced.GeneratedBy = "vaka/" + version
		sliced.RequiredRuntimeVersion = runtimeBundleVersion
		restoreUser := strings.TrimSpace(composeSvc.User)
		if restoreUser == "" {
			restoreUser = strings.TrimSpace(rt.ImageUser)
		}
		sliced.Services[svcName].User = restoreUser
		policyRevision, err := policy.Revision(sliced)
		if err != nil {
			return "", nil, fmt.Errorf("compute policy revision for %s: %w", svcName, err)
		}

		raw, err := yaml.Marshal(sliced)
		if err != nil {
			return "", nil, fmt.Errorf("marshal policy for %s: %w", svcName, err)
		}

		envKey := "VAKA_" + strings.ToUpper(strings.ReplaceAll(svcName, "-", "_")) + "_CONF"
		extraEnv = append(extraEnv, envKey+"="+base64.StdEncoding.EncodeToString(raw))

		entries = append(entries, compose.ServiceEntry{
			Name:             svcName,
			Entrypoint:       rt.Entrypoint,
			Command:          rt.Command,
			CapDelta:         delta,
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

	overrideYAML, err = compose.BuildOverride(entries, runtimeMount)
	if err != nil {
		return "", nil, fmt.Errorf("build override: %w", err)
	}
	return overrideYAML, extraEnv, nil
}

func hasComposeOption(args []string, option string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
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
// incorrect command vectors. Prebuilding every eligible service ensures
// ResolveRuntime sees the post-build image.
func servicesNeedingPrebuild(ctx context.Context, ds DockerServices, policySvcs map[string]*policy.ServiceConfig, project *composetypes.Project, forceRebuild bool) ([]string, error) {
	var out []string
	for svcName := range policySvcs {
		composeSvc, ok := project.Services[svcName]
		if !ok {
			continue
		}
		if !needsImageRuntimeFallback(composeSvc) {
			continue
		}
		if composeSvc.Build == nil {
			continue
		}
		if composeSvc.Image != "" && !forceRebuild {
			exists, err := ds.ImageExists(ctx, composeSvc.Image)
			if err != nil {
				return nil, err
			}
			if exists {
				continue
			}
		}
		out = append(out, svcName)
	}
	sort.Strings(out)
	return out, nil
}

func needsImageRuntimeFallback(svc composetypes.ServiceConfig) bool {
	needsHealthcheck := svc.Image != "" && (svc.HealthCheck == nil || (!svc.HealthCheck.Disable && len(svc.HealthCheck.Test) == 0))
	return len(svc.Entrypoint) == 0 || strings.TrimSpace(svc.User) == "" || needsHealthcheck
}

// computeCapDelta returns capabilities vaka needs that are absent from Docker's
// default set and not already in the merged compose service's cap_add. Both
// short-form (NET_ADMIN) and prefixed-form (CAP_NET_ADMIN) user entries are
// recognised, along with the ALL catch-all.
func computeCapDelta(svc composetypes.ServiceConfig) []string {
	existing := map[string]bool{}
	for _, cap := range svc.CapAdd {
		u := strings.ToUpper(cap)
		existing[strings.TrimPrefix(u, "CAP_")] = true
	}
	if existing["ALL"] {
		return nil
	}
	var delta []string
	for _, cap := range []string{"NET_ADMIN"} {
		if !existing[cap] && !defaultDockerCaps["CAP_"+cap] {
			delta = append(delta, cap)
		}
	}
	return delta
}
