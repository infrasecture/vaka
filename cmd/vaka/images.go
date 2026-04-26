package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	dockercli "github.com/docker/cli/cli/command"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	dockerflags "github.com/docker/cli/cli/flags"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/spf13/pflag"
)

const dockerConfigPathHint = "~/.docker/config.json"

// DockerServices is the interface for all Docker daemon interactions in vaka.
// A single implementation is created per runFull invocation; a test double can
// replace it entirely.
type DockerServices interface {
	// EnsureImage inspects ref locally and pulls it if absent.
	EnsureImage(ctx context.Context, ref string) error
	// ImageExists returns true if ref is available locally. Transport errors
	// other than NotFound are propagated.
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ResolveRuntime resolves runtime metadata needed by vaka:
	// effective entrypoint/command vectors and image-level USER fallback.
	ResolveRuntime(ctx context.Context, svcName string, svc composetypes.ServiceConfig) (ResolvedRuntime, error)
}

// ResolvedRuntime is resolved service runtime metadata from compose + image.
type ResolvedRuntime struct {
	Entrypoint []string
	Command    []string
	// ImageUser is the image config USER value. Compose `service.user` is
	// intentionally not folded into this field so callers can apply explicit
	// precedence rules (compose user first, image fallback second).
	ImageUser string
}

// dockerClient is a narrow interface over the Docker API operations used by
// dockerServices. *client.Client satisfies it; tests inject a stub.
type dockerClient interface {
	ImageInspect(ctx context.Context, ref string, opts ...client.ImageInspectOption) (dockerimage.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, opts dockerimage.PullOptions) (io.ReadCloser, error)
}

// dockerServices is the production DockerServices backed by the Docker API.
// The API client is initialized through docker/cli flag/env/config resolution
// so it targets the same backend Docker CLI would use for this invocation.
type dockerServices struct {
	c          dockerClient
	targetDesc string
}

var loadDockerConfigFile = dockerconfig.LoadDefaultConfigFile

// NewDockerServices creates a DockerServices for one vaka invocation using
// docker/cli target resolution semantics:
//
//  1. explicit --context/-c from compose global flags
//  2. DOCKER_HOST fallback to default context endpoint
//  3. DOCKER_CONTEXT
//  4. currentContext from ~/.docker/config.json
//  5. default context
func NewDockerServices(args []string) (DockerServices, error) {
	cfg := loadDockerConfigFile(io.Discard)
	opts := newDockerClientOptions(args)
	targetDesc := dockerTargetDescription(args, cfg)

	apiClient, err := dockercli.NewAPIClientFromFlags(opts, cfg)
	if err != nil {
		return nil, fmt.Errorf("create Docker client for %s: %w", targetDesc, err)
	}
	return &dockerServices{
		c:          apiClient,
		targetDesc: targetDesc,
	}, nil
}

// dockerContextFromArgs returns the docker context selected via compose global
// flags. The last occurrence wins. Returns empty when unset.
func dockerContextFromArgs(args []string) string {
	return composeGlobalValue(args, "--context", "-c")
}

func newDockerClientOptions(args []string) *dockerflags.ClientOptions {
	opts := dockerflags.NewClientOptions()
	fs := pflag.NewFlagSet("vaka-docker", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts.InstallFlags(fs)
	if ctxName := dockerContextFromArgs(args); ctxName != "" {
		opts.Context = ctxName
	}
	opts.SetDefaultOptions(fs)
	return opts
}

func dockerTargetDescription(args []string, cfg *configfile.ConfigFile) string {
	if ctxName := dockerContextFromArgs(args); ctxName != "" {
		return fmt.Sprintf("context %q (from --context)", ctxName)
	}
	if host := strings.TrimSpace(os.Getenv(client.EnvOverrideHost)); host != "" {
		return fmt.Sprintf("daemon %q (from %s)", host, client.EnvOverrideHost)
	}
	if ctxName := strings.TrimSpace(os.Getenv(dockercli.EnvOverrideContext)); ctxName != "" {
		return fmt.Sprintf("context %q (from %s)", ctxName, dockercli.EnvOverrideContext)
	}
	if cfg != nil && strings.TrimSpace(cfg.CurrentContext) != "" {
		return fmt.Sprintf("context %q (from %s)", strings.TrimSpace(cfg.CurrentContext), dockerConfigPathHint)
	}
	return "default Docker context"
}

// ImageExists returns true if ref is present in the local image store.
func (d *dockerServices) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := d.c.ImageInspect(ctx, ref)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("inspect %s on %s: %w", ref, d.targetDesc, err)
}

// EnsureImage inspects ref locally; pulls it if absent.
func (d *dockerServices) EnsureImage(ctx context.Context, ref string) error {
	_, err := d.c.ImageInspect(ctx, ref)
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect %s on %s: %w", ref, d.targetDesc, err)
	}
	rc, pullErr := d.c.ImagePull(ctx, ref, dockerimage.PullOptions{})
	if pullErr != nil {
		return fmt.Errorf("failed to pull %s on %s — check network connectivity or use --vaka-init-present if binaries are baked into the image: %w", ref, d.targetDesc, pullErr)
	}
	defer rc.Close()
	if _, err := io.Copy(os.Stderr, rc); err != nil {
		return fmt.Errorf("read pull stream for %s on %s: %w", ref, d.targetDesc, err)
	}
	return nil
}

// ResolveRuntime resolves effective runtime metadata for svc, following
// Docker/Compose semantics:
//
//   - compose entrypoint set: resolved pair is (compose.Entrypoint, compose.Command).
//     Docker resets CMD to empty when ENTRYPOINT is overridden, so a compose
//     entrypoint without command legitimately yields an empty command.
//   - compose entrypoint empty, command set: the image's ENTRYPOINT is preserved
//     (common pattern: app image defines ENTRYPOINT, compose overrides args).
//   - both empty: both come from the image's Dockerfile defaults.
//
// For user restoration, image Config.User is also resolved when compose
// service.user is unset, so image inspection is performed when either
// entrypoint or user fallback requires it.
func (d *dockerServices) ResolveRuntime(ctx context.Context, svcName string, svc composetypes.ServiceConfig) (ResolvedRuntime, error) {
	resolved := ResolvedRuntime{
		Entrypoint: svc.Entrypoint,
		Command:    svc.Command,
	}

	needImageEntrypoint := len(svc.Entrypoint) == 0
	needImageUser := strings.TrimSpace(svc.User) == ""
	needInspect := needImageEntrypoint || needImageUser
	if !needInspect {
		return resolved, nil
	}

	if svc.Image == "" {
		return ResolvedRuntime{}, fmt.Errorf(
			"service %s: cannot resolve image defaults without image: (needed for %s)",
			svcName, missingRuntimeFieldsHint(needImageEntrypoint, needImageUser),
		)
	}
	inspect, err := d.c.ImageInspect(ctx, svc.Image)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ResolvedRuntime{}, fmt.Errorf(
				"service %s: image %q not available locally on %s — pull/build it first, or set compose user/entrypoint so image defaults are not needed",
				svcName, svc.Image, d.targetDesc,
			)
		}
		return ResolvedRuntime{}, fmt.Errorf("service %s: inspect %q on %s: %w", svcName, svc.Image, d.targetDesc, err)
	}
	if inspect.Config == nil {
		return ResolvedRuntime{}, fmt.Errorf("service %s: image %q has no Config", svcName, svc.Image)
	}

	if needImageEntrypoint {
		resolved.Entrypoint = inspect.Config.Entrypoint
		if len(resolved.Command) == 0 {
			resolved.Command = inspect.Config.Cmd
		}
	}
	if needImageUser {
		resolved.ImageUser = inspect.Config.User
	}
	return resolved, nil
}

func missingRuntimeFieldsHint(needImageEntrypoint, needImageUser bool) string {
	switch {
	case needImageEntrypoint && needImageUser:
		return "entrypoint/cmd and user fallback"
	case needImageEntrypoint:
		return "entrypoint/cmd fallback"
	case needImageUser:
		return "user fallback"
	default:
		return "runtime fallback"
	}
}
