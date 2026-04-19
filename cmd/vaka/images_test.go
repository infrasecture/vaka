// cmd/vaka/images_test.go
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// fakeDockerClient implements dockerClient for unit tests without a live daemon.
type fakeDockerClient struct {
	notFound      bool                        // ImageInspect returns NotFound when true
	inspectResult dockerimage.InspectResponse // returned when notFound == false
	pullErr       error                       // error to return from ImagePull; nil = success
	pullCalled    bool
}

func (f *fakeDockerClient) ImageInspect(_ context.Context, _ string, _ ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	if f.notFound {
		return dockerimage.InspectResponse{}, errdefs.NotFound(errors.New("not found"))
	}
	return f.inspectResult, nil
}

func (f *fakeDockerClient) ImagePull(_ context.Context, _ string, _ dockerimage.PullOptions) (io.ReadCloser, error) {
	f.pullCalled = true
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return io.NopCloser(&bytes.Buffer{}), nil
}

// --- EnsureImage tests ---

func TestEnsureImagePresent(t *testing.T) {
	dc := &fakeDockerClient{notFound: false}
	e := &dockerServices{c: dc}
	if err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0"); err != nil {
		t.Fatalf("present: unexpected error: %v", err)
	}
	if dc.pullCalled {
		t.Error("present: pull must not be called when image is already present")
	}
}

func TestEnsureImageAbsentPullSucceeds(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	e := &dockerServices{c: dc}
	if err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0"); err != nil {
		t.Fatalf("absent+pull succeeds: unexpected error: %v", err)
	}
	if !dc.pullCalled {
		t.Error("absent+pull succeeds: pull must be called when image is absent")
	}
}

func TestEnsureImageAbsentPullFails(t *testing.T) {
	pullErr := errors.New("network unreachable")
	dc := &fakeDockerClient{notFound: true, pullErr: pullErr}
	e := &dockerServices{c: dc}
	err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0")
	if err == nil {
		t.Fatal("pull fails: expected error, got nil")
	}
	if !errors.Is(err, pullErr) {
		t.Errorf("pull fails: expected %v wrapped, got %v", pullErr, err)
	}
}

// --- ResolveEntrypoint tests ---

func TestResolveEntrypointFromCompose(t *testing.T) {
	// Entrypoint declared in compose — no image inspect needed.
	dc := &fakeDockerClient{}
	ds := &dockerServices{c: dc}
	svc := composetypes.ServiceConfig{
		Entrypoint: []string{"/app"},
		Command:    []string{"--flag"},
	}
	ep, cmd, err := ds.ResolveEntrypoint(context.Background(), "myapp", svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ep) != 1 || ep[0] != "/app" {
		t.Errorf("entrypoint = %v, want [/app]", ep)
	}
	if len(cmd) != 1 || cmd[0] != "--flag" {
		t.Errorf("command = %v, want [--flag]", cmd)
	}
}

func TestResolveEntrypointFromInspect(t *testing.T) {
	// No compose entrypoint — should inspect image and use Dockerfile defaults.
	dc := &fakeDockerClient{
		inspectResult: dockerimage.InspectResponse{
			Config: &dockerspec.DockerOCIImageConfig{
				ImageConfig: ocispec.ImageConfig{
					Entrypoint: []string{"/docker-entrypoint.sh"},
					Cmd:        []string{"nginx", "-g", "daemon off;"},
				},
			},
		},
	}
	ds := &dockerServices{c: dc}
	svc := composetypes.ServiceConfig{Image: "nginx:latest"}
	ep, cmd, err := ds.ResolveEntrypoint(context.Background(), "web", svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ep) != 1 || ep[0] != "/docker-entrypoint.sh" {
		t.Errorf("entrypoint = %v, want [/docker-entrypoint.sh]", ep)
	}
	if len(cmd) != 3 || cmd[0] != "nginx" {
		t.Errorf("command = %v, want [nginx -g daemon off;]", cmd)
	}
}

func TestResolveEntrypointImageNotFound(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	ds := &dockerServices{c: dc}
	svc := composetypes.ServiceConfig{Image: "myapp:latest"}
	_, _, err := ds.ResolveEntrypoint(context.Background(), "myapp", svc)
	if err == nil {
		t.Fatal("expected error for missing image, got nil")
	}
}
