package recipe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeRootConfinement(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "recipe")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := OpenSafeRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	t.Run("write escaping path refused", func(t *testing.T) {
		if err := root.WriteFileSync("../pwned", []byte("x"), 0o644); err == nil {
			t.Fatal("write escaped the root")
		}
	})

	t.Run("rename escaping the root refused", func(t *testing.T) {
		if err := root.WriteFileSync("inside.txt", []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := root.Rename("inside.txt", "../stolen.txt"); err == nil {
			t.Fatal("rename escaped the root")
		}
	})

	t.Run("open through out-of-tree symlink refused", func(t *testing.T) {
		if err := root.Symlink("../outside.txt", "sneaky"); err != nil {
			t.Fatal(err) // creating the link is allowed; following it is not
		}
		if _, err := root.Open("sneaky"); err == nil {
			t.Fatal("opened a file outside the root through a symlink")
		}
		// Lstat and Readlink observe the link itself without following it.
		if fi, err := root.Lstat("sneaky"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("Lstat: %v %v", fi, err)
		}
		if target, err := root.Readlink("sneaky"); err != nil || target != "../outside.txt" {
			t.Fatalf("Readlink: %q %v", target, err)
		}
	})

	t.Run("write through swapped symlink component refused", func(t *testing.T) {
		if err := root.Mkdir("sub", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := root.Remove("sub"); err != nil {
			t.Fatal(err)
		}
		if err := root.Symlink("..", "sub"); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFileSync("sub/escaped.txt", []byte("x"), 0o644); err == nil {
			t.Fatal("wrote through a symlinked path component pointing outside")
		}
	})

	t.Run("create excl refuses existing", func(t *testing.T) {
		if err := root.WriteFileSync("once", []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := root.CreateExcl("once", 0o644); err == nil {
			t.Fatal("CreateExcl overwrote an existing file")
		}
	})

	t.Run("sync dir of root works", func(t *testing.T) {
		if err := root.SyncDir("."); err != nil {
			t.Fatalf("SyncDir: %v", err)
		}
	})
}

func TestSafeRootSyncSeamRecordsOrder(t *testing.T) {
	dir := t.TempDir()
	root, err := OpenSafeRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var synced []string
	old := syncFn
	syncFn = func(f *os.File) error {
		synced = append(synced, filepath.Base(f.Name()))
		return f.Sync()
	}
	defer func() { syncFn = old }()

	if err := root.WriteFileSync("a.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := root.SyncDir("."); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 2 || synced[0] != "a.txt" {
		t.Fatalf("sync order = %v, want [a.txt <rootdir>]", synced)
	}
}

func TestWalkFiles(t *testing.T) {
	dir := t.TempDir()
	root, err := OpenSafeRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if err := root.MkdirAll("sub/deep", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.txt", "sub/b.txt", "sub/deep/c.txt"} {
		if err := root.WriteFileSync(p, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.Symlink("a.txt", "link"); err != nil {
		t.Fatal(err)
	}

	var seen []string
	err = root.WalkFiles(func(path string, _ os.DirEntry) error {
		seen = append(seen, path)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	want := "a.txt link sub/b.txt sub/deep/c.txt"
	if strings.Join(seen, " ") != want {
		t.Fatalf("walked %v, want %s", seen, want)
	}
}
