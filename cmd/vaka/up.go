// cmd/vaka/up.go
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
	"vaka.dev/vaka/pkg/policy"
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

// runInjection is the injection path for "up" and "run":
// 1. Collect all -f files from args (or discover defaults if none).
// 2. Validate vaka.yaml against the merged compose project.
// 3. Load the fully-merged compose project via compose-go (authoritative for
//    entrypoint/cap data — handles multi-file merge, env interpolation, etc.).
// 4. Per service: resolve entrypoint, compute cap delta, serialise policy.
// 5. Build override YAML, inject -f - into argv, exec docker.
func runInjection(vakaFile string, args []string) error {
	composeFiles := allFileFlags(args)
	var defaults []string
	if len(composeFiles) == 0 {
		defaults = discoverComposeFiles(".")
		if len(defaults) == 0 {
			return fmt.Errorf("no compose configuration file found in current directory")
		}
		composeFiles = defaults
	}

	p, project, err := loadAndValidate(vakaFile, composeFiles)
	if err != nil {
		return err
	}

	ctx := context.Background()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer dockerClient.Close()

	var entries []compose.ServiceEntry
	envVars := os.Environ()

	for svcName, svc := range p.Services {
		composeSvc, ok := project.Services[svcName]
		if !ok {
			return fmt.Errorf("service %q: not found in compose files %v", svcName, composeFiles)
		}

		entrypoint, cmd, err := resolveEntrypoint(ctx, dockerClient, svcName, composeSvc)
		if err != nil {
			return err
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
			return err
		}
		raw, err := yaml.Marshal(sliced)
		if err != nil {
			return fmt.Errorf("marshal policy for %s: %w", svcName, err)
		}

		envKey := "VAKA_" + strings.ToUpper(strings.ReplaceAll(svcName, "-", "_")) + "_CONF"
		envVars = append(envVars, envKey+"="+base64.StdEncoding.EncodeToString(raw))

		entries = append(entries, compose.ServiceEntry{
			Name:       svcName,
			Entrypoint: entrypoint,
			Command:    cmd,
			CapDelta:   delta,
			EnvVarName: envKey,
		})
	}

	overrideYAML, err := compose.BuildOverride(entries)
	if err != nil {
		return fmt.Errorf("build override: %w", err)
	}

	// args already contains global flags + subcommand at correct positions.
	// Prepend "compose"; injectStdinOverride inserts -f - after the last -f.
	dockerArgs := injectStdinOverride(append([]string{"compose"}, args...), defaults)

	c := exec.Command("docker", dockerArgs...)
	c.Stdin = strings.NewReader(overrideYAML)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = envVars
	return c.Run()
}

// resolveEntrypoint returns the effective entrypoint and command for a service,
// using the already-merged compose-go ServiceConfig (all -f files merged).
// Falls back to Docker SDK image inspection only when neither entrypoint nor
// command is declared in any of the compose files.
func resolveEntrypoint(ctx context.Context, dockerClient *client.Client, svcName string, svc composetypes.ServiceConfig) ([]string, []string, error) {
	if len(svc.Entrypoint) > 0 || len(svc.Command) > 0 {
		return svc.Entrypoint, svc.Command, nil
	}
	if svc.Image == "" {
		return nil, nil, fmt.Errorf("service %s: no image and no entrypoint/command declared", svcName)
	}
	inspect, err := dockerClient.ImageInspect(ctx, svc.Image)
	if err != nil {
		if client.IsErrNotFound(err) {
			return nil, nil, fmt.Errorf(
				"service %s: image %q not available locally and no entrypoint/command declared — pull first or add entrypoint:",
				svcName, svc.Image)
		}
		return nil, nil, fmt.Errorf("service %s: inspect %q: %w", svcName, svc.Image, err)
	}
	if inspect.Config == nil {
		return nil, nil, fmt.Errorf("service %s: image %q has no Config", svcName, svc.Image)
	}
	return inspect.Config.Entrypoint, inspect.Config.Cmd, nil
}

// computeCapDelta returns the capabilities vaka needs that are absent from
// Docker's default set and not already in the merged compose service's cap_add.
func computeCapDelta(svc composetypes.ServiceConfig) []string {
	existing := map[string]bool{}
	for _, cap := range svc.CapAdd {
		existing[strings.ToUpper(cap)] = true
	}
	var delta []string
	for _, cap := range []string{"NET_ADMIN"} {
		if !existing[cap] && !defaultDockerCaps["CAP_"+cap] {
			delta = append(delta, cap)
		}
	}
	return delta
}
