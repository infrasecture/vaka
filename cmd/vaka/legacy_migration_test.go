package main

import (
	"context"
	"errors"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
)

const testLegacyVolume = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type fakeLegacyRuntimeClient struct {
	listFn         func(containertypes.ListOptions) ([]containertypes.Summary, error)
	removedHelpers []string
	removedVolumes []string
	containerErr   error
	volumeErr      error
	forceValues    []bool
}

func (f *fakeLegacyRuntimeClient) ContainerList(_ context.Context, options containertypes.ListOptions) ([]containertypes.Summary, error) {
	return f.listFn(options)
}

func (f *fakeLegacyRuntimeClient) ContainerRemove(_ context.Context, id string, _ containertypes.RemoveOptions) error {
	f.removedHelpers = append(f.removedHelpers, id)
	return f.containerErr
}

func (f *fakeLegacyRuntimeClient) VolumeRemove(_ context.Context, name string, force bool) error {
	f.removedVolumes = append(f.removedVolumes, name)
	f.forceValues = append(f.forceValues, force)
	return f.volumeErr
}

func legacyHelper(id, image, volumeName, destination string) containertypes.Summary {
	return containertypes.Summary{
		ID:    id,
		Image: image,
		Labels: map[string]string{
			composeProjectLabel: "demo",
			composeServiceLabel: legacyHelperService,
		},
		Mounts: []containertypes.MountPoint{{
			Type:        mount.TypeVolume,
			Name:        volumeName,
			Destination: destination,
		}},
	}
}

func TestCaptureLegacyRuntimeAcceptsOnlyGeneratedAnonymousHelperVolume(t *testing.T) {
	valid := legacyHelper("helper-valid", "emsi/vaka-init:v0.1.2", testLegacyVolume, legacyRuntimePath)
	wrongImage := legacyHelper("helper-wrong-image", "example/helper:latest", testLegacyVolume, legacyRuntimePath)
	namedVolume := legacyHelper("helper-named-volume", "emsi/vaka-init:v0.1.2", "user-data", legacyRuntimePath)
	wrongPath := legacyHelper("helper-wrong-path", "emsi/vaka-init:v0.1.2", testLegacyVolume, "/data")
	fake := &fakeLegacyRuntimeClient{listFn: func(options containertypes.ListOptions) ([]containertypes.Summary, error) {
		labels := options.Filters.Get("label")
		if len(labels) != 2 {
			t.Fatalf("capture labels = %v, want project and service filters", labels)
		}
		return []containertypes.Summary{valid, wrongImage, namedVolume, wrongPath}, nil
	}}
	ds := &dockerServices{legacy: fake, targetDesc: "test-context"}
	state, err := ds.CaptureLegacyRuntime(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Volumes) != 1 || !state.Volumes[testLegacyVolume]["helper-valid"] {
		t.Fatalf("captured state = %+v, want only valid helper volume", state)
	}
}

func TestCleanupLegacyRuntimeDefersWhileServiceUsesVolume(t *testing.T) {
	fake := &fakeLegacyRuntimeClient{listFn: func(containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{ID: "helper"}, {ID: "still-legacy-service"}}, nil
	}}
	ds := &dockerServices{legacy: fake}
	state := legacyRuntimeState{Volumes: map[string]map[string]bool{
		testLegacyVolume: {"helper": true},
	}}
	result, err := ds.CleanupLegacyRuntime(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeferredVolumes != 1 || len(fake.removedHelpers) != 0 || len(fake.removedVolumes) != 0 {
		t.Fatalf("result=%+v helpers=%v volumes=%v", result, fake.removedHelpers, fake.removedVolumes)
	}
}

func TestCleanupLegacyRuntimeRemovesHelperThenVolumeWithoutForce(t *testing.T) {
	fake := &fakeLegacyRuntimeClient{listFn: func(containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{ID: "helper"}}, nil
	}}
	ds := &dockerServices{legacy: fake}
	state := legacyRuntimeState{Volumes: map[string]map[string]bool{
		testLegacyVolume: {"helper": true},
	}}
	result, err := ds.CleanupLegacyRuntime(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedVolumes != 1 || len(fake.removedHelpers) != 1 || len(fake.removedVolumes) != 1 {
		t.Fatalf("result=%+v helpers=%v volumes=%v", result, fake.removedHelpers, fake.removedVolumes)
	}
	if fake.forceValues[0] {
		t.Fatal("legacy volume removal must never use force")
	}
}

func TestCleanupLegacyRuntimeHandlesHelperAlreadyRemovedByDown(t *testing.T) {
	fake := &fakeLegacyRuntimeClient{
		listFn:       func(containertypes.ListOptions) ([]containertypes.Summary, error) { return nil, nil },
		containerErr: errdefs.NotFound(errors.New("gone")),
	}
	ds := &dockerServices{legacy: fake}
	state := legacyRuntimeState{Volumes: map[string]map[string]bool{
		testLegacyVolume: {"helper": true},
	}}
	result, err := ds.CleanupLegacyRuntime(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedVolumes != 1 || len(fake.removedVolumes) != 1 {
		t.Fatalf("result=%+v removedVolumes=%v", result, fake.removedVolumes)
	}
}

func TestCleanupLegacyRuntimeTreatsVolumeUseRaceAsDeferred(t *testing.T) {
	fake := &fakeLegacyRuntimeClient{
		listFn:    func(containertypes.ListOptions) ([]containertypes.Summary, error) { return nil, nil },
		volumeErr: errdefs.Conflict(errors.New("volume is in use")),
	}
	ds := &dockerServices{legacy: fake}
	state := legacyRuntimeState{Volumes: map[string]map[string]bool{
		testLegacyVolume: {"helper": true},
	}}
	result, err := ds.CleanupLegacyRuntime(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeferredVolumes != 1 {
		t.Fatalf("result=%+v, want one deferred volume", result)
	}
}
