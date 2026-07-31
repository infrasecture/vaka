package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
)

const (
	composeProjectLabel        = "com.docker.compose.project"
	composeServiceLabel        = "com.docker.compose.service"
	legacyHelperService        = "__vaka-init"
	legacyRuntimePath          = "/opt/vaka"
	dockerAnonymousVolumeLabel = "com.docker.volume.anonymous"
)

var anonymousVolumeName = regexp.MustCompile(`^[a-f0-9]{64}$`)

type legacyRuntimeClient interface {
	ContainerList(ctx context.Context, options containertypes.ListOptions) ([]containertypes.Summary, error)
	ContainerRemove(ctx context.Context, containerID string, options containertypes.RemoveOptions) error
	VolumeInspect(ctx context.Context, volumeID string) (volumetypes.Volume, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
}

type legacyRuntimeMigrator interface {
	CaptureLegacyRuntime(context.Context, string) (legacyRuntimeState, error)
	CleanupLegacyRuntime(context.Context, legacyRuntimeState) (legacyCleanupResult, error)
}

type legacyRuntimeState struct {
	Project string
	Volumes map[string]map[string]bool // volume name -> helper container IDs
}

func (s legacyRuntimeState) Empty() bool {
	return len(s.Volumes) == 0
}

type legacyCleanupResult struct {
	RemovedVolumes  int
	DeferredVolumes int
}

func (d *dockerServices) CaptureLegacyRuntime(ctx context.Context, project string) (legacyRuntimeState, error) {
	state := legacyRuntimeState{Project: project, Volumes: make(map[string]map[string]bool)}
	if strings.TrimSpace(project) == "" || d.legacy == nil {
		return state, nil
	}
	containers, err := d.legacy.ContainerList(ctx, containertypes.ListOptions{
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", composeProjectLabel+"="+project),
			filters.Arg("label", composeServiceLabel+"="+legacyHelperService),
		),
	})
	if err != nil {
		return state, fmt.Errorf("find legacy vaka runtime for project %s on %s: %w", project, d.targetDesc, err)
	}
	for _, ctr := range containers {
		if ctr.Labels[composeProjectLabel] != project || ctr.Labels[composeServiceLabel] != legacyHelperService {
			continue
		}
		if !strings.HasPrefix(ctr.Image, vakaInitBaseImage+":") {
			continue
		}
		for _, mounted := range ctr.Mounts {
			if mounted.Type != mount.TypeVolume || mounted.Destination != legacyRuntimePath || !anonymousVolumeName.MatchString(mounted.Name) {
				continue
			}
			volume, err := d.legacy.VolumeInspect(ctx, mounted.Name)
			if err != nil {
				if errdefs.IsNotFound(err) {
					continue
				}
				return state, fmt.Errorf("inspect candidate legacy volume %s on %s: %w", mounted.Name, d.targetDesc, err)
			}
			if _, anonymous := volume.Labels[dockerAnonymousVolumeLabel]; !anonymous {
				continue
			}
			if state.Volumes[mounted.Name] == nil {
				state.Volumes[mounted.Name] = make(map[string]bool)
			}
			state.Volumes[mounted.Name][ctr.ID] = true
		}
	}
	return state, nil
}

func (d *dockerServices) CleanupLegacyRuntime(ctx context.Context, state legacyRuntimeState) (legacyCleanupResult, error) {
	var result legacyCleanupResult
	if state.Empty() || d.legacy == nil {
		return result, nil
	}

	volumeNames := make([]string, 0, len(state.Volumes))
	for name := range state.Volumes {
		volumeNames = append(volumeNames, name)
	}
	sort.Strings(volumeNames)

	var cleanupErrs []error
	for _, volumeName := range volumeNames {
		helperIDs := state.Volumes[volumeName]
		consumers, err := d.legacy.ContainerList(ctx, containertypes.ListOptions{
			All:     true,
			Filters: filters.NewArgs(filters.Arg("volume", volumeName)),
		})
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("list consumers of legacy volume %s: %w", volumeName, err))
			continue
		}
		inUse := false
		for _, ctr := range consumers {
			if !helperIDs[ctr.ID] {
				inUse = true
				break
			}
		}
		if inUse {
			result.DeferredVolumes++
			continue
		}

		for helperID := range helperIDs {
			err := d.legacy.ContainerRemove(ctx, helperID, containertypes.RemoveOptions{})
			if err != nil && !errdefs.IsNotFound(err) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("remove legacy helper container %.12s: %w", helperID, err))
				inUse = true
			}
		}
		if inUse {
			result.DeferredVolumes++
			continue
		}

		if err := d.legacy.VolumeRemove(ctx, volumeName, false); err != nil {
			if errdefs.IsNotFound(err) {
				result.RemovedVolumes++
				continue
			}
			if errdefs.IsConflict(err) {
				result.DeferredVolumes++
				continue
			}
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove legacy runtime volume %s: %w", volumeName, err))
			continue
		}
		result.RemovedVolumes++
	}
	return result, errors.Join(cleanupErrs...)
}

func captureLegacyRuntime(ctx context.Context, ds DockerServices, project string) (legacyRuntimeState, error) {
	migrator, ok := ds.(legacyRuntimeMigrator)
	if !ok {
		return legacyRuntimeState{}, nil
	}
	return migrator.CaptureLegacyRuntime(ctx, project)
}

func cleanupLegacyRuntime(ctx context.Context, ds DockerServices, state legacyRuntimeState) {
	migrator, ok := ds.(legacyRuntimeMigrator)
	if !ok || state.Empty() {
		return
	}
	result, err := migrator.CleanupLegacyRuntime(ctx, state)
	if result.RemovedVolumes > 0 {
		fmt.Fprintf(os.Stderr, "vaka: migrated %d legacy runtime volume(s)\n", result.RemovedVolumes)
	}
	if result.DeferredVolumes > 0 {
		fmt.Fprintf(os.Stderr, "vaka: retained %d legacy runtime volume(s) still used by existing containers\n", result.DeferredVolumes)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vaka: warning: legacy runtime cleanup incomplete: %v\n", err)
	}
}
