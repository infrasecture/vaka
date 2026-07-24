//go:build !linux && !darwin

package recipe

import (
	"os"
	"syscall"
)

// renameNoReplace is the portable fallback for platforms without an atomic
// no-replace rename (vaka's release targets are linux and darwin, which have
// native syscalls). It pre-checks existence, so it carries a small TOCTOU
// window — acceptable only on non-release platforms.
func renameNoReplace(oldpath, newpath string) error {
	if _, err := os.Lstat(newpath); err == nil {
		return syscall.EEXIST
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(oldpath, newpath)
}
