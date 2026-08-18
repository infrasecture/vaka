package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
	"vaka.dev/vaka/internal/runtimebundle"
)

const (
	serviceImageProbeLabel     = "agent.vaka.rootfs-probe"
	maxImageSymlinkEvaluations = 255
)

type mountInstallPhase uint8

const (
	// Docker's OCI defaults are installed before configured container mounts.
	mountPhaseDefault mountInstallPhase = iota
	// Docker sorts configured mounts by destination depth, parents first.
	mountPhaseUser
	// Devices and masked/read-only paths are applied after configured mounts.
	mountPhasePostUser
)

// Docker supplies these mounts independently from the Compose service model.
// Resolve their image-side destinations too: runc follows pre-existing image
// symlinks for ordinary mount targets. Their contents are Docker-controlled,
// so ordinary nesting such as /dev followed by /dev/pts is safe.
var dockerDefaultMountTargets = []string{
	"/proc",
	"/dev",
	"/dev/pts",
	"/sys",
	"/sys/fs/cgroup",
	"/dev/mqueue",
	"/dev/shm",
}

// These daemon-generated bind mounts are sorted with configured mounts.
var dockerNetworkMountTargets = []string{
	"/etc/hostname",
	"/etc/hosts",
	"/etc/resolv.conf",
}

// The runtime creates the standard device nodes after setting up mounts. A
// user-controlled /dev mount must therefore not be able to redirect them.
var dockerDefaultDeviceTargets = []string{
	"/dev/console",
	"/dev/full",
	"/dev/null",
	"/dev/ptmx",
	"/dev/random",
	"/dev/tty",
	"/dev/urandom",
	"/dev/zero",
}

type imageMountTarget struct {
	path               string
	origin             string
	phase              mountInstallPhase
	maySupplySymlinks  bool
	allowRuntimeTarget bool
}

type resolvedImageMountTarget struct {
	imageMountTarget
	resolved string
}

// validateServiceImageMountPaths inspects the exact pinned service-image
// rootfs without executing it. Docker/runc resolves existing symlinks in mount
// destinations, and later targets are resolved against earlier mounts. Both
// the pristine image and the complete mount topology must therefore be safe.
func (d *dockerServices) validateServiceImageMountPaths(
	ctx context.Context,
	serviceName, imageID string,
	svc composetypes.ServiceConfig,
	imageVolumes map[string]struct{},
) error {
	targets := serviceImageMountTargets(svc, imageVolumes)
	return d.validateImageMountPaths(ctx, "service "+serviceName, imageID, svc.Privileged, true, targets)
}

func (d *dockerServices) validateImageMountPaths(
	ctx context.Context,
	subject, imageID string,
	privileged bool,
	includeProbeSecurityPaths bool,
	targets []imageMountTarget,
) error {
	probe, err := d.c.ContainerCreate(ctx, &containertypes.Config{
		Image:      imageID,
		Entrypoint: []string{"/"},
		WorkingDir: "/",
		Labels:     map[string]string{serviceImageProbeLabel: "true"},
	}, &containertypes.HostConfig{
		NetworkMode: "none",
		Privileged:  privileged,
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("%s: create temporary rootfs probe for image %s on %s: %w", subject, imageID, d.targetDesc, err)
	}
	if strings.TrimSpace(probe.ID) == "" {
		return fmt.Errorf("%s: Docker returned no container ID for temporary rootfs probe", subject)
	}

	validationErr := d.inspectImageMountPaths(ctx, subject, probe.ID, includeProbeSecurityPaths, targets)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cleanupErr := d.c.ContainerRemove(cleanupCtx, probe.ID, containertypes.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	cancel()
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("remove temporary rootfs probe %s for %s: %w", probe.ID, subject, cleanupErr)
	}
	return errors.Join(validationErr, cleanupErr)
}

func (d *dockerServices) inspectImageMountPaths(
	ctx context.Context,
	subject, containerID string,
	includeProbeSecurityPaths bool,
	targets []imageMountTarget,
) error {
	inspect, err := d.c.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("%s: inspect temporary rootfs probe %s: %w", subject, containerID, err)
	}
	if inspect.ID != containerID {
		return fmt.Errorf("%s: inspect temporary rootfs probe %s returned different identity %s", subject, containerID, inspect.ID)
	}
	if inspect.HostConfig == nil {
		return fmt.Errorf("%s: temporary rootfs probe %s has no HostConfig metadata", subject, containerID)
	}
	if includeProbeSecurityPaths {
		for _, target := range inspect.HostConfig.MaskedPaths {
			targets = appendImageMountTarget(targets, "Docker masked path", target, mountPhasePostUser, false, false)
		}
		for _, target := range inspect.HostConfig.ReadonlyPaths {
			targets = appendImageMountTarget(targets, "Docker read-only path", target, mountPhasePostUser, false, false)
		}
	}

	resolver := imagePathResolver{
		ctx:         ctx,
		client:      d.c,
		containerID: containerID,
		cache:       make(map[string]cachedPathStat),
	}

	stat, err := resolver.lstat(runtimebundle.MountPath)
	switch {
	case err == nil && stat.Mode&os.ModeSymlink != 0:
		return fmt.Errorf("%s: image path %s is a symbolic link; Vaka requires the runtime target to be absent or a real directory", subject, runtimebundle.MountPath)
	case err == nil && !stat.Mode.IsDir():
		return fmt.Errorf("%s: image path %s is not a directory; Vaka requires the runtime target to be absent or a real directory", subject, runtimebundle.MountPath)
	case err != nil && !errdefs.IsNotFound(err):
		return fmt.Errorf("%s: inspect image path %s: %w", subject, runtimebundle.MountPath, err)
	}

	resolvedTargets := make([]resolvedImageMountTarget, 0, len(targets))
	for _, target := range targets {
		resolved, err := resolver.resolve(target.path)
		if err != nil {
			return fmt.Errorf("%s: resolve image path for %s %q: %w", subject, target.origin, target.path, err)
		}
		if protectedPathOverlap(resolved) && !validAllowedRuntimeTarget(target, resolved) {
			return fmt.Errorf("%s: %s %q resolves through the service image to protected path %q", subject, target.origin, target.path, resolved)
		}
		resolvedTargets = append(resolvedTargets, resolvedImageMountTarget{
			imageMountTarget: target,
			resolved:         resolved,
		})
	}

	sort.Slice(resolvedTargets, func(i, j int) bool {
		if resolvedTargets[i].resolved != resolvedTargets[j].resolved {
			return resolvedTargets[i].resolved < resolvedTargets[j].resolved
		}
		if resolvedTargets[i].phase != resolvedTargets[j].phase {
			return resolvedTargets[i].phase < resolvedTargets[j].phase
		}
		return resolvedTargets[i].origin < resolvedTargets[j].origin
	})
	for i := range resolvedTargets {
		ancestor := resolvedTargets[i]
		if !ancestor.maySupplySymlinks {
			continue
		}
		for j := range resolvedTargets {
			later := resolvedTargets[j]
			if !mountMayInstallBefore(ancestor.imageMountTarget, later.imageMountTarget) || !strictPathContains(ancestor.resolved, later.resolved) {
				continue
			}
			return fmt.Errorf(
				"%s: %s %q is an externally populated ancestor of later %s %q; Docker can resolve the nested target through mounted content and into Vaka's protected runtime",
				subject, ancestor.origin, ancestor.path, later.origin, later.path,
			)
		}
	}
	return nil
}

func mountMayInstallBefore(earlier, later imageMountTarget) bool {
	if earlier.phase != later.phase {
		return earlier.phase < later.phase
	}
	if earlier.phase != mountPhaseUser {
		return true
	}
	// Docker orders configured mounts by the component count of their lexical
	// destinations before runc resolves any symlinks. Equal-depth ordering is
	// unspecified, so it cannot be relied upon as a security boundary.
	return mountTargetDepth(earlier.path) <= mountTargetDepth(later.path)
}

func mountTargetDepth(target string) int {
	cleaned := path.Clean(target)
	if cleaned == "/" {
		return 0
	}
	return len(pathComponents(cleaned))
}

func validAllowedRuntimeTarget(target imageMountTarget, resolved string) bool {
	return target.allowRuntimeTarget && path.Clean(target.path) == protectedRuntimePath && resolved == protectedRuntimePath
}

func strictPathContains(parent, child string) bool {
	return parent != child && pathContains(parent, child)
}

func appendImageMountTarget(
	targets []imageMountTarget,
	origin, target string,
	phase mountInstallPhase,
	maySupplySymlinks, allowRuntimeTarget bool,
) []imageMountTarget {
	// Whitespace is valid in Linux pathnames. Never normalize it away: Docker
	// and Compose preserve it, so changing it would validate a different path.
	if target == "" {
		return targets
	}
	return append(targets, imageMountTarget{
		path:               target,
		origin:             origin,
		phase:              phase,
		maySupplySymlinks:  maySupplySymlinks,
		allowRuntimeTarget: allowRuntimeTarget,
	})
}

func serviceImageMountTargets(svc composetypes.ServiceConfig, imageVolumes map[string]struct{}) []imageMountTarget {
	var targets []imageMountTarget
	for _, target := range dockerDefaultMountTargets {
		targets = appendImageMountTarget(targets, "Docker implicit mount target", target, mountPhaseDefault, false, false)
	}
	if svc.Init != nil && *svc.Init {
		targets = appendImageMountTarget(targets, "Docker init mount target", "/sbin/docker-init", mountPhaseDefault, false, false)
	}
	for _, target := range dockerNetworkMountTargets {
		targets = appendImageMountTarget(targets, "Docker implicit mount target", target, mountPhaseUser, false, false)
	}
	if svc.UseAPISocket {
		targets = appendImageMountTarget(targets, "Docker API socket target", "/var/run/docker.sock", mountPhaseUser, false, false)
	}
	for _, volume := range svc.Volumes {
		targets = appendImageMountTarget(targets, "volume target", volume.Target, mountPhaseUser, serviceVolumeMaySupplySymlinks(volume), false)
	}
	for _, config := range svc.Configs {
		target := config.Target
		if target == "" {
			target = "/" + config.Source
		}
		targets = appendImageMountTarget(targets, "config target", target, mountPhaseUser, false, false)
	}
	for _, secret := range svc.Secrets {
		target := secret.Target
		if target == "" {
			target = "/run/secrets/" + secret.Source
		}
		targets = appendImageMountTarget(targets, "secret target", target, mountPhaseUser, false, false)
	}
	for _, tmpfs := range svc.Tmpfs {
		target, _, _ := strings.Cut(tmpfs, ":")
		targets = appendImageMountTarget(targets, "tmpfs target", target, mountPhaseUser, false, false)
	}
	for _, device := range svc.Devices {
		targets = appendImageMountTarget(targets, "device target", device.Target, mountPhasePostUser, false, false)
	}
	for target := range imageVolumes {
		targets = appendImageMountTarget(targets, "image VOLUME", target, mountPhaseUser, true, false)
	}
	for _, target := range dockerDefaultDeviceTargets {
		targets = appendImageMountTarget(targets, "Docker implicit device target", target, mountPhasePostUser, false, false)
	}
	targets = appendImageMountTarget(targets, "Vaka runtime mount target", runtimebundle.MountPath, mountPhaseUser, false, true)

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].path == targets[j].path {
			return targets[i].origin < targets[j].origin
		}
		return targets[i].path < targets[j].path
	})
	return targets
}

func serviceVolumeMaySupplySymlinks(volume composetypes.ServiceVolumeConfig) bool {
	switch volume.Type {
	case composetypes.VolumeTypeTmpfs, composetypes.VolumeTypeNamedPipe:
		return false
	default:
		// Bind, named-volume, image, cluster, and future external mount types
		// can contain attacker-controlled symlinks before a container starts.
		return true
	}
}

func liveContainerImageMountTargets(inspect containertypes.InspectResponse) []imageMountTarget {
	var targets []imageMountTarget
	for _, target := range dockerDefaultMountTargets {
		targets = appendImageMountTarget(targets, "Docker implicit mount target", target, mountPhaseDefault, false, false)
	}
	if inspect.HostConfig != nil && inspect.HostConfig.Init != nil && *inspect.HostConfig.Init && inspect.HostConfig.PidMode.IsPrivate() {
		targets = appendImageMountTarget(targets, "Docker init mount target", "/sbin/docker-init", mountPhaseDefault, false, false)
	}
	for _, target := range dockerNetworkMountTargets {
		targets = appendImageMountTarget(targets, "Docker implicit mount target", target, mountPhaseUser, false, false)
	}
	for _, mounted := range inspect.Mounts {
		if path.Clean(mounted.Destination) == protectedRuntimePath {
			continue
		}
		targets = appendImageMountTarget(
			targets,
			fmt.Sprintf("live %s mount target", mounted.Type),
			mounted.Destination,
			mountPhaseUser,
			liveMountMaySupplySymlinks(mounted.Type),
			false,
		)
	}
	if inspect.HostConfig != nil {
		for _, configured := range inspect.HostConfig.Mounts {
			if path.Clean(configured.Target) == protectedRuntimePath {
				continue
			}
			targets = appendImageMountTarget(
				targets,
				fmt.Sprintf("configured %s mount target", configured.Type),
				configured.Target,
				mountPhaseUser,
				liveMountMaySupplySymlinks(configured.Type),
				false,
			)
		}
		for target := range inspect.HostConfig.Tmpfs {
			targets = appendImageMountTarget(targets, "configured tmpfs target", target, mountPhaseUser, false, false)
		}
		for _, device := range inspect.HostConfig.Devices {
			targets = appendImageMountTarget(targets, "configured device target", device.PathInContainer, mountPhasePostUser, false, false)
		}
		for _, target := range inspect.HostConfig.MaskedPaths {
			targets = appendImageMountTarget(targets, "Docker masked path", target, mountPhasePostUser, false, false)
		}
		for _, target := range inspect.HostConfig.ReadonlyPaths {
			targets = appendImageMountTarget(targets, "Docker read-only path", target, mountPhasePostUser, false, false)
		}
	}
	for _, target := range dockerDefaultDeviceTargets {
		targets = appendImageMountTarget(targets, "Docker implicit device target", target, mountPhasePostUser, false, false)
	}
	targets = appendImageMountTarget(targets, "Vaka runtime mount target", runtimebundle.MountPath, mountPhaseUser, false, true)
	return targets
}

func liveMountMaySupplySymlinks(mountType mount.Type) bool {
	switch mountType {
	case mount.TypeTmpfs, mount.TypeNamedPipe:
		return false
	default:
		return true
	}
}

type containerPathStatClient interface {
	ContainerStatPath(context.Context, string, string) (containertypes.PathStat, error)
}

type cachedPathStat struct {
	stat containertypes.PathStat
	err  error
}

type imagePathResolver struct {
	ctx         context.Context
	client      containerPathStatClient
	containerID string
	cache       map[string]cachedPathStat
}

func (r *imagePathResolver) lstat(candidate string) (containertypes.PathStat, error) {
	candidate = path.Clean(candidate)
	if cached, ok := r.cache[candidate]; ok {
		return cached.stat, cached.err
	}
	stat, err := r.client.ContainerStatPath(r.ctx, r.containerID, candidate)
	r.cache[candidate] = cachedPathStat{stat: stat, err: err}
	return stat, err
}

func (r *imagePathResolver) resolve(candidate string) (string, error) {
	// path.Clean normalizes separators and dot components in the same pathname;
	// unlike TrimSpace, it does not change valid leading/trailing whitespace.
	candidate = path.Clean(candidate)
	if candidate == "." || !path.IsAbs(candidate) {
		return "", fmt.Errorf("mount target must be an absolute path, got %q", candidate)
	}
	if candidate == "/" {
		return candidate, nil
	}

	pending := pathComponents(candidate)
	resolved := "/"
	symlinks := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		current := path.Join(resolved, component)
		stat, err := r.lstat(current)
		if errdefs.IsNotFound(err) {
			return path.Join(append([]string{current}, pending...)...), nil
		}
		if err != nil {
			return "", err
		}
		if stat.Mode&os.ModeSymlink == 0 {
			resolved = current
			continue
		}

		symlinks++
		if symlinks > maxImageSymlinkEvaluations {
			return "", fmt.Errorf("too many symbolic links while resolving %q", candidate)
		}
		// Docker reports a fully evaluated target, but preserve and re-evaluate
		// it component-by-component so this remains correct for alternate
		// clients, broken links, and symlink chains.
		linkTarget := stat.LinkTarget
		if linkTarget == "" {
			return "", fmt.Errorf("Docker reported symbolic link %q without a resolved target", current)
		}
		if path.IsAbs(linkTarget) {
			linkTarget = path.Clean(linkTarget)
		} else {
			linkTarget = path.Clean(path.Join(path.Dir(current), linkTarget))
		}
		if !path.IsAbs(linkTarget) {
			return "", fmt.Errorf("symbolic link %q resolved outside the container root to %q", current, linkTarget)
		}
		pending = append(pathComponents(linkTarget), pending...)
		resolved = "/"
	}
	return path.Clean(resolved), nil
}

func pathComponents(candidate string) []string {
	cleaned := path.Clean(candidate)
	if cleaned == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
}
