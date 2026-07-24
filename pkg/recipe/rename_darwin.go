//go:build darwin

package recipe

import "golang.org/x/sys/unix"

// renameNoReplace renames oldpath to newpath, failing with EEXIST if newpath
// already exists (renamex_np with RENAME_EXCL) — the darwin equivalent of
// Linux RENAME_NOREPLACE, giving the same atomic no-adopt commit.
func renameNoReplace(oldpath, newpath string) error {
	return unix.RenamexNp(oldpath, newpath, unix.RENAME_EXCL)
}
