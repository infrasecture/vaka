package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	dockercli "github.com/docker/cli/cli/command"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	dockerflags "github.com/docker/cli/cli/flags"
	dockertypes "github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	registrytypes "github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/registry"
	"github.com/moby/term"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/pflag"
	"vaka.dev/vaka/internal/runtimebundle"
)

// DockerServices is the interface for all Docker daemon interactions in vaka.
// A single implementation is created per runFull invocation; a test double can
// replace it entirely.
type DockerServices interface {
	// CheckRuntimeCompatibility verifies the selected Engine and local Compose
	// plugin before generating image-mount syntax.
	CheckRuntimeCompatibility(ctx context.Context) error
	// ResolveRuntimeImage returns the complete immutable local ID of Vaka's
	// runtime image and the source syntax selected for the detected Engine and
	// Compose versions. When repair is true, a missing or incompatible image is
	// pulled once before failing.
	ResolveRuntimeImage(ctx context.Context, ref, expectedVersion string, repair bool) (ResolvedImage, error)
	// ImageExists returns true if ref is available locally. Transport errors
	// other than NotFound are propagated.
	ImageExists(ctx context.Context, ref string) (bool, error)
	// ResolveRuntime resolves runtime metadata needed by vaka:
	// effective entrypoint/command vectors and image-level USER fallback.
	ResolveRuntime(ctx context.Context, svcName string, svc composetypes.ServiceConfig) (ResolvedRuntime, error)
}

// ResolvedImage identifies the exact local image selected for a run.
// MountSource is either ID or the immutable compact prefix required by affected
// Engine versions; it is never the mutable tag used to locate the image.
type ResolvedImage struct {
	ID          string
	MountSource string
}

// ResolvedRuntime is resolved service runtime metadata from compose + image.
type ResolvedRuntime struct {
	// ImageID is the exact service image inspected for all inherited runtime
	// metadata. The generated Compose override executes this identity directly.
	ImageID    string
	Entrypoint []string
	Command    []string
	// Healthcheck is the effective image/Compose healthcheck test vector. It is
	// wrapped through vaka-init's exec trampoline by the generated override.
	Healthcheck []string
	// HealthcheckShell is the image's SHELL vector used by CMD-SHELL probes.
	HealthcheckShell []string
	// ImageUser is the image config USER value. Compose `service.user` is
	// intentionally not folded into this field so callers can apply explicit
	// precedence rules (compose user first, image fallback second).
	ImageUser string
}

// dockerClient is a narrow interface over the Docker API operations used by
// dockerServices. *client.Client satisfies it; tests inject a stub.
type dockerClient interface {
	ServerVersion(ctx context.Context) (dockertypes.Version, error)
	ClientVersion() string
	ImageInspect(ctx context.Context, ref string, opts ...client.ImageInspectOption) (dockerimage.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, opts dockerimage.PullOptions) (io.ReadCloser, error)
	ContainerCreate(ctx context.Context, config *containertypes.Config, hostConfig *containertypes.HostConfig, networkingConfig *networktypes.NetworkingConfig, platform *ocispec.Platform, containerName string) (containertypes.CreateResponse, error)
	ContainerInspect(ctx context.Context, containerID string) (containertypes.InspectResponse, error)
	ContainerStatPath(ctx context.Context, containerID, path string) (containertypes.PathStat, error)
	ContainerRemove(ctx context.Context, containerID string, options containertypes.RemoveOptions) error
}

// dockerServices is the production DockerServices backed by the Docker API.
// The API client is initialized through docker/cli flag/env/config resolution
// so it targets the same backend Docker CLI would use for this invocation.
type dockerServices struct {
	c                          dockerClient
	legacy                     legacyRuntimeClient
	targetDesc                 string
	useCompactImageMountSource bool
	// cfg is the resolved Docker config file; used to attach registry
	// credentials (X-Registry-Auth) to pulls so private images work.
	cfg *configfile.ConfigFile
	// pullPolicy governs whether ResolveRuntime fetches a missing *service*
	// image. The zero value (PullNever) preserves the original inspect-only
	// behavior; the production constructor sets it from --vaka-pull.
	pullPolicy PullPolicy
}

var loadDockerConfigFile = dockerconfig.LoadDefaultConfigFile

var queryComposeVersion = func(ctx context.Context) (string, error) {
	c := exec.CommandContext(ctx, "docker", "compose", "version", "--short")
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker compose version: %s", firstNonEmpty(string(out), err.Error()))
	}
	return strings.TrimSpace(string(out)), nil
}

// NewDockerServices creates a DockerServices for one vaka invocation using
// docker/cli target resolution semantics:
//
//  1. DOCKER_HOST
//  2. DOCKER_CONTEXT
//  3. currentContext from Docker config (DOCKER_CONFIG/config.json)
//  4. default context
func NewDockerServices(inv *ComposeInvocation, pullPolicy PullPolicy) (DockerServices, error) {
	cfg := loadDockerConfigFile(os.Stderr)
	opts := newDockerClientOptions(inv)
	targetDesc := dockerTargetDescription(cfg)

	apiClient, err := dockercli.NewAPIClientFromFlags(opts, cfg)
	if err != nil {
		return nil, fmt.Errorf("create Docker client for %s: %w", targetDesc, err)
	}
	return &dockerServices{
		c:          apiClient,
		legacy:     apiClient,
		targetDesc: targetDesc,
		cfg:        cfg,
		pullPolicy: pullPolicy,
	}, nil
}

func newDockerClientOptions(_ *ComposeInvocation) *dockerflags.ClientOptions {
	opts := dockerflags.NewClientOptions()
	fs := pflag.NewFlagSet("vaka-docker", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts.InstallFlags(fs)
	opts.SetDefaultOptions(fs)
	return opts
}

func dockerConfigFilePathHint() string {
	return filepath.Join(dockerconfig.Dir(), dockerconfig.ConfigFileName)
}

func dockerTargetDescription(cfg *configfile.ConfigFile) string {
	if host := strings.TrimSpace(os.Getenv(client.EnvOverrideHost)); host != "" {
		return fmt.Sprintf("daemon %q (from %s)", host, client.EnvOverrideHost)
	}
	if ctxName := strings.TrimSpace(os.Getenv(dockercli.EnvOverrideContext)); ctxName != "" {
		return fmt.Sprintf("context %q (from %s)", ctxName, dockercli.EnvOverrideContext)
	}
	if cfg != nil && strings.TrimSpace(cfg.CurrentContext) != "" {
		return fmt.Sprintf("context %q (from %s)", strings.TrimSpace(cfg.CurrentContext), dockerConfigFilePathHint())
	}
	return "default Docker context"
}

func (d *dockerServices) CheckRuntimeCompatibility(ctx context.Context) error {
	server, err := d.c.ServerVersion(ctx)
	if err != nil {
		return fmt.Errorf("query Docker Engine version on %s: %w", d.targetDesc, err)
	}
	if err := checkDockerCompatibility(server.Version, server.APIVersion); err != nil {
		return fmt.Errorf("Docker target %s: %w", d.targetDesc, err)
	}
	if err := checkDockerClientCompatibility(d.c.ClientVersion()); err != nil {
		return fmt.Errorf("Docker target %s: %w", d.targetDesc, err)
	}
	composeVersion, err := queryComposeVersion(ctx)
	if err != nil {
		return err
	}
	if err := checkComposeCompatibility(composeVersion); err != nil {
		return err
	}
	useCompactSource, err := resolveImageMountVersionCompatibility(server.Version, composeVersion)
	if err != nil {
		return fmt.Errorf("Docker target %s: %w", d.targetDesc, err)
	}
	d.useCompactImageMountSource = useCompactSource
	return nil
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

// ResolveRuntimeImage resolves and validates Vaka's own runtime image. ref is
// the platform-neutral runtime tag embedded in the CLI. Vaka derives the
// matching immutable architecture tag from the selected Docker daemon, never
// from the host running the CLI. Callers receive the immutable local image ID,
// and the image's bundle-version label must match exactly.
func (d *dockerServices) ResolveRuntimeImage(ctx context.Context, ref, expectedVersion string, repair bool) (ResolvedImage, error) {
	ref, err := d.runtimeImageReference(ctx, ref)
	if err != nil {
		return ResolvedImage{}, err
	}

	inspect, err := d.c.ImageInspect(ctx, ref)
	if errdefs.IsNotFound(err) && repair {
		if err := d.pullImage(ctx, ref); err != nil {
			return ResolvedImage{}, fmt.Errorf("pull runtime image %s on %s: %w", ref, d.targetDesc, err)
		}
		inspect, err = d.c.ImageInspect(ctx, ref)
	}
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ResolvedImage{}, fmt.Errorf("runtime image %s is not present on %s", ref, d.targetDesc)
		}
		return ResolvedImage{}, fmt.Errorf("inspect runtime image %s on %s: %w", ref, d.targetDesc, err)
	}

	resolved, validationErr := validateRuntimeImage(ref, expectedVersion, inspect)
	if validationErr == nil {
		validationErr = d.validateRuntimeMountIdentity(ctx, ref, resolved.ID)
	}
	if validationErr == nil {
		resolved = d.selectRuntimeMountSource(resolved)
	}
	if validationErr == nil || !repair {
		return resolved, validationErr
	}

	// A present but incompatible local tag can be repaired by refreshing the
	// Vaka-owned immutable tag once. Service images never use this path.
	if err := d.pullImage(ctx, ref); err != nil {
		return ResolvedImage{}, fmt.Errorf("refresh incompatible runtime image %s on %s: %w", ref, d.targetDesc, err)
	}
	inspect, err = d.c.ImageInspect(ctx, ref)
	if err != nil {
		return ResolvedImage{}, fmt.Errorf("inspect refreshed runtime image %s on %s: %w", ref, d.targetDesc, err)
	}
	resolved, validationErr = validateRuntimeImage(ref, expectedVersion, inspect)
	if validationErr != nil {
		return ResolvedImage{}, fmt.Errorf("runtime image %s remains incompatible after refresh: %w", ref, validationErr)
	}
	if err := d.validateRuntimeMountIdentity(ctx, ref, resolved.ID); err != nil {
		return ResolvedImage{}, fmt.Errorf("runtime image %s remains incompatible after refresh: %w", ref, err)
	}
	return d.selectRuntimeMountSource(resolved), nil
}

func (d *dockerServices) runtimeImageReference(ctx context.Context, ref string) (string, error) {
	server, err := d.c.ServerVersion(ctx)
	if err != nil {
		return "", fmt.Errorf("query Docker target architecture on %s: %w", d.targetDesc, err)
	}
	arch, err := normalizeRuntimeArchitecture(server.Arch)
	if err != nil {
		return "", fmt.Errorf("Docker target %s: %w", d.targetDesc, err)
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("parse runtime image reference %q: %w", ref, err)
	}
	tagged, ok := named.(reference.Tagged)
	if !ok {
		return "", fmt.Errorf("runtime image reference %q has no version tag", ref)
	}
	archRef, err := reference.WithTag(reference.TrimNamed(named), tagged.Tag()+"-"+arch)
	if err != nil {
		return "", fmt.Errorf("build %s runtime image reference from %q: %w", arch, ref, err)
	}
	return reference.FamiliarString(archRef), nil
}

func normalizeRuntimeArchitecture(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "amd64", "x86_64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("architecture %q has no published Vaka runtime image; supported architectures: amd64, arm64", raw)
	}
}

// validateRuntimeMountIdentity ensures the immutable identity passed through
// Compose is also a directly resolvable Docker image. Containerd-backed stores
// can expose child manifest digests that exist as content but not as image
// records; image mounts cannot use those content-only digests.
func (d *dockerServices) validateRuntimeMountIdentity(ctx context.Context, ref, id string) error {
	inspect, err := d.c.ImageInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("runtime image %s returned mount identity %s that is not directly inspectable on %s: %w", ref, id, d.targetDesc, err)
	}
	if inspect.ID != id {
		return fmt.Errorf("runtime image %s mount identity %s resolves to different image %s on %s", ref, id, inspect.ID, d.targetDesc)
	}
	return nil
}

func (d *dockerServices) selectRuntimeMountSource(resolved ResolvedImage) ResolvedImage {
	resolved.MountSource = resolved.ID
	if d.useCompactImageMountSource {
		resolved.MountSource = strings.TrimPrefix(resolved.ID, "sha256:")[:40]
	}
	return resolved
}

func validateRuntimeImage(ref, expectedVersion string, inspect dockerimage.InspectResponse) (ResolvedImage, error) {
	if !validDockerImageID(inspect.ID) {
		return ResolvedImage{}, fmt.Errorf("runtime image %s returned invalid image ID %q", ref, inspect.ID)
	}
	if inspect.Config == nil {
		return ResolvedImage{}, fmt.Errorf("runtime image %s has no image configuration", ref)
	}
	actualVersion := strings.TrimSpace(inspect.Config.Labels[runtimebundle.VersionLabel])
	if actualVersion != expectedVersion {
		if actualVersion == "" {
			actualVersion = "<missing>"
		}
		return ResolvedImage{}, fmt.Errorf("runtime image %s has bundle version %s, require %s", ref, actualVersion, expectedVersion)
	}
	return ResolvedImage{ID: inspect.ID}, nil
}

func validDockerImageID(id string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, prefix))
	return err == nil
}

// pullImage pulls ref and streams progress to stderr. The returned error is
// unwrapped so callers can attach context appropriate to why the pull ran.
// Registry credentials from the Docker config are attached so private images
// pull the same way they do under `docker pull`/Compose.
func (d *dockerServices) pullImage(ctx context.Context, ref string) error {
	rc, err := d.c.ImagePull(ctx, ref, d.pullOptions(ref))
	if err != nil {
		return err
	}
	defer rc.Close()
	stderrFD, isTerminal := term.GetFdInfo(os.Stderr)
	if err := jsonmessage.DisplayJSONMessagesStream(rc, os.Stderr, stderrFD, isTerminal, nil); err != nil {
		return fmt.Errorf("display pull stream for %s on %s: %w", ref, d.targetDesc, err)
	}
	return nil
}

// pullOptions builds PullOptions for ref, attaching an X-Registry-Auth token
// resolved from the Docker config's credential store for ref's registry. When
// no credentials are configured (public registry), RegistryAuth is left empty
// so the pull proceeds anonymously — identical to the prior behavior.
func (d *dockerServices) pullOptions(ref string) dockerimage.PullOptions {
	opts := dockerimage.PullOptions{}
	if d.cfg == nil {
		return opts
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return opts
	}
	repoInfo, err := registry.ParseRepositoryInfo(named)
	if err != nil {
		return opts
	}
	authConfig := dockercli.ResolveAuthConfig(d.cfg, repoInfo.Index)
	if authConfig.Username == "" && authConfig.Password == "" &&
		authConfig.IdentityToken == "" && authConfig.RegistryToken == "" {
		return opts // no credentials for this registry — pull anonymously
	}
	if encoded, err := registrytypes.EncodeAuthConfig(authConfig); err == nil {
		opts.RegistryAuth = encoded
	}
	return opts
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
	if svc.HealthCheck != nil && !svc.HealthCheck.Disable && len(svc.HealthCheck.Test) > 0 {
		resolved.Healthcheck = append([]string{}, svc.HealthCheck.Test...)
	}

	// A nil Compose value inherits the image field. An explicitly empty value
	// clears it and must remain empty; this distinction also matters when
	// `compose run SERVICE COMMAND` replaces only Command in the final model.
	needImageEntrypoint := svc.Entrypoint == nil
	needImageUser := strings.TrimSpace(svc.User) == ""
	needImageHealthcheck := svc.HealthCheck == nil || (!svc.HealthCheck.Disable && len(svc.HealthCheck.Test) == 0)
	needImageHealthcheckShell := len(resolved.Healthcheck) > 0 && resolved.Healthcheck[0] == "CMD-SHELL"

	if svc.Image == "" {
		return ResolvedRuntime{}, fmt.Errorf(
			"service %s: cannot inspect the exact service image without image: (needed for %s and protected image-volume validation)",
			svcName, missingRuntimeFieldsHint(needImageEntrypoint, needImageUser, needImageHealthcheck),
		)
	}
	// A locally-absent image is fetched once through a pull when the policy
	// permits it (--vaka-pull; default missing-pinned pulls only digest-pinned
	// refs). Present images are never re-pulled. The vaka-init helper is never
	// routed here.
	inspect, err := d.c.ImageInspect(ctx, svc.Image)
	// An explicit Compose pull_policy is authoritative. buildInjectionOverride
	// applies it before inspection; --vaka-pull is only the fallback for a
	// service that did not declare a Compose policy.
	if errdefs.IsNotFound(err) && strings.TrimSpace(svc.PullPolicy) == "" && d.pullPolicy.pullsMissing(svc.Image) {
		if perr := d.pullImage(ctx, svc.Image); perr != nil {
			return ResolvedRuntime{}, fmt.Errorf("service %s: pull %q on %s: %w", svcName, svc.Image, d.targetDesc, perr)
		}
		inspect, err = d.c.ImageInspect(ctx, svc.Image)
	}
	if err != nil {
		if errdefs.IsNotFound(err) {
			hint := "pull it first (or run with --vaka-pull=missing)"
			if policy := strings.TrimSpace(svc.PullPolicy); policy != "" {
				hint = fmt.Sprintf("its effective Compose pull policy %q did not make it available; change that policy or prepare the image explicitly", policy)
			}
			return ResolvedRuntime{}, fmt.Errorf(
				"service %s: image %q not available locally on %s — %s, or set compose user/entrypoint so image defaults are not needed",
				svcName, svc.Image, d.targetDesc, hint,
			)
		}
		return ResolvedRuntime{}, fmt.Errorf("service %s: inspect %q on %s: %w", svcName, svc.Image, d.targetDesc, err)
	}
	if inspect.Config == nil {
		return ResolvedRuntime{}, fmt.Errorf("service %s: image %q has no Config", svcName, svc.Image)
	}
	if !validDockerImageID(inspect.ID) {
		return ResolvedRuntime{}, fmt.Errorf("service %s: image %q returned invalid image ID %q", svcName, svc.Image, inspect.ID)
	}
	for volumePath := range inspect.Config.Volumes {
		if protectedPathOverlap(volumePath) {
			return ResolvedRuntime{}, fmt.Errorf("service %s: image %q declares VOLUME %q overlapping Vaka's protected runtime or policy mount", svcName, svc.Image, volumePath)
		}
	}
	if err := d.validateServiceImageMountPaths(ctx, svcName, inspect.ID, svc, inspect.Config.Volumes); err != nil {
		return ResolvedRuntime{}, err
	}
	resolved.ImageID = inspect.ID

	if needImageEntrypoint {
		resolved.Entrypoint = inspect.Config.Entrypoint
		if svc.Command == nil {
			resolved.Command = inspect.Config.Cmd
		}
	}
	if needImageUser {
		resolved.ImageUser = inspect.Config.User
	}
	if needImageHealthcheck && inspect.Config.Healthcheck != nil {
		resolved.Healthcheck = append([]string{}, inspect.Config.Healthcheck.Test...)
	}
	if needImageHealthcheckShell || (needImageHealthcheck && len(resolved.Healthcheck) > 0 && resolved.Healthcheck[0] == "CMD-SHELL") {
		resolved.HealthcheckShell = append([]string{}, inspect.Config.Shell...)
	}
	return resolved, nil
}

func missingRuntimeFieldsHint(needImageEntrypoint, needImageUser, needImageHealthcheck bool) string {
	switch {
	case needImageEntrypoint && needImageUser && needImageHealthcheck:
		return "entrypoint/cmd, user, and healthcheck fallback"
	case needImageEntrypoint && needImageUser:
		return "entrypoint/cmd and user fallback"
	case needImageEntrypoint && needImageHealthcheck:
		return "entrypoint/cmd and healthcheck fallback"
	case needImageUser && needImageHealthcheck:
		return "user and healthcheck fallback"
	case needImageEntrypoint:
		return "entrypoint/cmd fallback"
	case needImageUser:
		return "user fallback"
	case needImageHealthcheck:
		return "healthcheck fallback"
	default:
		return "runtime fallback"
	}
}
