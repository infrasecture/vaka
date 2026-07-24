//go:build linux

package recipe

import "golang.org/x/sys/unix"

// renameNoReplace renames oldpath to newpath, failing with EEXIST if newpath
// already exists (RENAME_NOREPLACE) — an atomic no-adopt commit.
func renameNoReplace(oldpath, newpath string) error {
	return unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
}
