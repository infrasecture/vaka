// cmd/vaka/images_test.go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dockercli "github.com/docker/cli/cli/command"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	dockertypes "github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"vaka.dev/vaka/internal/runtimebundle"
)

// fakeDockerClient implements dockerClient for unit tests without a live daemon.
type fakeDockerClient struct {
	serverVersion          dockertypes.Version
	clientVersion          string
	serverErr              error
	notFound               bool                        // ImageInspect returns NotFound when true
	inspectResult          dockerimage.InspectResponse // returned when notFound == false
	inspectResults         map[string]dockerimage.InspectResponse
	inspectErrs            map[string]error
	inspectRefs            []string
	inspectCalled          int   // number of ImageInspect invocations
	pullErr                error // error to return from ImagePull; nil = success
	pullCalled             bool
	pullRefs               []string
	createErr              error
	createCalled           int
	createConfig           *containertypes.Config
	containerInspectResult containertypes.InspectResponse
	containerInspectErr    error
	containerInspected     []string
	statResults            map[string]containertypes.PathStat
	statErrs               map[string]error
	statPaths              []string
	removeErr              error
	removed                []string
}

func (f *fakeDockerClient) ClientVersion() string {
	if f.clientVersion != "" {
		return f.clientVersion
	}
	return minimumDockerAPIVersion
}

func (f *fakeDockerClient) ServerVersion(context.Context) (dockertypes.Version, error) {
	if f.serverErr != nil {
		return dockertypes.Version{}, f.serverErr
	}
	if f.serverVersion.Version != "" || f.serverVersion.APIVersion != "" {
		return f.serverVersion, nil
	}
	return dockertypes.Version{Version: minimumDockerEngineVersion, APIVersion: minimumDockerAPIVersion, Arch: "amd64"}, nil
}

func (f *fakeDockerClient) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	f.inspectCalled++
	f.inspectRefs = append(f.inspectRefs, ref)
	if err, ok := f.inspectErrs[ref]; ok {
		return dockerimage.InspectResponse{}, err
	}
	if result, ok := f.inspectResults[ref]; ok {
		return result, nil
	}
	if f.notFound {
		return dockerimage.InspectResponse{}, errdefs.NotFound(errors.New("not found"))
	}
	return f.inspectResult, nil
}

func (f *fakeDockerClient) ImagePull(_ context.Context, ref string, _ dockerimage.PullOptions) (io.ReadCloser, error) {
	f.pullCalled = true
	f.pullRefs = append(f.pullRefs, ref)
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	f.notFound = false
	return io.NopCloser(strings.NewReader("{\"status\":\"Pulling from emsi/vaka-init\"}\n")), nil
}

func (f *fakeDockerClient) ContainerCreate(_ context.Context, config *containertypes.Config, _ *containertypes.HostConfig, _ *networktypes.NetworkingConfig, _ *ocispec.Platform, _ string) (containertypes.CreateResponse, error) {
	f.createCalled++
	f.createConfig = config
	if f.createErr != nil {
		return containertypes.CreateResponse{}, f.createErr
	}
	return containertypes.CreateResponse{ID: "rootfs-probe"}, nil
}

func (f *fakeDockerClient) ContainerInspect(_ context.Context, containerID string) (containertypes.InspectResponse, error) {
	f.containerInspected = append(f.containerInspected, containerID)
	if f.containerInspectErr != nil {
		return containertypes.InspectResponse{}, f.containerInspectErr
	}
	if f.containerInspectResult.ContainerJSONBase != nil || f.containerInspectResult.Config != nil {
		return f.containerInspectResult, nil
	}
	return containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
		ID: containerID, HostConfig: &containertypes.HostConfig{},
	}}, nil
}

func (f *fakeDockerClient) ContainerStatPath(_ context.Context, containerID, candidate string) (containertypes.PathStat, error) {
	f.statPaths = append(f.statPaths, containerID+":"+candidate)
	if err, ok := f.statErrs[candidate]; ok {
		return containertypes.PathStat{}, err
	}
	if stat, ok := f.statResults[candidate]; ok {
		return stat, nil
	}
	if containerID != "rootfs-probe" && candidate == protectedRuntimePath {
		return containertypes.PathStat{Name: "vaka", Mode: os.ModeDir | 0o555}, nil
	}
	return containertypes.PathStat{}, errdefs.NotFound(errors.New("path not found"))
}

func (f *fakeDockerClient) ContainerRemove(_ context.Context, containerID string, _ containertypes.RemoveOptions) error {
	f.removed = append(f.removed, containerID)
	return f.removeErr
}

// imageConfig builds a fake inspect result with ENTRYPOINT/CMD/USER defaults.
func imageConfig(entrypoint, cmd []string, user string) dockerimage.InspectResponse {
	return dockerimage.InspectResponse{
		ID: testRuntimeImageID,
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

func TestCheckRuntimeCompatibility(t *testing.T) {
	originalQuery := queryComposeVersion
	t.Cleanup(func() { queryComposeVersion = originalQuery })

	tests := []struct {
		name             string
		server           dockertypes.Version
		clientVersion    string
		composeVersion   string
		wantCompactMount bool
		wantErr          string
	}{
		{
			name:           "minimum versions",
			server:         dockertypes.Version{Version: "28.0.0", APIVersion: "1.48"},
			composeVersion: "2.35.0",
		},
		{
			name:             "engine 29.1 with compose 5.0",
			server:           dockertypes.Version{Version: "29.1.3", APIVersion: "1.52"},
			composeVersion:   "5.0.1",
			wantCompactMount: true,
		},
		{
			name:           "engine 29.1 with compose 5.1 path bug",
			server:         dockertypes.Version{Version: "29.1.3", APIVersion: "1.52"},
			composeVersion: "5.1.0",
			wantErr:        "path-length incompatibility",
		},
		{
			name:           "engine too old",
			server:         dockertypes.Version{Version: "27.5.0", APIVersion: "1.47"},
			composeVersion: "2.35.0",
			wantErr:        "Docker Engine",
		},
		{
			name:           "client API override too old",
			server:         dockertypes.Version{Version: "28.0.0", APIVersion: "1.48"},
			clientVersion:  "1.47",
			composeVersion: "2.35.0",
			wantErr:        "Docker client API",
		},
		{
			name:           "compose too old",
			server:         dockertypes.Version{Version: "28.0.0", APIVersion: "1.48"},
			composeVersion: "2.34.0",
			wantErr:        "Docker Compose",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queryComposeVersion = func(context.Context) (string, error) {
				return tc.composeVersion, nil
			}
			ds := &dockerServices{
				c:          &fakeDockerClient{serverVersion: tc.server, clientVersion: tc.clientVersion},
				targetDesc: "test-context",
			}
			err := ds.CheckRuntimeCompatibility(context.Background())
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want contains %q", err, tc.wantErr)
			}
			if err == nil && ds.useCompactImageMountSource != tc.wantCompactMount {
				t.Fatalf("useCompactImageMountSource = %v, want %v", ds.useCompactImageMountSource, tc.wantCompactMount)
			}
		})
	}
}

// --- ResolveRuntimeImage tests ---

const testRuntimeImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testRuntimeImageRef = "emsi/vaka-init:runtime-v0.1.0"
const testRuntimeAMD64Ref = testRuntimeImageRef + "-amd64"

func runtimeImageConfig(version string) dockerimage.InspectResponse {
	inspect := imageConfig(nil, nil, "")
	inspect.ID = testRuntimeImageID
	inspect.Config.Labels = map[string]string{runtimebundle.VersionLabel: version}
	return inspect
}

func TestResolveRuntimeImageReturnsExactIDWithoutPull(t *testing.T) {
	dc := &fakeDockerClient{inspectResult: runtimeImageConfig("v0.1.0")}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	got, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", true)
	if err != nil {
		t.Fatalf("ResolveRuntimeImage: %v", err)
	}
	if got.ID != testRuntimeImageID {
		t.Fatalf("ID = %q, want %q", got.ID, testRuntimeImageID)
	}
	if got.MountSource != testRuntimeImageID {
		t.Fatalf("MountSource = %q, want complete ID", got.MountSource)
	}
	if dc.pullCalled {
		t.Fatal("present compatible runtime image was re-pulled")
	}
	wantRefs := []string{testRuntimeAMD64Ref, testRuntimeImageID}
	if !strEq(dc.inspectRefs, wantRefs) {
		t.Fatalf("inspect refs = %v, want %v", dc.inspectRefs, wantRefs)
	}
}

func TestResolveRuntimeImageUsesCompactSourceForAffectedEngine(t *testing.T) {
	dc := &fakeDockerClient{inspectResult: runtimeImageConfig("v0.1.0")}
	ds := &dockerServices{
		c:                          dc,
		targetDesc:                 "test-context",
		useCompactImageMountSource: true,
	}
	got, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", false)
	if err != nil {
		t.Fatalf("ResolveRuntimeImage: %v", err)
	}
	if got.ID != testRuntimeImageID || got.MountSource != strings.Repeat("a", 40) {
		t.Fatalf("resolved image = %+v", got)
	}
}

func TestResolveRuntimeImageMissingRespectsRepair(t *testing.T) {
	dc := &fakeDockerClient{notFound: true, inspectResult: runtimeImageConfig("v0.1.0")}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	if _, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", false); err == nil {
		t.Fatal("missing runtime image accepted without repair")
	}
	if dc.pullCalled {
		t.Fatal("repair=false pulled missing runtime image")
	}
	got, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", true)
	if err != nil {
		t.Fatalf("repair missing runtime image: %v", err)
	}
	if got.ID != testRuntimeImageID || !dc.pullCalled {
		t.Fatalf("resolved=%+v pullCalled=%v", got, dc.pullCalled)
	}
	if !strEq(dc.pullRefs, []string{testRuntimeAMD64Ref}) {
		t.Fatalf("pull refs = %v, want [%s]", dc.pullRefs, testRuntimeAMD64Ref)
	}
}

func TestResolveRuntimeImageRejectsVersionMismatch(t *testing.T) {
	dc := &fakeDockerClient{inspectResult: runtimeImageConfig("v0.0.9")}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	_, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", false)
	if err == nil || !strings.Contains(err.Error(), "has bundle version v0.0.9, require v0.1.0") {
		t.Fatalf("version mismatch error = %v", err)
	}
	if dc.pullCalled {
		t.Fatal("repair=false refreshed incompatible runtime image")
	}
}

func TestResolveRuntimeImagePullFailureIsWrapped(t *testing.T) {
	pullErr := errors.New("network unreachable")
	dc := &fakeDockerClient{notFound: true, pullErr: pullErr}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	_, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", true)
	if err == nil || !errors.Is(err, pullErr) {
		t.Fatalf("pull failure = %v, want wrapped %v", err, pullErr)
	}
}

func TestResolveRuntimeImageRejectsInvalidImageID(t *testing.T) {
	inspect := runtimeImageConfig("v0.1.0")
	inspect.ID = "runtime:mutable"
	dc := &fakeDockerClient{inspectResult: inspect}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	_, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", false)
	if err == nil || !strings.Contains(err.Error(), "invalid image ID") {
		t.Fatalf("invalid ID error = %v", err)
	}
}

func TestNormalizeRuntimeArchitecture(t *testing.T) {
	tests := map[string]string{
		"amd64":   "amd64",
		"x86_64":  "amd64",
		"ARM64":   "arm64",
		"aarch64": "arm64",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := normalizeRuntimeArchitecture(input)
			if err != nil || got != want {
				t.Fatalf("normalizeRuntimeArchitecture(%q) = %q, %v; want %q", input, got, err, want)
			}
		})
	}
	if _, err := normalizeRuntimeArchitecture("riscv64"); err == nil || !strings.Contains(err.Error(), "supported architectures: amd64, arm64") {
		t.Fatalf("unsupported architecture error = %v", err)
	}
}

func TestResolveRuntimeImageUsesDockerTargetArchitecture(t *testing.T) {
	dc := &fakeDockerClient{
		serverVersion: dockertypes.Version{Version: "29.2.1", APIVersion: "1.53", Arch: "aarch64"},
		inspectResult: runtimeImageConfig("v0.1.0"),
	}
	ds := &dockerServices{c: dc, targetDesc: "context colima"}
	if _, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", false); err != nil {
		t.Fatalf("ResolveRuntimeImage: %v", err)
	}
	wantRefs := []string{testRuntimeImageRef + "-arm64", testRuntimeImageID}
	if !strEq(dc.inspectRefs, wantRefs) {
		t.Fatalf("inspect refs = %v, want %v", dc.inspectRefs, wantRefs)
	}
}

func TestResolveRuntimeImageRejectsUnsupportedDockerTargetArchitecture(t *testing.T) {
	dc := &fakeDockerClient{serverVersion: dockertypes.Version{Version: "29.2.1", APIVersion: "1.53", Arch: "riscv64"}}
	ds := &dockerServices{c: dc, targetDesc: "remote-context"}
	_, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", true)
	if err == nil || !strings.Contains(err.Error(), `architecture "riscv64"`) || len(dc.inspectRefs) != 0 || dc.pullCalled {
		t.Fatalf("unsupported target result: err=%v inspectRefs=%v pullCalled=%v", err, dc.inspectRefs, dc.pullCalled)
	}
}

func TestResolveRuntimeImageRequiresVersionedReference(t *testing.T) {
	dc := &fakeDockerClient{}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	_, err := ds.ResolveRuntimeImage(context.Background(), "emsi/vaka-init", "v0.1.0", true)
	if err == nil || !strings.Contains(err.Error(), "has no version tag") || len(dc.inspectRefs) != 0 || dc.pullCalled {
		t.Fatalf("untagged reference result: err=%v inspectRefs=%v pullCalled=%v", err, dc.inspectRefs, dc.pullCalled)
	}
}

func TestResolveRuntimeImageTargetArchitectureQueryFailure(t *testing.T) {
	serverErr := errors.New("daemon unavailable")
	dc := &fakeDockerClient{serverErr: serverErr}
	ds := &dockerServices{c: dc, targetDesc: "context colima"}
	_, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", true)
	if err == nil || !errors.Is(err, serverErr) || !strings.Contains(err.Error(), "query Docker target architecture") {
		t.Fatalf("target architecture query error = %v", err)
	}
}

func TestResolveRuntimeImageRejectsContentOnlyMountIdentity(t *testing.T) {
	notFound := errdefs.NotFound(errors.New("content exists without image record"))
	dc := &fakeDockerClient{
		inspectResult: runtimeImageConfig("v0.1.0"),
		inspectErrs:   map[string]error{testRuntimeImageID: notFound},
	}
	ds := &dockerServices{c: dc, targetDesc: "context colima"}
	_, err := ds.ResolveRuntimeImage(context.Background(), testRuntimeImageRef, "v0.1.0", false)
	if err == nil || !strings.Contains(err.Error(), "not directly inspectable") {
		t.Fatalf("content-only identity error = %v", err)
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
			name:          "entrypoint and user set still inspects image healthcheck",
			composeEP:     []string{"/app"},
			composeCmd:    []string{"--flag"},
			composeUser:   "app",
			wantEP:        []string{"/app"},
			wantCmd:       []string{"--flag"},
			wantImageUser: "",
			wantInspect:   true,
		},
		{
			name:          "entrypoint only with compose user still inspects image healthcheck",
			composeEP:     []string{"/app"},
			composeCmd:    nil,
			composeUser:   "1000:1000",
			wantEP:        []string{"/app"},
			wantCmd:       nil,
			wantImageUser: "",
			wantInspect:   true,
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
			name:          "explicit empty entrypoint clears image entrypoint and command",
			composeEP:     []string{},
			composeCmd:    nil,
			composeUser:   "1000",
			wantEP:        []string{},
			wantCmd:       nil,
			wantImageUser: "",
			wantInspect:   true,
		},
		{
			name:          "explicit empty command clears image command",
			composeEP:     nil,
			composeCmd:    []string{},
			composeUser:   "1000",
			wantEP:        imgEP,
			wantCmd:       []string{},
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

func TestResolveRuntimeIncludesInheritedHealthcheckAndShell(t *testing.T) {
	image := imageConfig([]string{"/app"}, nil, "1000:1000")
	image.Config.Healthcheck = &dockerspec.HealthcheckConfig{Test: []string{"CMD-SHELL", "check-ready || exit 1"}}
	image.Config.Shell = []string{"/bin/bash", "-o", "pipefail", "-c"}
	ds := &dockerServices{c: &fakeDockerClient{inspectResult: image}, targetDesc: "test-context"}
	got, err := ds.ResolveRuntime(context.Background(), "app", composetypes.ServiceConfig{
		Image:      "app:latest",
		Entrypoint: []string{"/app"},
		User:       "1000:1000",
	})
	if err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if !strEq(got.Healthcheck, image.Config.Healthcheck.Test) {
		t.Fatalf("healthcheck = %v, want %v", got.Healthcheck, image.Config.Healthcheck.Test)
	}
	if !strEq(got.HealthcheckShell, image.Config.Shell) {
		t.Fatalf("healthcheck shell = %v, want %v", got.HealthcheckShell, image.Config.Shell)
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

func TestResolveRuntimeExplicitNeverDoesNotUseVakaPullFallback(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	ds := &dockerServices{c: dc, targetDesc: "test-context", pullPolicy: PullMissing}
	_, err := ds.ResolveRuntime(context.Background(), "myapp", composetypes.ServiceConfig{
		Image:      "myapp:latest",
		PullPolicy: composetypes.PullPolicyNever,
	})
	if err == nil || !strings.Contains(err.Error(), `effective Compose pull policy "never"`) {
		t.Fatalf("error = %v, want explicit policy explanation", err)
	}
	if dc.pullCalled {
		t.Fatal("explicit pull_policy: never was overridden by --vaka-pull")
	}
}

func TestResolveRuntimeUsesVakaPullFallbackWithoutComposePolicy(t *testing.T) {
	dc := &fakeDockerClient{notFound: true, inspectResult: imageConfig([]string{"/app"}, nil, "1000")}
	ds := &dockerServices{c: dc, targetDesc: "test-context", pullPolicy: PullMissing}
	if _, err := ds.ResolveRuntime(context.Background(), "myapp", composetypes.ServiceConfig{Image: "myapp:latest"}); err != nil {
		t.Fatalf("ResolveRuntime: %v", err)
	}
	if !dc.pullCalled {
		t.Fatal("missing image did not use --vaka-pull fallback")
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

func TestResolveRuntimeNoImageFailsEvenWithComposeRuntimeFields(t *testing.T) {
	dc := &fakeDockerClient{}
	ds := &dockerServices{c: dc, targetDesc: "test-context"}
	svc := composetypes.ServiceConfig{
		Entrypoint: []string{"/usr/local/bin/app"},
		Command:    []string{"serve"},
		User:       "1000:1000",
	}
	_, err := ds.ResolveRuntime(context.Background(), "svc", svc)
	if err == nil || !strings.Contains(err.Error(), "exact service image") {
		t.Fatalf("error = %v, want exact image requirement", err)
	}
	if dc.inspectCalled != 0 {
		t.Errorf("ImageInspect called %d times; expected 0", dc.inspectCalled)
	}
}

func TestResolveRuntimeRejectsProtectedImageVolume(t *testing.T) {
	image := imageConfig([]string{"/app"}, nil, "1000:1000")
	image.Config.Volumes = map[string]struct{}{protectedRuntimePath + "/sbin": {}}
	ds := &dockerServices{c: &fakeDockerClient{inspectResult: image}, targetDesc: "test-context"}
	_, err := ds.ResolveRuntime(context.Background(), "app", composetypes.ServiceConfig{Image: "app:latest", Entrypoint: []string{"/app"}, User: "1000:1000"})
	if err == nil || !strings.Contains(err.Error(), "declares VOLUME") {
		t.Fatalf("protected image volume error = %v", err)
	}
}
