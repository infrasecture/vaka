package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/config/configfile"
	clitypes "github.com/docker/cli/cli/config/types"
	dockertypes "github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestParsePullPolicy(t *testing.T) {
	cases := map[string]PullPolicy{
		"":               PullMissingPinned, // default
		"missing-pinned": PullMissingPinned,
		"missing":        PullMissing,
		"never":          PullNever,
	}
	for in, want := range cases {
		got, err := ParsePullPolicy(in)
		if err != nil {
			t.Errorf("ParsePullPolicy(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Errorf("ParsePullPolicy(%q) = %v, want %v", in, got, want)
		}
	}
	// "always" was removed; it must now be rejected like any other invalid value.
	for _, bad := range []string{"always", "sometimes"} {
		if _, err := ParsePullPolicy(bad); err == nil {
			t.Errorf("ParsePullPolicy(%q) expected an error", bad)
		}
	}
}

func TestIsDigestPinned(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	pinned := []string{
		"nginx@sha256:" + hex64,
		"nginx:1.2@sha256:" + hex64,
		"ghcr.io/x/y:tag@sha256:" + hex64,
	}
	unpinned := []string{
		"nginx", "nginx:latest", "nginx@sha256:tooshort",
		"nginx@sha256:" + strings.Repeat("g", 64), // non-hex
	}
	for _, r := range pinned {
		if !isDigestPinned(r) {
			t.Errorf("isDigestPinned(%q) = false, want true", r)
		}
	}
	for _, r := range unpinned {
		if isDigestPinned(r) {
			t.Errorf("isDigestPinned(%q) = true, want false", r)
		}
	}
}

// pullFake is a stateful dockerClient: ImagePull can make a previously-absent
// image present, so pull-then-inspect flows are exercisable.
type pullFake struct {
	present  bool
	pullOK   bool // pull succeeds and makes the image present
	pullErr  error
	pulls    int
	inspects int
}

func (f *pullFake) ServerVersion(context.Context) (dockertypes.Version, error) {
	return dockertypes.Version{Version: minimumDockerEngineVersion, APIVersion: minimumDockerAPIVersion}, nil
}

func (f *pullFake) ClientVersion() string { return minimumDockerAPIVersion }

func (f *pullFake) ImageInspect(context.Context, string, ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	f.inspects++
	if !f.present {
		return dockerimage.InspectResponse{}, errdefs.NotFound(errors.New("not found"))
	}
	return imageConfig([]string{"/app"}, nil, ""), nil
}

func (f *pullFake) ImagePull(context.Context, string, dockerimage.PullOptions) (io.ReadCloser, error) {
	f.pulls++
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	if f.pullOK {
		f.present = true
	}
	return io.NopCloser(strings.NewReader("{}\n")), nil
}

func (f *pullFake) ContainerCreate(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *ocispec.Platform, string) (containertypes.CreateResponse, error) {
	return containertypes.CreateResponse{ID: "rootfs-probe"}, nil
}

func (f *pullFake) ContainerInspect(context.Context, string) (containertypes.InspectResponse, error) {
	return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
		ID: "rootfs-probe", HostConfig: &containertypes.HostConfig{},
	}}, nil
}

func (f *pullFake) ContainerStatPath(context.Context, string, string) (containertypes.PathStat, error) {
	return containertypes.PathStat{}, errdefs.NotFound(errors.New("path not found"))
}

func (f *pullFake) ContainerRemove(context.Context, string, containertypes.RemoveOptions) error {
	return nil
}

func TestResolveRuntimePullPolicy(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	pinned := "app@sha256:" + hex64
	unpinned := "app:latest"

	cases := []struct {
		name      string
		policy    PullPolicy
		ref       string
		present   bool
		pullOK    bool
		pullErr   error
		wantPulls int
		wantErr   bool
	}{
		{"never: missing → error, no pull", PullNever, pinned, false, true, nil, 0, true},
		{"missing-pinned: missing+pinned → pull", PullMissingPinned, pinned, false, true, nil, 1, false},
		{"missing-pinned: missing+unpinned → no pull, error", PullMissingPinned, unpinned, false, true, nil, 0, true},
		{"missing: missing+unpinned → pull", PullMissing, unpinned, false, true, nil, 1, false},
		{"missing-pinned: present → no pull", PullMissingPinned, pinned, true, true, nil, 0, false},
		{"missing-pinned: pull fails → error", PullMissingPinned, pinned, false, false, errors.New("net down"), 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &pullFake{present: tc.present, pullOK: tc.pullOK, pullErr: tc.pullErr}
			ds := &dockerServices{c: fake, targetDesc: "test", pullPolicy: tc.policy}
			// Empty entrypoint/user forces image inspection.
			_, err := ds.ResolveRuntime(context.Background(), "app", composetypes.ServiceConfig{Image: tc.ref})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if fake.pulls != tc.wantPulls {
				t.Errorf("pulls = %d, want %d", fake.pulls, tc.wantPulls)
			}
		})
	}
}

func TestPullOptionsAuth(t *testing.T) {
	hex := strings.Repeat("a", 64)
	cfg := configfile.New("")
	cfg.AuthConfigs["myreg.example.com"] = clitypes.AuthConfig{Username: "u", Password: "p"}
	ds := &dockerServices{cfg: cfg, targetDesc: "test"}

	if ds.pullOptions("myreg.example.com/app@sha256:"+hex).RegistryAuth == "" {
		t.Error("configured registry: expected non-empty RegistryAuth")
	}
	if ds.pullOptions("other.example.com/app:latest").RegistryAuth != "" {
		t.Error("unconfigured registry: expected empty RegistryAuth (anonymous)")
	}
	nilCfg := &dockerServices{targetDesc: "test"}
	if nilCfg.pullOptions("x@sha256:"+hex).RegistryAuth != "" {
		t.Error("nil cfg: expected empty RegistryAuth")
	}
}
