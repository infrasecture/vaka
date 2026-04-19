// cmd/vaka/images.go
package main

import (
	"context"
	"fmt"
	"io"
	"os"

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
	// ResolveEntrypoint returns the effective entrypoint and command for a
	// compose service. If the service declares either field they are returned
	// directly; otherwise the image is inspected to obtain defaults.
	ResolveEntrypoint(ctx context.Context, svcName string, svc composetypes.ServiceConfig) ([]string, []string, error)
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

// ResolveEntrypoint returns the effective entrypoint and command for svc.
// If the compose service declares either field they are returned directly;
// otherwise the image is inspected to obtain the Dockerfile defaults.
func (d *dockerServices) ResolveEntrypoint(ctx context.Context, svcName string, svc composetypes.ServiceConfig) ([]string, []string, error) {
	if len(svc.Entrypoint) > 0 || len(svc.Command) > 0 {
		return svc.Entrypoint, svc.Command, nil
	}
	if svc.Image == "" {
		return nil, nil, fmt.Errorf("service %s: no image and no entrypoint/command declared", svcName)
	}
	inspect, err := d.c.ImageInspect(ctx, svc.Image)
	if err != nil {
		if errdefs.IsNotFound(err) {
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
