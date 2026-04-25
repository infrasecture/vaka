// cmd/vaka/intercept.go
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
	"vaka.dev/vaka/pkg/policy"
)

// vakaInitLabel is the compose service label that signals the image already
// ships the vaka-init binaries at /opt/vaka/sbin/. When present, the service
// does not depend on the __vaka-init volume helper container.
const vakaInitLabel = "agent.vaka.init"

// vakaInitBaseImage is the image repository for the __vaka-init helper
// container. The full reference is built by appending ":" + version.
const vakaInitBaseImage = "emsi/vaka-init"

// Test hooks: overridden in unit tests to avoid real Docker side effects.
var (
	newDockerServices   = NewDockerServices
	execDockerComposeFn = execDockerCompose
)

// defaultDockerCaps is the set of capabilities present in a default Docker
// container (no cap_drop, no cap_add). NET_ADMIN is notably absent.
var defaultDockerCaps = map[string]bool{
	"CAP_CHOWN": true, "CAP_DAC_OVERRIDE": true, "CAP_FOWNER": true,
	"CAP_FSETID": true, "CAP_KILL": true, "CAP_SETGID": true,
	"CAP_SETUID": true, "CAP_SETPCAP": true, "CAP_NET_BIND_SERVICE": true,
	"CAP_NET_RAW": true, "CAP_SYS_CHROOT": true, "CAP_MKNOD": true,
	"CAP_AUDIT_WRITE": true, "CAP_SETFCAP": true,
}

// subcmdPath classifies how a compose subcommand is handled.
type subcmdPath int

const (
	pathCobra       subcmdPath = iota // validate, show-nft, version — cobra commands
	pathFull                          // up, run, create, volumes — full policy + __vaka-init injection
	pathLifecycle                     // down, stop, kill, rm — __vaka-init container only
	pathShowCompose                   // show-compose — print generated override only
	pathPassthrough                   // all others — forwarded unchanged to docker compose
)

// classifySubcmd maps a compose subcommand name to its dispatch path.
func classifySubcmd(subcmd string) subcmdPath {
	switch subcmd {
	case "validate", "show-nft", "doctor", "version", "":
		return pathCobra
	case "up", "run", "create", "volumes":
		return pathFull
	case "down", "stop", "kill", "rm":
		return pathLifecycle
	case "show-compose":
		return pathShowCompose
	default:
		return pathPassthrough
	}
}

// execDockerCompose executes docker compose with the given args.
// When overrideYAML is non-empty it is injected via -f - (with default compose
// files also passed via -f so compose merges them correctly).
// extraEnv, when non-nil, is appended to the inherited environment.
func execDockerCompose(args []string, overrideYAML string, extraEnv []string) error {
	var dockerArgs []string
	if overrideYAML != "" {
		defaults := []string{}
		if len(allFileFlags(args)) == 0 {
			defaults = discoverComposeFiles(".")
		}
		dockerArgs = injectStdinOverride(append([]string{"compose"}, args...), defaults)
	} else {
		dockerArgs = append([]string{"compose"}, args...)
	}
	c := exec.Command("docker", dockerArgs...)
	if overrideYAML != "" {
		c.Stdin = strings.NewReader(overrideYAML)
	} else {
		c.Stdin = os.Stdin
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if extraEnv != nil {
		c.Env = append(os.Environ(), extraEnv...)
	}
	return c.Run()
}

// runFull handles full-override commands: up, run, create, volumes.
// It loads and validates vaka.yaml, ensures the __vaka-init image when needed,
// builds the full compose override, and delegates to execDockerCompose.
func runFull(vakaFile string, args []string, vakaInitPresent bool) error {
	ctx := context.Background()
	ds, err := newDockerServices()
	if err != nil {
		return err
	}
	overrideYAML, extraEnv, err := buildInjectionOverride(ctx, ds, vakaFile, args, vakaInitPresent)
	if err != nil {
		return err
	}
	return execDockerComposeFn(args, overrideYAML, extraEnv)
}

// buildInjectionOverride builds the compose override and per-service secret env
// payload from the same shared path used by full injection commands.
//
// Side effects are intentional and shared with runFull: pre-build and
// emsi/vaka-init image ensure happen here so show-compose cannot drift.
func buildInjectionOverride(
	ctx context.Context,
	ds DockerServices,
	vakaFile string,
	args []string,
	vakaInitPresent bool,
) (overrideYAML string, extraEnv []string, err error) {
	composeFiles := allFileFlags(args)
	if len(composeFiles) == 0 {
		composeFiles = discoverComposeFiles(".")
		if len(composeFiles) == 0 {
			return "", nil, fmt.Errorf("no compose configuration file found in current directory")
		}
	}

	p, project, err := loadAndValidate(vakaFile, composeFiles)
	if err != nil {
		return "", nil, err
	}

	// Pre-build any service whose image must be inspected for ENTRYPOINT/CMD
	// and/or USER fallback but isn't available locally and has a build: section.
	// Without this,
	// `vaka up --build` fails for services that rely on Dockerfile defaults.
	// When the user passes --build, every service with a build: section is
	// prebuilt so ResolveRuntime inspects the fresh image, not a stale copy.
	forceRebuild := hasBuildFlag(args)
	toBuild, err := servicesNeedingPrebuild(ctx, ds, p.Services, project, forceRebuild)
	if err != nil {
		return "", nil, err
	}
	if len(toBuild) > 0 {
		fmt.Fprintf(os.Stderr, "vaka: pre-building services to resolve entrypoints: %v\n", toBuild)
		buildArgs := append(globalFlags(args), "build")
		buildArgs = append(buildArgs, toBuild...)
		if err := execDockerComposeFn(buildArgs, "", nil); err != nil {
			return "", nil, fmt.Errorf("pre-build: %w", err)
		}
	}

	var entries []compose.ServiceEntry
	extraEnv = nil

	for svcName, svc := range p.Services {
		composeSvc, ok := project.Services[svcName]
		if !ok {
			return "", nil, fmt.Errorf("service %q: not found in compose files %v", svcName, composeFiles)
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
		sliced.VakaVersion = version
		restoreUser := strings.TrimSpace(composeSvc.User)
		if restoreUser == "" {
			restoreUser = strings.TrimSpace(rt.ImageUser)
		}
		sliced.Services[svcName].User = restoreUser

		raw, err := yaml.Marshal(sliced)
		if err != nil {
			return "", nil, fmt.Errorf("marshal policy for %s: %w", svcName, err)
		}

		envKey := "VAKA_" + strings.ToUpper(strings.ReplaceAll(svcName, "-", "_")) + "_CONF"
		extraEnv = append(extraEnv, envKey+"="+base64.StdEncoding.EncodeToString(raw))

		entries = append(entries, compose.ServiceEntry{
			Name:       svcName,
			Entrypoint: rt.Entrypoint,
			Command:    rt.Command,
			CapDelta:   delta,
			EnvVarName: envKey,
			OptOut:     composeSvc.Labels[vakaInitLabel] == "present",
		})
	}

	// Pull __vaka-init image only when injection is actually needed.
	needsInjection := false
	for _, e := range entries {
		if !e.OptOut {
			needsInjection = true
			break
		}
	}
	vakaInitImageRef := ""
	if !vakaInitPresent && needsInjection {
		vakaInitImageRef = vakaInitBaseImage + ":" + version
		if err := ds.EnsureImage(ctx, vakaInitImageRef); err != nil {
			return "", nil, err
		}
	}

	overrideYAML, err = compose.BuildOverride(entries, vakaInitImageRef)
	if err != nil {
		return "", nil, fmt.Errorf("build override: %w", err)
	}
	return overrideYAML, extraEnv, nil
}

// lifecycleOverrideYAML returns the minimal compose override YAML declaring the
// __vaka-init container so lifecycle commands (down/stop/kill/rm) include it in
// their operation. Returns "" when vakaInitPresent is true; execDockerCompose
// then runs as a pure passthrough without injecting -f -.
func lifecycleOverrideYAML(vakaInitPresent bool, imageRef string) (string, error) {
	if vakaInitPresent {
		return "", nil
	}
	return compose.BuildVakaInitOnlyOverride(imageRef)
}

// runLifecycle handles lifecycle commands: down, stop, kill, rm.
func runLifecycle(args []string, vakaInitPresent bool) error {
	overrideYAML, err := lifecycleOverrideYAML(vakaInitPresent, vakaInitBaseImage+":"+version)
	if err != nil {
		return fmt.Errorf("build vaka-init container override: %w", err)
	}
	return execDockerCompose(args, overrideYAML, nil)
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
	return len(svc.Entrypoint) == 0 || strings.TrimSpace(svc.User) == ""
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
