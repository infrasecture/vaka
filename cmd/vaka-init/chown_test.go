//go:build linux

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/policy"
)

func withChownStubs(t *testing.T, mounts []mountInfoEntry, lchown func(string, int, int) error) {
	t.Helper()
	oldReadMountInfo := readMountInfoFn
	oldLchown := lchownFn
	t.Cleanup(func() {
		readMountInfoFn = oldReadMountInfo
		lchownFn = oldLchown
	})
	readMountInfoFn = func(_ string) ([]mountInfoEntry, error) { return mounts, nil }
	lchownFn = lchown
}

func TestApplyChownActionsOwnerOmittedWithoutGeneratedUserFails(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "owned-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	f.Close()

	withChownStubs(t, []mountInfoEntry{
		{MountPoint: filepath.Clean(filepath.Dir(f.Name())), MountOpts: map[string]bool{"rw": true}},
	}, func(_ string, _ int, _ int) error { return nil })

	err = applyChownActions([]policy.ChownAction{{Path: f.Name()}}, nil, "/does/not/exist", "/does/not/exist")
	if err == nil {
		t.Fatal("expected error for omitted owner with empty generated user")
	}
	if !strings.Contains(err.Error(), "services.<name>.user is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyChownActionsPathMustExist(t *testing.T) {
	nonexistent := filepath.Join(t.TempDir(), "no-such-path")
	withChownStubs(t, []mountInfoEntry{
		{MountPoint: filepath.Clean(filepath.Dir(nonexistent)), MountOpts: map[string]bool{"rw": true}},
	}, func(_ string, _ int, _ int) error { return nil })

	err := applyChownActions(
		[]policy.ChownAction{{Path: nonexistent}},
		&execIdentity{UID: 1000, GID: 1000},
		"/does/not/exist",
		"/does/not/exist",
	)
	if err == nil {
		t.Fatal("expected error for missing runtime.chown path")
	}
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyChownActionsRejectsRootMount(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "owned-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	f.Close()

	withChownStubs(t, []mountInfoEntry{
		{MountPoint: "/", MountOpts: map[string]bool{"rw": true}},
	}, func(_ string, _ int, _ int) error { return nil })

	err = applyChownActions(
		[]policy.ChownAction{{Path: f.Name()}},
		&execIdentity{UID: 1000, GID: 1000},
		"/does/not/exist",
		"/does/not/exist",
	)
	if err == nil {
		t.Fatal("expected root-mount rejection")
	}
	if !strings.Contains(err.Error(), "root filesystem mount") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyChownActionsRejectsReadOnlyMount(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "owned-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	f.Close()

	withChownStubs(t, []mountInfoEntry{
		{MountPoint: filepath.Clean(filepath.Dir(f.Name())), MountOpts: map[string]bool{"ro": true}},
	}, func(_ string, _ int, _ int) error { return nil })

	err = applyChownActions(
		[]policy.ChownAction{{Path: f.Name()}},
		&execIdentity{UID: 1000, GID: 1000},
		"/does/not/exist",
		"/does/not/exist",
	)
	if err == nil {
		t.Fatal("expected read-only mount rejection")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyChownActionsExplicitOwnerSuccess(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "owned-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	f.Close()

	var gotPath string
	var gotUID, gotGID int
	withChownStubs(t, []mountInfoEntry{
		{MountPoint: filepath.Clean(filepath.Dir(f.Name())), MountOpts: map[string]bool{"rw": true}},
	}, func(path string, uid, gid int) error {
		gotPath, gotUID, gotGID = path, uid, gid
		return nil
	})

	err = applyChownActions(
		[]policy.ChownAction{{Path: f.Name(), Owner: "123:456"}},
		nil,
		"/does/not/exist",
		"/does/not/exist",
	)
	if err != nil {
		t.Fatalf("applyChownActions: %v", err)
	}
	if gotPath != f.Name() || gotUID != 123 || gotGID != 456 {
		t.Fatalf("lchown got %q %d:%d, want %q 123:456", gotPath, gotUID, gotGID, f.Name())
	}
}

func TestFindMountUsesLongestPrefix(t *testing.T) {
	mounts := []mountInfoEntry{
		{MountPoint: "/tmp", MountOpts: map[string]bool{"rw": true}},
		{MountPoint: "/tmp/data", MountOpts: map[string]bool{"rw": true}},
	}
	got := findMount("/tmp/data/x", mounts)
	if got == nil {
		t.Fatal("findMount returned nil")
	}
	if got.MountPoint != "/tmp/data" {
		t.Fatalf("findMount mountpoint = %q, want /tmp/data", got.MountPoint)
	}
}

func TestChownPathRecursiveDoesNotFollowSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	childDir := filepath.Join(root, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	childFile := filepath.Join(childDir, "file.txt")
	if err := os.WriteFile(childFile, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write child file: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(childDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	oldLchown := lchownFn
	defer func() { lchownFn = oldLchown }()
	var called []string
	lchownFn = func(path string, _ int, _ int) error {
		called = append(called, path)
		return nil
	}

	if err := chownPath(root, 1000, 1000, true); err != nil {
		t.Fatalf("chownPath recursive: %v", err)
	}

	want := []string{root, childDir, childFile, link}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("lchown call paths = %v, want %v", called, want)
	}
}

func TestParseMountInfoLineDecodesEscapes(t *testing.T) {
	line := "27 22 0:23 / /var/lib/docker\\040data rw,relatime - ext4 /dev/sda rw"
	got, err := parseMountInfoLine(line)
	if err != nil {
		t.Fatalf("parseMountInfoLine: %v", err)
	}
	if got.MountPoint != "/var/lib/docker data" {
		t.Fatalf("mountpoint = %q, want %q", got.MountPoint, "/var/lib/docker data")
	}
	if got.FSType != "ext4" {
		t.Fatalf("fstype = %q, want ext4", got.FSType)
	}
}
