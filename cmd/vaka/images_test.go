// cmd/vaka/images_test.go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	dockercli "github.com/docker/cli/cli/command"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
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
	return io.NopCloser(strings.NewReader("{\"status\":\"Pulling from emsi/vaka-init\"}\n")), nil
}

// imageConfig builds a fake inspect result with ENTRYPOINT/CMD/USER defaults.
func imageConfig(entrypoint, cmd []string, user string) dockerimage.InspectResponse {
	return dockerimage.InspectResponse{
		Config: &dockerspec.DockerOCIImageConfig{
			ImageConfig: ocispec.ImageConfig{
				Entrypoint: entrypoint,
				Cmd:        cmd,
				User:       user,
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

func TestDockerTargetDescriptionPrecedence(t *testing.T) {
	t.Setenv(client.EnvOverrideHost, "")
	t.Setenv(dockercli.EnvOverrideContext, "")

	oldConfigDir := dockerconfig.Dir()
	configDir := t.TempDir()
	dockerconfig.SetDir(configDir)
	t.Cleanup(func() {
		dockerconfig.SetDir(oldConfigDir)
	})

	cfg := &configfile.ConfigFile{CurrentContext: "cfg-context"}
	configPath := filepath.Join(configDir, dockerconfig.ConfigFileName)
	tests := []struct {
		name      string
		host      string
		envCtx    string
		cfg       *configfile.ConfigFile
		wantDescr string
	}{
		{
			name:      "docker host wins over docker context env",
			host:      "tcp://remote:2376",
			envCtx:    "ctx-env",
			cfg:       cfg,
			wantDescr: `daemon "tcp://remote:2376" (from DOCKER_HOST)`,
		},
		{
			name:      "docker context env when no host",
			host:      "",
			envCtx:    "ctx-env",
			cfg:       cfg,
			wantDescr: `context "ctx-env" (from DOCKER_CONTEXT)`,
		},
		{
			name:      "config current context fallback",
			host:      "",
			envCtx:    "",
			cfg:       cfg,
			wantDescr: fmt.Sprintf(`context "cfg-context" (from %s)`, configPath),
		},
		{
			name:      "default context fallback",
			host:      "",
			envCtx:    "",
			cfg:       &configfile.ConfigFile{},
			wantDescr: "default Docker context",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(client.EnvOverrideHost, tc.host)
			t.Setenv(dockercli.EnvOverrideContext, tc.envCtx)
			got := dockerTargetDescription(tc.cfg)
			if got != tc.wantDescr {
				t.Fatalf("dockerTargetDescription()=%q, want %q", got, tc.wantDescr)
			}
		})
	}
}

// --- EnsureImage tests ---

func TestEnsureImagePresent(t *testing.T) {
	dc := &fakeDockerClient{notFound: false}
	e := &dockerServices{c: dc, targetDesc: "test-context"}
	if err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0"); err != nil {
		t.Fatalf("present: unexpected error: %v", err)
	}
	if dc.pullCalled {
		t.Error("present: pull must not be called when image is already present")
	}
}

func TestEnsureImageAbsentPullSucceeds(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	e := &dockerServices{c: dc, targetDesc: "test-context"}
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
	e := &dockerServices{c: dc, targetDesc: "test-context"}
	err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0")
	if err == nil {
		t.Fatal("pull fails: expected error, got nil")
	}
	if !errors.Is(err, pullErr) {
		t.Errorf("pull fails: expected %v wrapped, got %v", pullErr, err)
	}
}

// --- ResolveRuntime tests ---

// TestResolveRuntimeMatrix exercises compose/image runtime resolution for
// entrypoint/cmd and user fallback.
func TestResolveRuntimeMatrix(t *testing.T) {
	imgEP := []string{"/docker-entrypoint.sh"}
	imgCmd := []string{"nginx", "-g", "daemon off;"}
	imgUser := "1001:1002"

	tests := []struct {
		name          string
		composeEP     []string
		composeCmd    []string
		composeUser   string
		wantEP        []string
		wantCmd       []string
		wantImageUser string
		wantInspect   bool
	}{
		{
			name:          "entrypoint and user set no inspect",
			composeEP:     []string{"/app"},
			composeCmd:    []string{"--flag"},
			composeUser:   "app",
			wantEP:        []string{"/app"},
			wantCmd:       []string{"--flag"},
			wantImageUser: "",
			wantInspect:   false,
		},
		{
			name:          "entrypoint only with compose user set no inspect",
			composeEP:     []string{"/app"},
			composeCmd:    nil,
			composeUser:   "1000:1000",
			wantEP:        []string{"/app"},
			wantCmd:       nil,
			wantImageUser: "",
			wantInspect:   false,
		},
		{
			name:          "command only with compose user set needs image entrypoint",
			composeEP:     nil,
			composeCmd:    []string{"worker"},
			composeUser:   "1000",
			wantEP:        imgEP,
			wantCmd:       []string{"worker"},
			wantImageUser: "",
			wantInspect:   true,
		},
		{
			name:          "neither entrypoint nor user set",
			composeEP:     nil,
			composeCmd:    nil,
			composeUser:   "",
			wantEP:        imgEP,
			wantCmd:       imgCmd,
			wantImageUser: imgUser,
			wantInspect:   true,
		},
		{
			name:          "entrypoint set and user empty image user fallback",
			composeEP:     []string{"/app"},
			composeCmd:    []string{"serve"},
			composeUser:   "",
			wantEP:        []string{"/app"},
			wantCmd:       []string{"serve"},
			wantImageUser: imgUser,
			wantInspect:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dc := &fakeDockerClient{inspectResult: imageConfig(imgEP, imgCmd, imgUser)}
			ds := &dockerServices{c: dc, targetDesc: "test-context"}
			svc := composetypes.ServiceConfig{
				Image:      "nginx:latest",
				Entrypoint: tc.composeEP,
				Command:    tc.composeCmd,
				User:       tc.composeUser,
			}
			got, err := ds.ResolveRuntime(context.Background(), "web", svc)
			if err != nil {
				t.Fatalf("ResolveRuntime unexpected error: %v", err)
			}
			if !strEq(got.Entrypoint, tc.wantEP) {
				t.Errorf("entrypoint = %v, want %v", got.Entrypoint, tc.wantEP)
			}
			if !strEq(got.Command, tc.wantCmd) {
				t.Errorf("command = %v, want %v", got.Command, tc.wantCmd)
			}
			if got.ImageUser != tc.wantImageUser {
				t.Errorf("image user = %q, want %q", got.ImageUser, tc.wantImageUser)
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
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
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
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	ok, err := ds.ImageExists(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatalf("unexpected error on NotFound: %v", err)
	}
	if ok {
		t.Error("expected ImageExists=false for absent image")
	}
}

func TestResolveRuntimeImageNotFound(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	svc := composetypes.ServiceConfig{Image: "myapp:latest"}
	_, err := ds.ResolveRuntime(context.Background(), "myapp", svc)
	if err == nil {
		t.Fatal("expected error for missing image, got nil")
	}
}

func TestResolveRuntimeNoImageNeedsFallback(t *testing.T) {
	dc := &fakeDockerClient{}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	svc := composetypes.ServiceConfig{Command: []string{"worker"}}
	_, err := ds.ResolveRuntime(context.Background(), "svc", svc)
	if err == nil {
		t.Fatal("expected error when image fallback is needed but image is unset")
	}
	if dc.inspectCalled != 0 {
		t.Errorf("ImageInspect called %d times; expected 0 (no image)", dc.inspectCalled)
	}
}

func TestResolveRuntimeNoImageButComposeHasAllRuntimeFields(t *testing.T) {
	dc := &fakeDockerClient{}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	svc := composetypes.ServiceConfig{
		Entrypoint: []string{"/usr/local/bin/app"},
		Command:    []string{"serve"},
		User:       "1000:1000",
	}
	got, err := ds.ResolveRuntime(context.Background(), "svc", svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strEq(got.Entrypoint, svc.Entrypoint) {
		t.Errorf("entrypoint = %v, want %v", got.Entrypoint, svc.Entrypoint)
	}
	if !strEq(got.Command, svc.Command) {
		t.Errorf("command = %v, want %v", got.Command, svc.Command)
	}
	if got.ImageUser != "" {
		t.Errorf("image user = %q, want empty", got.ImageUser)
	}
	if dc.inspectCalled != 0 {
		t.Errorf("ImageInspect called %d times; expected 0", dc.inspectCalled)
	}
}
