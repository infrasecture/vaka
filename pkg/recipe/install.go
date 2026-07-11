package recipe

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// InstallSpec describes one fresh install of a fetched, digest-verified
// recipe tarball.
type InstallSpec struct {
	Registry    string // configured registry name (lock provenance)
	Name        string // recipe name; also the tarball's top-level directory
	Version     string
	Digest      string // sha256:<hex> of the tarball, already verified
	TarballPath string // local path of the verified tarball
	Target      string // directory to create; must not exist
}

// Install materializes a recipe at spec.Target: extract into a dot-prefixed
// staging sibling (same filesystem), fsync the tree, write the lock, and
// commit with a single no-replace atomic rename. A fresh install is
// all-or-nothing: any existing target path — including an empty directory —
// is refused with no override (design §6), and failure leaves nothing
// behind.
func Install(spec InstallSpec) (*Lock, error) {
	target := filepath.Clean(spec.Target)
	if _, err := os.Lstat(target); err == nil {
		return nil, fmt.Errorf(
			"target %q already exists; vaka get never adopts or writes into an existing path — remove it or choose another directory (updates require a %s inside)",
			target, LockFileName)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	parent := filepath.Dir(target)
	staging, err := os.MkdirTemp(parent, ".vaka-get-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(staging)
		}
	}()

	tarball, err := os.Open(spec.TarballPath)
	if err != nil {
		return nil, err
	}
	defer tarball.Close()
	if err := ExtractRecipe(tarball, spec.Name, staging); err != nil {
		return nil, err
	}

	root, err := OpenSafeRoot(staging)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	lock := NewLock(spec.Registry, spec.Name, spec.Version, spec.Digest)
	if err := root.WalkFiles(func(path string, _ fs.DirEntry) error {
		state, err := EntryState(root, path)
		if err != nil {
			return err
		}
		lock.Files[path] = state
		return nil
	}); err != nil {
		return nil, err
	}

	data, err := lock.Marshal()
	if err != nil {
		return nil, err
	}
	if err := root.WriteFileSync(LockFileName, data, 0o644); err != nil {
		return nil, err
	}
	if err := syncTree(root); err != nil {
		return nil, err
	}

	if err := commitNewDir(staging, target); err != nil {
		return nil, err
	}
	committed = true

	if err := syncPath(parent); err != nil {
		return nil, err
	}
	return lock, nil
}

// commitNewDir atomically renames staging to target, failing (instead of
// replacing) if anything exists at target — this closes the race between
// the early existence check and the rename: even an empty directory or
// symlink planted at target in between cannot be replaced.
func commitNewDir(staging, target string) error {
	err := unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf(
			"target %q already exists; vaka get never adopts or writes into an existing path — remove it or choose another directory (updates require a %s inside)",
			target, LockFileName)
	}
	return fmt.Errorf("install %q: %w", target, err)
}

// syncTree fsyncs every file and directory beneath the root, making the
// staged tree durable before the commit rename.
func syncTree(root *SafeRoot) error {
	if err := fs.WalkDir(root.r.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // symlinks are made durable by their directory's fsync
		}
		if d.IsDir() {
			return root.SyncDir(path)
		}
		f, err := root.Open(path)
		if err != nil {
			return err
		}
		if err := syncFn(f); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}); err != nil {
		return fmt.Errorf("sync staged tree: %w", err)
	}
	return nil
}

// syncPath fsyncs a directory by path (used for the target's parent, which
// lies outside any SafeRoot).
func syncPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := syncFn(d); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
