package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/docker/docker/api/types/container"
)

func TestValidateServiceImageMountPaths(t *testing.T) {
	directory := func(name string) containertypes.PathStat {
		return containertypes.PathStat{Name: name, Mode: os.ModeDir | 0o755}
	}
	symlink := func(name, target string) containertypes.PathStat {
		return containertypes.PathStat{Name: name, Mode: os.ModeSymlink | 0o777, LinkTarget: target}
	}

	tests := []struct {
		name         string
		svc          composetypes.ServiceConfig
		imageVolumes map[string]struct{}
		stats        map[string]containertypes.PathStat
		want         string
	}{
		{
			name:  "runtime path absent and unrelated target",
			svc:   composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{Target: "/workspace"}}},
			stats: map[string]containertypes.PathStat{},
		},
		{
			name:  "runtime path is real directory",
			stats: map[string]containertypes.PathStat{protectedRuntimePath: directory("vaka")},
		},
		{
			name:  "runtime path is symlink",
			stats: map[string]containertypes.PathStat{protectedRuntimePath: symlink("vaka", "/tmp/vaka-redirect")},
			want:  "is a symbolic link",
		},
		{
			name:  "runtime path is regular file",
			stats: map[string]containertypes.PathStat{protectedRuntimePath: {Name: "vaka", Mode: 0o644}},
			want:  "is not a directory",
		},
		{
			name: "volume target resolves into runtime",
			svc:  composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{Target: "/runtime-alias/replacement"}}},
			stats: map[string]containertypes.PathStat{
				"/runtime-alias": symlink("runtime-alias", "/vaka/sbin"),
			},
			want: "resolves through the service image to protected path",
		},
		{
			name: "nested volume target can be redirected by mounted parent",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{
				{Type: composetypes.VolumeTypeVolume, Target: "/mnt"},
				{Type: composetypes.VolumeTypeVolume, Target: "/mnt/link"},
			}},
			want: "externally populated ancestor",
		},
		{
			name: "nested target below fresh tmpfs is safe",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{
				{Type: composetypes.VolumeTypeTmpfs, Target: "/mnt"},
				{Type: composetypes.VolumeTypeVolume, Target: "/mnt/data"},
			}},
		},
		{
			name: "resolved ancestor mounted after descendant is safe",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{
				{Type: composetypes.VolumeTypeVolume, Target: "/deep/parent"},
				{Type: composetypes.VolumeTypeVolume, Target: "/child"},
			}},
			stats: map[string]containertypes.PathStat{
				"/deep":        directory("deep"),
				"/deep/parent": symlink("parent", "/mnt"),
				"/child":       symlink("child", "/mnt/link"),
			},
		},
		{
			name: "equal-depth mount order fails closed",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{
				{Type: composetypes.VolumeTypeVolume, Target: "/parent"},
				{Type: composetypes.VolumeTypeVolume, Target: "/child"},
			}},
			stats: map[string]containertypes.PathStat{
				"/parent": symlink("parent", "/mnt"),
				"/child":  symlink("child", "/mnt/link"),
			},
			want: "externally populated ancestor",
		},
		{
			name: "trailing whitespace remains part of target path",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeVolume, Target: "/a/b/runtime-alias ",
			}}},
			stats: map[string]containertypes.PathStat{
				"/a":                  directory("a"),
				"/a/b":                directory("b"),
				"/a/b/runtime-alias ": symlink("runtime-alias ", "/vaka/sbin"),
			},
			want: "resolves through the service image to protected path",
		},
		{
			name: "trailing whitespace in symlink target remains significant",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeVolume, Target: "/runtime-alias/child",
			}}},
			stats: map[string]containertypes.PathStat{
				"/runtime-alias": symlink("runtime-alias", "/vaka "),
			},
		},
		{
			name: "multi-hop image symlink resolves into runtime",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeVolume, Target: "/first/child",
			}}},
			stats: map[string]containertypes.PathStat{
				"/first":  symlink("first", "/second"),
				"/second": symlink("second", "/vaka/sbin"),
			},
			want: "resolves through the service image to protected path",
		},
		{
			name: "relative symlink resolving away from runtime is allowed",
			svc:  composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{Target: "/aliases/data"}}},
			stats: map[string]containertypes.PathStat{
				"/aliases": symlink("aliases", "workspace"),
			},
		},
		{
			name: "image volume resolves into runtime",
			imageVolumes: map[string]struct{}{
				"/runtime-volume": {},
			},
			stats: map[string]containertypes.PathStat{
				"/runtime-volume": symlink("runtime-volume", "/vaka"),
			},
			want: "image VOLUME",
		},
		{
			name: "implicit mount resolves into runtime",
			stats: map[string]containertypes.PathStat{
				"/etc": symlink("etc", "/vaka/sbin"),
			},
			want: "Docker implicit mount target",
		},
		{
			name: "relative mount target fails closed",
			svc:  composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{Target: "workspace"}}},
			want: "must be an absolute path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeDockerClient{statResults: tc.stats}
			ds := &dockerServices{c: client, targetDesc: "test-context"}
			err := ds.validateServiceImageMountPaths(context.Background(), "app", testRuntimeImageID, tc.svc, tc.imageVolumes)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if len(client.removed) != 1 || client.removed[0] != "rootfs-probe" {
				t.Fatalf("probe cleanup = %v, want [rootfs-probe]", client.removed)
			}
		})
	}
}

func TestValidateServiceImageMountPathsReportsProbeLifecycleFailures(t *testing.T) {
	createFailure := &fakeDockerClient{createErr: errors.New("create denied")}
	ds := &dockerServices{c: createFailure, targetDesc: "test-context"}
	err := ds.validateServiceImageMountPaths(context.Background(), "app", testRuntimeImageID, composetypes.ServiceConfig{}, nil)
	if err == nil || !strings.Contains(err.Error(), "create temporary rootfs probe") {
		t.Fatalf("create error = %v", err)
	}
	if len(createFailure.removed) != 0 {
		t.Fatalf("unexpected cleanup after failed create: %v", createFailure.removed)
	}

	cleanupFailure := &fakeDockerClient{removeErr: errors.New("remove denied")}
	ds = &dockerServices{c: cleanupFailure, targetDesc: "test-context"}
	err = ds.validateServiceImageMountPaths(context.Background(), "app", testRuntimeImageID, composetypes.ServiceConfig{}, nil)
	if err == nil || !strings.Contains(err.Error(), "remove temporary rootfs probe") {
		t.Fatalf("cleanup error = %v", err)
	}
}

func TestValidateServiceImageMountPathsUsesDaemonSecurityPaths(t *testing.T) {
	client := &fakeDockerClient{
		containerInspectResult: containertypes.InspectResponse{ContainerJSONBase: &containertypes.ContainerJSONBase{
			ID: "rootfs-probe",
			HostConfig: &containertypes.HostConfig{
				MaskedPaths: []string{"/proc/kcore"},
			},
		}},
	}
	ds := &dockerServices{c: client, targetDesc: "test-context"}
	err := ds.validateServiceImageMountPaths(context.Background(), "app", testRuntimeImageID, composetypes.ServiceConfig{
		Volumes: []composetypes.ServiceVolumeConfig{{
			Type: composetypes.VolumeTypeVolume, Target: "/proc",
		}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "Docker masked path") {
		t.Fatalf("daemon security path error = %v", err)
	}
}

func TestImagePathResolverRejectsSymlinkCycle(t *testing.T) {
	client := &fakeDockerClient{statResults: map[string]containertypes.PathStat{
		"/one": {Name: "one", Mode: os.ModeSymlink | 0o777, LinkTarget: "/two"},
		"/two": {Name: "two", Mode: os.ModeSymlink | 0o777, LinkTarget: "/one"},
	}}
	resolver := imagePathResolver{
		ctx: context.Background(), client: client, containerID: "rootfs-probe",
		cache: make(map[string]cachedPathStat),
	}
	_, err := resolver.resolve("/one/child")
	if err == nil || !strings.Contains(err.Error(), "too many symbolic links") {
		t.Fatalf("cycle error = %v", err)
	}
}
