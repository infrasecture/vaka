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
	"github.com/docker/docker/errdefs"
	"vaka.dev/vaka/internal/runtimebundle"
)

const serviceImageProbeLabel = "agent.vaka.rootfs-probe"

// Docker supplies these mounts independently from the Compose service model.
// Resolve their image-side destinations too: runc follows pre-existing image
// symlinks for ordinary mount targets.
var implicitContainerMountTargets = []string{
	"/dev",
	"/dev/mqueue",
	"/dev/pts",
	"/dev/shm",
	"/etc/hostname",
	"/etc/hosts",
	"/etc/resolv.conf",
	"/proc",
	"/sys",
	"/sys/fs/cgroup",
}

type imageMountTarget struct {
	path   string
	origin string
}

// validateServiceImageMountPaths inspects the exact pinned service-image
// rootfs without executing it. Docker/runc resolves existing symlinks in mount
// destinations, so lexical Compose validation alone cannot prove that an
// unrelated-looking target will not land in /vaka.
func (d *dockerServices) validateServiceImageMountPaths(
	ctx context.Context,
	serviceName, imageID string,
	svc composetypes.ServiceConfig,
	imageVolumes map[string]struct{},
) error {
	probe, err := d.c.ContainerCreate(ctx, &containertypes.Config{
		Image:      imageID,
		Entrypoint: []string{"/"},
		WorkingDir: "/",
		Labels:     map[string]string{serviceImageProbeLabel: "true"},
	}, &containertypes.HostConfig{
		NetworkMode: "none",
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("service %s: create temporary rootfs probe for image %s on %s: %w", serviceName, imageID, d.targetDesc, err)
	}
	if strings.TrimSpace(probe.ID) == "" {
		return fmt.Errorf("service %s: Docker returned no container ID for temporary rootfs probe", serviceName)
	}

	validationErr := d.inspectServiceImageMountPaths(ctx, serviceName, probe.ID, svc, imageVolumes)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cleanupErr := d.c.ContainerRemove(cleanupCtx, probe.ID, containertypes.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})
	cancel()
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("remove temporary rootfs probe %s for service %s: %w", probe.ID, serviceName, cleanupErr)
	}
	return errors.Join(validationErr, cleanupErr)
}

func (d *dockerServices) inspectServiceImageMountPaths(
	ctx context.Context,
	serviceName, containerID string,
	svc composetypes.ServiceConfig,
	imageVolumes map[string]struct{},
) error {
	resolver := imagePathResolver{
		ctx:         ctx,
		client:      d.c,
		containerID: containerID,
		cache:       make(map[string]cachedPathStat),
	}

	stat, err := resolver.lstat(runtimebundle.MountPath)
	switch {
	case err == nil && stat.Mode&os.ModeSymlink != 0:
		return fmt.Errorf("service %s: image path %s is a symbolic link; Vaka requires the runtime target to be absent or a real directory", serviceName, runtimebundle.MountPath)
	case err == nil && !stat.Mode.IsDir():
		return fmt.Errorf("service %s: image path %s is not a directory; Vaka requires the runtime target to be absent or a real directory", serviceName, runtimebundle.MountPath)
	case err != nil && !errdefs.IsNotFound(err):
		return fmt.Errorf("service %s: inspect image path %s: %w", serviceName, runtimebundle.MountPath, err)
	}

	for _, target := range serviceImageMountTargets(svc, imageVolumes) {
		resolved, err := resolver.resolve(target.path)
		if err != nil {
			return fmt.Errorf("service %s: resolve image path for %s %q: %w", serviceName, target.origin, target.path, err)
		}
		if protectedPathOverlap(resolved) {
			return fmt.Errorf("service %s: %s %q resolves through the service image to protected path %q", serviceName, target.origin, target.path, resolved)
		}
	}
	return nil
}

func serviceImageMountTargets(svc composetypes.ServiceConfig, imageVolumes map[string]struct{}) []imageMountTarget {
	var targets []imageMountTarget
	add := func(origin, target string) {
		target = strings.TrimSpace(target)
		if target == "" {
			return
		}
		targets = append(targets, imageMountTarget{path: target, origin: origin})
	}

	for _, target := range implicitContainerMountTargets {
		add("Docker implicit mount target", target)
	}
	if svc.UseAPISocket {
		add("Docker API socket target", "/var/run/docker.sock")
	}
	for _, volume := range svc.Volumes {
		add("volume target", volume.Target)
	}
	for _, config := range svc.Configs {
		target := config.Target
		if target == "" {
			target = "/" + config.Source
		}
		add("config target", target)
	}
	for _, secret := range svc.Secrets {
		target := secret.Target
		if target == "" {
			target = "/run/secrets/" + secret.Source
		}
		add("secret target", target)
	}
	for _, tmpfs := range svc.Tmpfs {
		target, _, _ := strings.Cut(tmpfs, ":")
		add("tmpfs target", target)
	}
	for _, device := range svc.Devices {
		add("device target", device.Target)
	}
	for target := range imageVolumes {
		add("image VOLUME", target)
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].path == targets[j].path {
			return targets[i].origin < targets[j].origin
		}
		return targets[i].path < targets[j].path
	})
	return targets
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
	candidate = path.Clean(strings.TrimSpace(candidate))
	if candidate == "." || !path.IsAbs(candidate) {
		return "", fmt.Errorf("mount target must be an absolute path, got %q", candidate)
	}
	if candidate == "/" {
		return candidate, nil
	}

	pending := strings.Split(strings.TrimPrefix(candidate, "/"), "/")
	resolved := "/"
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

		linkTarget := strings.TrimSpace(stat.LinkTarget)
		if linkTarget == "" {
			return "", fmt.Errorf("Docker reported symbolic link %q without a resolved target", current)
		}
		if path.IsAbs(linkTarget) {
			resolved = path.Clean(linkTarget)
		} else {
			resolved = path.Clean(path.Join(path.Dir(current), linkTarget))
		}
	}
	return path.Clean(resolved), nil
}
