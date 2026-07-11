package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Lock entry states (design §6): one canonical string per tracked path,
// compared by the update pre-check and recorded by install and journal.
//
//	sha256:<hex>     regular file
//	sha256:<hex>+x   regular file with any executable bit set
//	link:<target>    symlink, storing its literal target path (git-style)
const (
	statePrefixFile = "sha256:"
	statePrefixLink = "link:"
	stateExecSuffix = "+x"
)

// EntryState computes the lock-entry state of path inside root. It opens
// the path and validates the type on the handle (mutation-time semantics),
// so a concurrently swapped entry yields an error, not a wrong state.
func EntryState(root *SafeRoot, path string) (string, error) {
	fi, err := root.Lstat(path)
	if err != nil {
		return "", err
	}

	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := root.Readlink(path)
		if err != nil {
			return "", err
		}
		return statePrefixLink + target, nil
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: unsupported entry type %s", path, fi.Mode().Type())
	}

	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hfi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !hfi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: entry changed type while reading", path)
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	state := statePrefixFile + hex.EncodeToString(h.Sum(nil))
	if hfi.Mode().Perm()&0o111 != 0 {
		state += stateExecSuffix
	}
	return state, nil
}

// IsLinkState reports whether a lock-entry state describes a symlink.
func IsLinkState(state string) bool { return strings.HasPrefix(state, statePrefixLink) }
