//go:build linux

package main

import (
	"io/fs"
	"strings"
	"testing"
	"time"
)

type runtimeMountFileInfo struct {
	mode fs.FileMode
}

func (i runtimeMountFileInfo) Name() string       { return "vaka" }
func (i runtimeMountFileInfo) Size() int64        { return 0 }
func (i runtimeMountFileInfo) Mode() fs.FileMode  { return i.mode }
func (i runtimeMountFileInfo) ModTime() time.Time { return time.Time{} }
func (i runtimeMountFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i runtimeMountFileInfo) Sys() any           { return nil }

func TestValidateRuntimeMount(t *testing.T) {
	readOnlyRuntime := mountInfoEntry{MountPoint: "/vaka", FSType: "overlay", MountOpts: map[string]bool{"ro": true}}
	tests := []struct {
		name   string
		info   fs.FileInfo
		mounts []mountInfoEntry
		want   string
	}{
		{
			name:   "literal read-only mount",
			info:   runtimeMountFileInfo{mode: fs.ModeDir | 0o555},
			mounts: []mountInfoEntry{{MountPoint: "/", MountOpts: map[string]bool{"rw": true}}, readOnlyRuntime},
		},
		{
			name:   "symbolic link",
			info:   runtimeMountFileInfo{mode: fs.ModeSymlink | 0o777},
			mounts: []mountInfoEntry{readOnlyRuntime},
			want:   "symbolic link",
		},
		{
			name:   "regular file",
			info:   runtimeMountFileInfo{mode: 0o555},
			mounts: []mountInfoEntry{readOnlyRuntime},
			want:   "not a directory",
		},
		{
			name:   "mount landed at resolved symlink target",
			info:   runtimeMountFileInfo{mode: fs.ModeDir | 0o755},
			mounts: []mountInfoEntry{{MountPoint: "/tmp/vaka-redirect", MountOpts: map[string]bool{"ro": true}}},
			want:   "found 0",
		},
		{
			name:   "writable runtime",
			info:   runtimeMountFileInfo{mode: fs.ModeDir | 0o755},
			mounts: []mountInfoEntry{{MountPoint: "/vaka", MountOpts: map[string]bool{"rw": true}}},
			want:   "not read-only",
		},
		{
			name:   "duplicate runtime mount",
			info:   runtimeMountFileInfo{mode: fs.ModeDir | 0o555},
			mounts: []mountInfoEntry{readOnlyRuntime, readOnlyRuntime},
			want:   "found 2",
		},
		{
			name: "nested runtime mount",
			info: runtimeMountFileInfo{mode: fs.ModeDir | 0o555},
			mounts: []mountInfoEntry{
				readOnlyRuntime,
				{MountPoint: "/vaka/sbin", MountOpts: map[string]bool{"ro": true}},
			},
			want: "unexpected nested mount",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntimeMount(tc.info, tc.mounts)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
