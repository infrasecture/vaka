// cmd/vaka/images.go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

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

// dockerServices is the production DockerServices backed by the Docker daemon.
type dockerServices struct {
	c dockerClient
}

// NewDockerServices creates a DockerServices using the Docker environment
// (DOCKER_HOST, TLS settings, active context). The underlying client is
// created once and reused for all operations. Close is intentionally omitted:
// this is a short-lived CLI process that exits immediately after the operation,
// so the OS reclaims all resources without an explicit teardown.
func NewDockerServices() (DockerServices, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &dockerServices{c: c}, nil
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
	return false, fmt.Errorf("inspect %s: %w", ref, err)
}

// EnsureImage inspects ref locally; pulls it if absent.
func (d *dockerServices) EnsureImage(ctx context.Context, ref string) error {
	_, err := d.c.ImageInspect(ctx, ref)
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect %s: %w", ref, err)
	}
	rc, pullErr := d.c.ImagePull(ctx, ref, dockerimage.PullOptions{})
	if pullErr != nil {
		return fmt.Errorf("failed to pull %s — check network connectivity or use --vaka-init-present if binaries are baked into the image: %w", ref, pullErr)
	}
	defer rc.Close()
	_, err = io.Copy(os.Stderr, rc)
	return err
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
				"service %s: image %q not available locally — pull/build it first, or set compose user/entrypoint so image defaults are not needed",
				svcName, svc.Image)
		}
		return ResolvedRuntime{}, fmt.Errorf("service %s: inspect %q: %w", svcName, svc.Image, err)
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
