// cmd/vaka/get_update_test.go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vaka.dev/vaka/pkg/recipe"
)

func writeLockDir(t *testing.T, reg, name, ver string) string {
	t.Helper()
	dir := t.TempDir()
	l := recipe.NewLock(reg, name, ver, "sha256:"+strings.Repeat("0", 64))
	data, err := l.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, recipe.LockFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveGetArgs(t *testing.T) {
	// Named forms: parsed directly, no filesystem access.
	t.Run("name only", func(t *testing.T) {
		ref, dir, err := resolveGetArgs([]string{"codex"})
		if err != nil || ref.Name != "codex" || ref.Version != "" || dir != "./codex" {
			t.Fatalf("ref=%+v dir=%q err=%v", ref, dir, err)
		}
	})
	t.Run("qualified name@version + dir", func(t *testing.T) {
		ref, dir, err := resolveGetArgs([]string{"acme/codex@1.2.3", "d"})
		if err != nil || ref.Registry != "acme" || ref.Name != "codex" || ref.Version != "1.2.3" || dir != "d" {
			t.Fatalf("ref=%+v dir=%q err=%v", ref, dir, err)
		}
	})

	// Nameless forms: identity comes from the target directory's lock.
	t.Run("@version with explicit dir", func(t *testing.T) {
		ld := writeLockDir(t, "testreg", "demo", "1.0.0")
		ref, dir, err := resolveGetArgs([]string{"@2.0.0", ld})
		if err != nil || ref.Registry != "testreg" || ref.Name != "demo" || ref.Version != "2.0.0" || dir != ld {
			t.Fatalf("ref=%+v dir=%q err=%v", ref, dir, err)
		}
	})
	t.Run("bare form reads cwd lock", func(t *testing.T) {
		ld := writeLockDir(t, "testreg", "demo", "1.0.0")
		t.Chdir(ld)
		ref, dir, err := resolveGetArgs(nil)
		if err != nil || ref.Registry != "testreg" || ref.Name != "demo" || ref.Version != "" || dir != "." {
			t.Fatalf("ref=%+v dir=%q err=%v", ref, dir, err)
		}
	})
	t.Run("no recipe to update", func(t *testing.T) {
		_, _, err := resolveGetArgs([]string{"@2.0.0", t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "no vaka recipe") {
			t.Fatalf("err = %v, want 'no vaka recipe'", err)
		}
	})
	t.Run("bare @version must be exact", func(t *testing.T) {
		ld := writeLockDir(t, "testreg", "demo", "1.0.0")
		_, _, err := resolveGetArgs([]string{"@1.2", ld})
		if err == nil || !strings.Contains(err.Error(), "exact SemVer") {
			t.Fatalf("err = %v, want exact-SemVer refusal", err)
		}
	})
}

// TestGetUpdateInPlaceForms exercises the update-in-place forms end to end
// against the offline fixture registry (demo@1.0.0). A same-version update is a
// no-op, which is enough to prove the arg forms resolve the directory's recipe
// and drive the update path.
func TestGetUpdateInPlaceForms(t *testing.T) {
	fixtureRegistry(t, matchingPolicyBlock())
	base := t.TempDir()
	target := filepath.Join(base, "demo")

	if _, _, err := runRecipeCmd(t, "get", "demo", target); err != nil {
		t.Fatalf("install: %v", err)
	}

	// @version + explicit dir updates that dir without repeating the name.
	stdout, _, err := runRecipeCmd(t, "get", "@1.0.0", target)
	if err != nil {
		t.Fatalf("@version dir: %v", err)
	}
	if !strings.Contains(stdout, "demo@1.0.0 already up to date") {
		t.Fatalf("@version output:\n%s", stdout)
	}

	// Bare form updates the current directory.
	t.Chdir(target)
	stdout, _, err = runRecipeCmd(t, "get")
	if err != nil {
		t.Fatalf("bare: %v", err)
	}
	if !strings.Contains(stdout, "demo@1.0.0 already up to date") {
		t.Fatalf("bare output:\n%s", stdout)
	}

	// Bare form outside a recipe directory errors clearly.
	t.Chdir(base)
	if _, _, err := runRecipeCmd(t, "get"); err == nil ||
		!strings.Contains(err.Error(), "no vaka recipe") {
		t.Fatalf("bare in non-recipe dir: err = %v", err)
	}
}
