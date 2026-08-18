//go:build linux

package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"vaka.dev/vaka/internal/runtimebundle"
)

// verifyRuntimeMount establishes the pathname invariant relied on by startup,
// healthchecks, and secure exec. Docker reports configured mount destinations
// lexically, but runc follows pre-existing image symlinks when it installs
// ordinary mounts. The process inside the namespace must therefore confirm
// that /vaka itself is the literal read-only mountpoint.
func verifyRuntimeMount() error {
	info, err := os.Lstat(runtimebundle.MountPath)
	if err != nil {
		return fmt.Errorf("lstat runtime path %s: %w", runtimebundle.MountPath, err)
	}
	mounts, err := readMountInfoFn("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	return validateRuntimeMount(info, mounts)
}

func validateRuntimeMount(info fs.FileInfo, mounts []mountInfoEntry) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime path %s is a symbolic link", runtimebundle.MountPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("runtime path %s is not a directory", runtimebundle.MountPath)
	}

	exact := 0
	for _, mount := range mounts {
		mountPoint := filepath.Clean(mount.MountPoint)
		if mountPoint == runtimebundle.MountPath {
			exact++
			if !mount.isReadOnly() {
				return fmt.Errorf("runtime mount %s is not read-only", runtimebundle.MountPath)
			}
			continue
		}
		if pathWithinMount(mountPoint, runtimebundle.MountPath) {
			return fmt.Errorf("unexpected nested mount %s under protected runtime %s", mountPoint, runtimebundle.MountPath)
		}
	}
	if exact != 1 {
		return fmt.Errorf("expected exactly one literal read-only mountpoint at %s, found %d", runtimebundle.MountPath, exact)
	}
	return nil
}
