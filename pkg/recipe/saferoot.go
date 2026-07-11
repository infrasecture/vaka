// Package recipe implements the local side of vaka recipes: hardened
// extraction of recipe archives and (in later commits) the lock/journal
// models and the install/update engine.
//
// Design: docs/design/recipes-registry.md §6–§7.
package recipe

import (
	"fmt"
	"io/fs"
	"os"
)

// syncFn is the fsync seam: the update transaction's durability tests
// replace it to record ordering. It receives every file and directory
// handle vaka syncs.
var syncFn = func(f *os.File) error { return f.Sync() }

// SafeRoot confines all recipe-directory I/O beneath one root: paths are
// resolved component-by-component with symlinks never followed out of the
// root (os.Root), and types are validated on opened handles at mutation
// time, not on pre-scan results. It is the single primitive shared by the
// tarball extractor and the update engine.
type SafeRoot struct {
	r *os.Root
}

// OpenSafeRoot opens dir as a confinement root.
func OpenSafeRoot(dir string) (*SafeRoot, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &SafeRoot{r: r}, nil
}

// Close releases the root handle.
func (s *SafeRoot) Close() error { return s.r.Close() }

// Name returns the directory the root was opened on.
func (s *SafeRoot) Name() string { return s.r.Name() }

// Lstat stats path without following a final symlink.
func (s *SafeRoot) Lstat(path string) (fs.FileInfo, error) { return s.r.Lstat(path) }

// Readlink returns the target of the symlink at path.
func (s *SafeRoot) Readlink(path string) (string, error) { return s.r.Readlink(path) }

// Open opens path read-only.
func (s *SafeRoot) Open(path string) (*os.File, error) { return s.r.Open(path) }

// ReadFile reads the file at path.
func (s *SafeRoot) ReadFile(path string) ([]byte, error) { return s.r.ReadFile(path) }

// CreateExcl creates path exclusively (O_EXCL): it fails if path exists.
func (s *SafeRoot) CreateExcl(path string, perm fs.FileMode) (*os.File, error) {
	return s.r.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

// WriteFileSync exclusively creates path with data and fsyncs it before
// closing — the staged-write half of every write-then-rename.
func (s *SafeRoot) WriteFileSync(path string, data []byte, perm fs.FileMode) error {
	f, err := s.CreateExcl(path, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := syncFn(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Rename renames old to new within the root.
func (s *SafeRoot) Rename(old, new string) error { return s.r.Rename(old, new) }

// Remove removes the file or empty directory at path.
func (s *SafeRoot) Remove(path string) error { return s.r.Remove(path) }

// RemoveAll removes path and any children.
func (s *SafeRoot) RemoveAll(path string) error { return s.r.RemoveAll(path) }

// Mkdir creates one directory.
func (s *SafeRoot) Mkdir(path string, perm fs.FileMode) error { return s.r.Mkdir(path, perm) }

// MkdirAll creates a directory and any missing parents.
func (s *SafeRoot) MkdirAll(path string, perm fs.FileMode) error { return s.r.MkdirAll(path, perm) }

// Symlink creates a symlink at path pointing to target. The caller is
// responsible for target policy (the extractor rejects absolute and
// escaping targets); creation itself never follows the target.
func (s *SafeRoot) Symlink(target, path string) error { return s.r.Symlink(target, path) }

// Chmod changes the mode of path.
func (s *SafeRoot) Chmod(path string, mode fs.FileMode) error { return s.r.Chmod(path, mode) }

// SyncDir fsyncs the directory at path ("." for the root itself), making
// preceding renames and unlinks in it durable.
func (s *SafeRoot) SyncDir(path string) error {
	d, err := s.r.Open(path)
	if err != nil {
		return err
	}
	if err := syncFn(d); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// WalkFiles calls fn for every file and symlink beneath the root (relative
// paths, lexical order), skipping directories themselves.
func (s *SafeRoot) WalkFiles(fn func(path string, d fs.DirEntry) error) error {
	return fs.WalkDir(s.r.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if path == "." || d.IsDir() {
			return nil
		}
		return fn(path, d)
	})
}
