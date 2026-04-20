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
	inspectCalled int                         // number of ImageInspect invocations
	pullErr       error                       // error to return from ImagePull; nil = success
	pullCalled    bool
}

func (f *fakeDockerClient) ImageInspect(_ context.Context, _ string, _ ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	f.inspectCalled++
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

// imageConfig builds a fake inspect result with the given ENTRYPOINT and CMD.
func imageConfig(entrypoint, cmd []string) dockerimage.InspectResponse {
	return dockerimage.InspectResponse{
		Config: &dockerspec.DockerOCIImageConfig{
			ImageConfig: ocispec.ImageConfig{
				Entrypoint: entrypoint,
				Cmd:        cmd,
			},
		},
	}
}

func strEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestResolveEntrypointMatrix exercises the four combinations of compose
// entrypoint/command presence against image defaults.
func TestResolveEntrypointMatrix(t *testing.T) {
	imgEP := []string{"/docker-entrypoint.sh"}
	imgCmd := []string{"nginx", "-g", "daemon off;"}

	tests := []struct {
		name           string
		composeEP      []string
		composeCmd     []string
		wantEP         []string
		wantCmd        []string
		wantInspect    bool
	}{
		{
			name:        "both set — no inspect",
			composeEP:   []string{"/app"},
			composeCmd:  []string{"--flag"},
			wantEP:      []string{"/app"},
			wantCmd:     []string{"--flag"},
			wantInspect: false,
		},
		{
			name:        "entrypoint only — no inspect, command empty (Docker resets CMD)",
			composeEP:   []string{"/app"},
			composeCmd:  nil,
			wantEP:      []string{"/app"},
			wantCmd:     nil,
			wantInspect: false,
		},
		{
			name:        "command only — image ENTRYPOINT preserved",
			composeEP:   nil,
			composeCmd:  []string{"worker"},
			wantEP:      imgEP,
			wantCmd:     []string{"worker"},
			wantInspect: true,
		},
		{
			name:        "neither — both from image",
			composeEP:   nil,
			composeCmd:  nil,
			wantEP:      imgEP,
			wantCmd:     imgCmd,
			wantInspect: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dc := &fakeDockerClient{inspectResult: imageConfig(imgEP, imgCmd)}
			ds := &dockerServices{c: dc}
			svc := composetypes.ServiceConfig{
				Image:      "nginx:latest",
				Entrypoint: tc.composeEP,
				Command:    tc.composeCmd,
			}
			ep, cmd, err := ds.ResolveEntrypoint(context.Background(), "web", svc)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strEq(ep, tc.wantEP) {
				t.Errorf("entrypoint = %v, want %v", ep, tc.wantEP)
			}
			if !strEq(cmd, tc.wantCmd) {
				t.Errorf("command = %v, want %v", cmd, tc.wantCmd)
			}
			if tc.wantInspect && dc.inspectCalled == 0 {
				t.Error("expected ImageInspect to be called")
			}
			if !tc.wantInspect && dc.inspectCalled != 0 {
				t.Errorf("ImageInspect called %d times; expected 0", dc.inspectCalled)
			}
		})
	}
}

// --- ImageExists tests ---

func TestImageExistsPresent(t *testing.T) {
	dc := &fakeDockerClient{notFound: false}
	ds := &dockerServices{c: dc}
	ok, err := ds.ImageExists(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected ImageExists=true for present image")
	}
}

func TestImageExistsAbsent(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	ds := &dockerServices{c: dc}
	ok, err := ds.ImageExists(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatalf("unexpected error on NotFound: %v", err)
	}
	if ok {
		t.Error("expected ImageExists=false for absent image")
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

func TestResolveEntrypointNoImageNoEntrypoint(t *testing.T) {
	dc := &fakeDockerClient{}
	ds := &dockerServices{c: dc}
	svc := composetypes.ServiceConfig{Command: []string{"worker"}}
	_, _, err := ds.ResolveEntrypoint(context.Background(), "svc", svc)
	if err == nil {
		t.Fatal("expected error when neither image nor entrypoint is set")
	}
	if dc.inspectCalled != 0 {
		t.Errorf("ImageInspect called %d times; expected 0 (no image)", dc.inspectCalled)
	}
}
