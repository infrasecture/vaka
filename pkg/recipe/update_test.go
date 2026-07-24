package recipe

import (
	"archive/tar"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	digestV1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestV2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	digestV3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

const (
	composeV1 = "services: {app: {image: a:1}}\n"
	composeV2 = "services: {app: {image: a:2}}\n"
	composeV3 = "services: {app: {image: a:3}}\n"
	newFileV2 = "added in v2\n"
)

func demoTarball(t *testing.T, compose string, withConf, withNewFile bool) string {
	return demoTarballWith(t, compose, withConf, withNewFile, nil)
}

// manifestForCompose returns a recipe.yaml whose version matches the compose
// generation, so the staged manifest agrees with the version updateTo/Install
// request (identity is now cross-checked at commit).
func manifestForCompose(compose string) string {
	v := "1.0.0"
	switch compose {
	case composeV2:
		v = "2.0.0"
	case composeV3:
		v = "3.0.0"
	}
	return "apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: demo\nversion: " + v + "\ndescription: test recipe fixture\n"
}

// demoTarballWith builds a well-formed demo recipe tarball (recipe.yaml,
// compose, vaka.yaml) plus a set of extra relative paths (their parent
// directories are created automatically by the extractor).
func demoTarballWith(t *testing.T, compose string, withConf, withNewFile bool, extra map[string]string) string {
	t.Helper()
	entries := []tarEntry{
		{name: "demo/", typeflag: tar.TypeDir},
		{name: "demo/recipe.yaml", typeflag: tar.TypeReg, content: manifestForCompose(compose)},
		{name: "demo/compose.yaml", typeflag: tar.TypeReg, content: compose},
		{name: "demo/docker-compose.yaml", typeflag: tar.TypeSymlink, linkname: "compose.yaml"},
		{name: "demo/vaka.yaml", typeflag: tar.TypeReg, content: validVakaYAML},
		{name: "demo/run.sh", typeflag: tar.TypeReg, content: "#!/bin/sh\n", mode: 0o755},
	}
	if withConf {
		entries = append(entries,
			tarEntry{name: "demo/conf/", typeflag: tar.TypeDir},
			tarEntry{name: "demo/conf/app.yaml", typeflag: tar.TypeReg, content: "conf v1\n"})
	}
	if withNewFile {
		entries = append(entries, tarEntry{name: "demo/newfile.txt", typeflag: tar.TypeReg, content: newFileV2})
	}
	for rel, content := range extra {
		entries = append(entries, tarEntry{name: "demo/" + rel, typeflag: tar.TypeReg, content: content})
	}
	path := filepath.Join(t.TempDir(), "demo.tar.gz")
	if err := os.WriteFile(path, makeTarGz(t, entries).Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func tarballV1(t *testing.T) string { return demoTarball(t, composeV1, true, false) }
func tarballV2(t *testing.T) string { return demoTarball(t, composeV2, false, true) }
func tarballV3(t *testing.T) string { return demoTarball(t, composeV3, false, true) }

func installedV1(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "demo")
	_, err := Install(InstallSpec{
		Registry: "official", Name: "demo", Version: "1.0.0",
		Digest: digestV1, TarballPath: tarballV1(t), Target: target,
	})
	if err != nil {
		t.Fatalf("install v1: %v", err)
	}
	return target
}

func updateTo(target, version, digest, tarball string) (*UpdateResult, error) {
	return Update(UpdateSpec{
		Registry: "official", Name: "demo", Version: version,
		Digest: digest, TarballPath: tarball, Target: target,
	})
}

// dirState maps every non-reserved path to its entry state.
func dirState(t *testing.T, target string) map[string]string {
	t.Helper()
	root, err := OpenSafeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	states := map[string]string{}
	if err := root.WalkFiles(func(p string, _ fs.DirEntry) error {
		for _, seg := range strings.Split(p, "/") {
			if strings.HasPrefix(seg, reservedPrefix) {
				return nil
			}
		}
		s, err := EntryState(root, p)
		if err != nil {
			return err
		}
		states[p] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return states
}

func lockOf(t *testing.T, target string) *Lock {
	t.Helper()
	root, err := OpenSafeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	l, exists, err := ReadLock(root)
	if err != nil || !exists {
		t.Fatalf("ReadLock: exists=%v err=%v", exists, err)
	}
	return l
}

func hasResidue(t *testing.T, target string, name string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(target, name))
	return err == nil
}

var errAbort = errors.New("simulated crash")

func abortAt(t *testing.T, step string) {
	t.Helper()
	old := afterStep
	afterStep = func(s string) error {
		if s == step {
			return errAbort
		}
		return nil
	}
	t.Cleanup(func() { afterStep = old })
}

func mutate(t *testing.T, target, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(target, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateHappyPath(t *testing.T) {
	target := installedV1(t)
	res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.NoChange || len(res.Warnings) != 0 {
		t.Fatalf("res = %+v", res)
	}

	states := dirState(t, target)
	if !strings.Contains(readFile(t, target, "compose.yaml"), "a:2") {
		t.Fatal("compose.yaml was not replaced")
	}
	if _, ok := states["conf/app.yaml"]; ok {
		t.Fatal("dropped file was not deleted")
	}
	if readFile(t, target, "newfile.txt") != newFileV2 {
		t.Fatal("new file was not installed")
	}
	if states["docker-compose.yaml"] != "link:compose.yaml" {
		t.Fatal("symlink lost")
	}

	l := lockOf(t, target)
	if l.Version != "2.0.0" || l.Digest != digestV2 || len(l.Deviations) != 0 {
		t.Fatalf("lock = %+v", l)
	}
	if len(l.Files) != len(states) {
		t.Fatalf("lock tracks %d files, dir has %d", len(l.Files), len(states))
	}
	for p, s := range states {
		if l.Files[p] != s {
			t.Fatalf("lock state of %s = %q, disk %q", p, l.Files[p], s)
		}
	}
	if hasResidue(t, target, JournalFileName) || hasResidue(t, target, StagingDirName) {
		t.Fatal("journal or staging residue after clean update")
	}
	if !hasResidue(t, target, UpdateLockFileName) {
		t.Fatal("persistent update lock file missing")
	}
}

func TestUpdateSameVersionIsNoChange(t *testing.T) {
	target := installedV1(t)
	if _, err := updateTo(target, "2.0.0", digestV2, tarballV2(t)); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, target, LockFileName)

	res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if err != nil {
		t.Fatalf("re-update: %v", err)
	}
	if !res.NoChange {
		t.Fatal("expected NoChange")
	}
	if readFile(t, target, LockFileName) != before {
		t.Fatal("NoChange rewrote the lock")
	}
}

func TestUpdateMatrix(t *testing.T) {
	t.Run("modified tracked file still shipped blocks", func(t *testing.T) {
		target := installedV1(t)
		mutate(t, target, "compose.yaml", "user hacked this\n")
		before := dirState(t, target)
		beforeLock := readFile(t, target, LockFileName)

		_, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		var blocked *blockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("err = %v, want blockedError", err)
		}
		if !strings.Contains(err.Error(), "compose.yaml") || !strings.Contains(err.Error(), "does not merge") {
			t.Fatalf("error %q", err.Error())
		}
		if !reflect.DeepEqual(before, dirState(t, target)) || readFile(t, target, LockFileName) != beforeLock {
			t.Fatal("rejected update mutated the directory")
		}
	})

	t.Run("chmod of tracked file blocks", func(t *testing.T) {
		target := installedV1(t)
		if err := os.Chmod(filepath.Join(target, "compose.yaml"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err == nil || !strings.Contains(err.Error(), "compose.yaml") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("repointed symlink blocks", func(t *testing.T) {
		target := installedV1(t)
		link := filepath.Join(target, "docker-compose.yaml")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("vaka.yaml", link); err != nil {
			t.Fatal(err)
		}
		_, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err == nil || !strings.Contains(err.Error(), "docker-compose.yaml") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("modified file dropped upstream is kept and untracked", func(t *testing.T) {
		target := installedV1(t)
		mutate(t, target, "conf/app.yaml", "my precious edits\n")

		res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if readFile(t, target, "conf/app.yaml") != "my precious edits\n" {
			t.Fatal("user copy was touched")
		}
		l := lockOf(t, target)
		if _, tracked := l.Files["conf/app.yaml"]; tracked {
			t.Fatal("kept user copy is still tracked")
		}
		wantDev := Deviation{Path: "conf/app.yaml", Kind: DeviationKeptUserCopy}
		if len(l.Deviations) != 1 || l.Deviations[0] != wantDev {
			t.Fatalf("deviations = %+v", l.Deviations)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "user-owned") {
			t.Fatalf("warnings = %v", res.Warnings)
		}
	})

	t.Run("locally deleted shipped file is reinstalled", func(t *testing.T) {
		target := installedV1(t)
		if err := os.Remove(filepath.Join(target, "compose.yaml")); err != nil {
			t.Fatal(err)
		}
		res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if !strings.Contains(readFile(t, target, "compose.yaml"), "a:2") {
			t.Fatal("deleted file not reinstalled")
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "restored") {
			t.Fatalf("warnings = %v", res.Warnings)
		}
	})

	t.Run("locally deleted dropped file needs nothing", func(t *testing.T) {
		target := installedV1(t)
		if err := os.Remove(filepath.Join(target, "conf/app.yaml")); err != nil {
			t.Fatal(err)
		}
		res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err != nil || len(res.Warnings) != 0 || len(lockOf(t, target).Deviations) != 0 {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("untracked collision is skipped and converges after resolution", func(t *testing.T) {
		target := installedV1(t)
		mutate(t, target, "newfile.txt", "mine, not the recipe's\n")

		res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if readFile(t, target, "newfile.txt") != "mine, not the recipe's\n" {
			t.Fatal("user file was overwritten — the cardinal sin")
		}
		l := lockOf(t, target)
		if _, tracked := l.Files["newfile.txt"]; tracked {
			t.Fatal("skipped collision is tracked")
		}
		if len(l.Deviations) != 1 || l.Deviations[0].Kind != DeviationSkippedCollision {
			t.Fatalf("deviations = %+v", l.Deviations)
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "kept your file") {
			t.Fatalf("warnings = %v", res.Warnings)
		}

		// Warnings repeat while unresolved.
		res, err = updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err != nil || len(res.Warnings) != 1 {
			t.Fatalf("repeat: res=%+v err=%v", res, err)
		}

		// Resolution converges: remove the file, re-run, recipe copy lands.
		if err := os.Remove(filepath.Join(target, "newfile.txt")); err != nil {
			t.Fatal(err)
		}
		res, err = updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err != nil {
			t.Fatalf("converge: %v", err)
		}
		if readFile(t, target, "newfile.txt") != newFileV2 {
			t.Fatal("recipe file not installed after resolution")
		}
		l = lockOf(t, target)
		if len(l.Deviations) != 0 || l.Files["newfile.txt"] == "" {
			t.Fatalf("post-resolution lock = %+v", l)
		}
	})

	t.Run("untracked byte-identical file is adopted", func(t *testing.T) {
		target := installedV1(t)
		mutate(t, target, "newfile.txt", newFileV2)
		res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		l := lockOf(t, target)
		if l.Files["newfile.txt"] == "" || len(l.Deviations) != 0 || len(res.Warnings) != 0 {
			t.Fatalf("adopt failed: lock=%+v warnings=%v", l, res.Warnings)
		}
	})
}

func TestUpdateInterruptionsConverge(t *testing.T) {
	// Reference: what a clean v1→v2 update produces.
	ref := installedV1(t)
	if _, err := updateTo(ref, "2.0.0", digestV2, tarballV2(t)); err != nil {
		t.Fatal(err)
	}
	want := dirState(t, ref)
	wantLock := lockOf(t, ref)

	steps := []string{
		"locked", "stale-cleaned", "prechecked", "journal-written",
		"applied compose.yaml", "applied newfile.txt", "deleted conf/app.yaml",
		"committed", "cleaned",
	}
	for _, step := range steps {
		t.Run("abort after "+step, func(t *testing.T) {
			target := installedV1(t)
			abortAt(t, step)
			if _, err := updateTo(target, "2.0.0", digestV2, tarballV2(t)); !errors.Is(err, errAbort) {
				t.Fatalf("err = %v, want simulated crash", err)
			}
			afterStep = func(string) error { return nil }

			res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
			if err != nil {
				t.Fatalf("re-run after crash at %q: %v", step, err)
			}
			_ = res
			if got := dirState(t, target); !reflect.DeepEqual(got, want) {
				t.Fatalf("state after recovery differs:\ngot  %v\nwant %v", got, want)
			}
			l := lockOf(t, target)
			if !reflect.DeepEqual(l.Files, wantLock.Files) || l.Version != "2.0.0" {
				t.Fatalf("lock after recovery = %+v", l)
			}
			if hasResidue(t, target, JournalFileName) || hasResidue(t, target, StagingDirName) {
				t.Fatal("journal or staging residue after recovery")
			}
		})
	}
}

func TestUpdateInterruptedThenNewerTargetConverges(t *testing.T) {
	target := installedV1(t)

	// Interrupt the v2 update mid-apply: compose.yaml now holds v2 content
	// while the lock still says v1.
	abortAt(t, "applied compose.yaml")
	if _, err := updateTo(target, "2.0.0", digestV2, tarballV2(t)); !errors.Is(err, errAbort) {
		t.Fatal("expected simulated crash")
	}
	afterStep = func(string) error { return nil }
	if !strings.Contains(readFile(t, target, "compose.yaml"), "a:2") {
		t.Fatal("test setup: v2 content not applied")
	}

	// The re-run targets v3, not v2: the new journal must inherit the v2
	// states from the dangling journal or the B-state file would be
	// misclassified as a user edit.
	abortAt(t, "journal-written")
	if _, err := updateTo(target, "3.0.0", digestV3, tarballV3(t)); !errors.Is(err, errAbort) {
		t.Fatal("expected simulated crash at journal-written")
	}
	afterStep = func(string) error { return nil }

	root, err := OpenSafeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	j, exists, err := ReadJournal(root)
	root.Close()
	if err != nil || !exists {
		t.Fatalf("journal: exists=%v err=%v", exists, err)
	}
	accepted := strings.Join(j.Plan["compose.yaml"].Accepted, "\n")
	for _, content := range []string{composeV1, composeV2} {
		if !strings.Contains(accepted, sha256Of(content)) {
			t.Fatalf("journal accepted states missing hash of %q:\n%s", content, accepted)
		}
	}

	// Full v3 run converges.
	if _, err := updateTo(target, "3.0.0", digestV3, tarballV3(t)); err != nil {
		t.Fatalf("converge to v3: %v", err)
	}
	if !strings.Contains(readFile(t, target, "compose.yaml"), "a:3") {
		t.Fatal("compose.yaml is not v3")
	}
	if l := lockOf(t, target); l.Version != "3.0.0" {
		t.Fatalf("lock version = %s", l.Version)
	}
}

func TestUpdateJournalNeverMasksUserEdits(t *testing.T) {
	target := installedV1(t)
	abortAt(t, "applied compose.yaml")
	if _, err := updateTo(target, "2.0.0", digestV2, tarballV2(t)); !errors.Is(err, errAbort) {
		t.Fatal("expected simulated crash")
	}
	afterStep = func(string) error { return nil }

	// A genuine user edit made after the crash matches no recorded
	// upstream state and must still block.
	mutate(t, target, "vaka.yaml", "user edit after crash\n")
	_, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
	var blocked *blockedError
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "vaka.yaml") {
		t.Fatalf("err = %v, want block on vaka.yaml", err)
	}
}

func TestUpdateConcurrentUpdaterRefused(t *testing.T) {
	target := installedV1(t)
	f, err := os.OpenFile(filepath.Join(target, UpdateLockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	_, err = updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("err = %v, want ErrUpdateInProgress", err)
	}
}

func TestUpdateRejectsMalformedNewVersion(t *testing.T) {
	target := installedV1(t)
	before := readFile(t, target, LockFileName)

	// A "new version" tarball that extracts but is not a well-formed recipe
	// (no recipe.yaml). It must not replace the working install.
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	buf := makeTarGz(t, []tarEntry{
		{name: "demo/", typeflag: tar.TypeDir},
		{name: "demo/compose.yaml", typeflag: tar.TypeReg, content: composeV2},
		{name: "demo/vaka.yaml", typeflag: tar.TypeReg, content: validVakaYAML},
	})
	if err := os.WriteFile(bad, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := updateTo(target, "2.0.0", digestV2, bad)
	if err == nil || !strings.Contains(err.Error(), "missing recipe.yaml") {
		t.Fatalf("err = %v, want fail-closed on malformed artifact", err)
	}
	if readFile(t, target, LockFileName) != before {
		t.Fatal("malformed update replaced the working recipe's lock")
	}
	if !strings.Contains(readFile(t, target, "compose.yaml"), "a:1") {
		t.Fatal("malformed update mutated the working recipe")
	}
	if hasResidue(t, target, JournalFileName) {
		t.Fatal("malformed update left a wedged journal")
	}
}

func TestUpdateRejectsSymlinkedParent(t *testing.T) {
	target := installedV1(t)
	// Replace the tracked conf/ directory with an in-tree symlink.
	if err := os.RemoveAll(filepath.Join(target, "conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(target, "conf")); err != nil {
		t.Fatal(err)
	}
	// Update to a version that still ships conf/app.yaml.
	tarball := demoTarballWith(t, composeV2, true, false, nil)
	_, err := updateTo(target, "2.0.0", digestV2, tarball)
	var obstruction *obstructionError
	if !errors.As(err, &obstruction) {
		t.Fatalf("err = %v, want obstructionError for symlinked parent", err)
	}
}

func TestUpdateLockRefusesSymlinkedLockFile(t *testing.T) {
	target := installedV1(t)
	// Plant the update lock as a symlink. os.Root follows in-tree symlinks
	// (and ignores O_NOFOLLOW), so acquireUpdateLock's explicit Lstat guard
	// must refuse it — otherwise the flock could be redirected to another
	// inode and defeat the stable-lock exclusion.
	if err := os.Symlink("compose.yaml", filepath.Join(target, UpdateLockFileName)); err != nil {
		t.Fatal(err)
	}
	_, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if err == nil || errors.Is(err, ErrUpdateInProgress) ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlinked-lock refusal", err)
	}
}

func TestUpToDateFailsFastWhenLocked(t *testing.T) {
	target := installedV1(t)
	// Another updater holds the update flock (as if mid-update, before its
	// journal is written). UpToDate must fail fast, not read a snapshot the
	// other updater is about to invalidate.
	f, err := os.OpenFile(filepath.Join(target, UpdateLockFileName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if _, err := UpToDate(target, "official", "demo", "1.0.0", digestV1); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("err = %v, want ErrUpdateInProgress", err)
	}
}

func TestUpdateContainmentMidApplySymlinkSwap(t *testing.T) {
	target := installedV1(t)

	// A canary directory OUTSIDE the recipe tree; a redirected write would
	// land here.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The update introduces a file under a new subdirectory.
	tarball := demoTarballWith(t, composeV2, false, false, map[string]string{
		"escapehatch/planted.txt": "recipe content",
	})

	// Mid-apply (right after the journal is written, before files are moved),
	// a concurrent actor swaps the new subdir path for a symlink escaping the
	// recipe root — the classic containment attack.
	old := afterStep
	afterStep = func(step string) error {
		if step == "journal-written" {
			_ = os.Remove(filepath.Join(target, "escapehatch"))
			if err := os.Symlink(outside, filepath.Join(target, "escapehatch")); err != nil {
				t.Fatalf("plant symlink: %v", err)
			}
		}
		return nil
	}
	t.Cleanup(func() { afterStep = old })

	_, err := updateTo(target, "2.0.0", digestV2, tarball)
	if err == nil {
		t.Fatal("expected the escaping-symlink apply to be refused")
	}
	// Containment guarantee (the one os.Root DOES provide): no write escaped
	// the recipe root into the outside canary.
	if _, statErr := os.Stat(filepath.Join(outside, "planted.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("write escaped the recipe root into %s (err=%v)", outside, statErr)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("outside dir was mutated: %v", entries)
	}
}

func TestUpdateRequiresLock(t *testing.T) {
	dir := t.TempDir()
	_, err := updateTo(dir, "2.0.0", digestV2, tarballV2(t))
	if err == nil || !strings.Contains(err.Error(), "not a vaka-managed recipe directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateRefusesDifferentRecipeIdentity(t *testing.T) {
	target := installedV1(t)
	before := readFile(t, target, LockFileName)

	// Wrong recipe name.
	_, err := Update(UpdateSpec{
		Registry: "official", Name: "other", Version: "2.0.0",
		Digest: digestV2, TarballPath: tarballV2(t), Target: target,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to update a different recipe") {
		t.Fatalf("name mismatch err = %v", err)
	}

	// Wrong registry.
	_, err = Update(UpdateSpec{
		Registry: "evil", Name: "demo", Version: "2.0.0",
		Digest: digestV2, TarballPath: tarballV2(t), Target: target,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to update a different recipe") {
		t.Fatalf("registry mismatch err = %v", err)
	}

	if readFile(t, target, LockFileName) != before {
		t.Fatal("identity refusal mutated the lock")
	}
	if hasResidue(t, target, JournalFileName) || hasResidue(t, target, StagingDirName) {
		t.Fatal("identity refusal left residue")
	}
}

func TestUpdateRefusesObstructedParent(t *testing.T) {
	target := installedV1(t)
	// Replace the tracked conf/ directory with a regular file.
	if err := os.RemoveAll(filepath.Join(target, "conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "conf"), []byte("user file where a dir was\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Update to a version that still ships conf/app.yaml, so the plan wants
	// to put a file under the now-obstructed parent.
	tarball := demoTarball(t, composeV2, true, false)
	_, err := updateTo(target, "2.0.0", digestV2, tarball)
	var obstruction *obstructionError
	if !errors.As(err, &obstruction) {
		t.Fatalf("err = %v, want obstructionError", err)
	}
	if !strings.Contains(err.Error(), "conf/app.yaml") || !strings.Contains(err.Error(), "non-directory") {
		t.Fatalf("error text = %q", err.Error())
	}
	// Refused in the pre-check, before the journal is written — the key
	// property is no wedged journal (staging is disposable, cleaned next run,
	// same as the blocked case). The user file is untouched.
	if hasResidue(t, target, JournalFileName) {
		t.Fatal("obstruction refusal left a wedged journal")
	}
	if readFile(t, target, "conf") != "user file where a dir was\n" {
		t.Fatal("obstruction refusal touched the user's file")
	}
}

func TestUpdateInstallsNewNestedFile(t *testing.T) {
	target := installedV1(t)
	// v2 tarball with a file under a new nested directory.
	tarball := demoTarballWith(t, composeV2, false, false, map[string]string{
		"conf/extra/deep.yaml": "nested new file\n",
	})
	if _, err := updateTo(target, "2.0.0", digestV2, tarball); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := readFile(t, target, "conf/extra/deep.yaml"); got != "nested new file\n" {
		t.Fatalf("nested file = %q", got)
	}
	if lockOf(t, target).Files["conf/extra/deep.yaml"] == "" {
		t.Fatal("nested new file not tracked")
	}
}

func TestUpdateKeepsPendingSameVersionJournal(t *testing.T) {
	target := installedV1(t)

	// Craft a dangling journal that is a still-PENDING same-version repair: its
	// baseGeneration equals the on-disk lock's generation (so it belongs to
	// this install and is not yet committed — its finalLock carries a fresh
	// generation). It must be kept, not deleted.
	root, err := OpenSafeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	lock, _, err := ReadLock(root)
	if err != nil {
		t.Fatal(err)
	}
	finalLock := NewLock(lock.Registry, lock.Name, lock.Version, lock.Digest) // fresh generation
	finalLock.Fetched = lock.Fetched
	plan := map[string]PlanEntry{}
	for p, s := range lock.Files {
		finalLock.Files[p] = s
		plan[p] = PlanEntry{Accepted: []string{s}, Final: s}
	}
	phantom := "sha256:" + sha256Of("phantom")
	finalLock.Files["phantom.yaml"] = phantom
	plan["phantom.yaml"] = PlanEntry{Accepted: []string{AbsentState}, Final: phantom}
	j := &Journal{
		APIVersion:     APIVersion,
		Kind:           "RecipeLockPending",
		BaseGeneration: lock.Generation, // belongs to this install → pending
		Target:         JournalTarget{Registry: lock.Registry, Name: lock.Name, Version: lock.Version, Digest: lock.Digest},
		Plan:           plan,
		FinalLock:      finalLock,
	}
	data, err := j.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFileSync(JournalFileName, data, 0o644); err != nil {
		t.Fatal(err)
	}
	root.Close()

	abortAt(t, "stale-cleaned")
	if _, err := updateTo(target, "1.0.0", digestV1, tarballV1(t)); !errors.Is(err, errAbort) {
		t.Fatalf("err = %v, want simulated crash at stale-cleaned", err)
	}
	if !hasResidue(t, target, JournalFileName) {
		t.Fatal("a still-pending same-version journal was wrongly deleted")
	}
}

// TestUpdateRejectsForeignJournal covers a journal copied from a different
// installation lineage: its accepted hashes must NOT drive ownership. The
// generation binding rejects it, so a user edit that happens to match one of
// the foreign journal's accepted states is preserved, not silently overwritten.
func TestUpdateRejectsForeignJournal(t *testing.T) {
	target := installedV1(t)
	userEdit := "MY PRECIOUS LOCAL EDIT\n"
	mutate(t, target, "compose.yaml", userEdit)

	root, err := OpenSafeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	editState, err := EntryState(root, "compose.yaml")
	root.Close()
	if err != nil {
		t.Fatal(err)
	}

	// A well-formed journal from another install: neither its baseGeneration nor
	// its finalLock generation matches this installation's lock. Its plan even
	// "accepts" the user's current edit, so a recovery that did not bind the
	// journal to a generation would treat the edit as vaka-owned.
	foreignFinal := NewLock("official", "demo", "2.0.0", digestV2)
	j := &Journal{
		APIVersion:     APIVersion,
		Kind:           "RecipeLockPending",
		BaseGeneration: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Target:         JournalTarget{Registry: "official", Name: "demo", Version: "2.0.0", Digest: digestV2},
		Plan:           map[string]PlanEntry{"compose.yaml": {Accepted: []string{editState}, Final: editState}},
		FinalLock:      foreignFinal,
	}
	data, err := j.Marshal()
	if err != nil {
		t.Fatalf("marshal foreign journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, JournalFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if err == nil || !strings.Contains(err.Error(), "does not belong to this installation") {
		t.Fatalf("err = %v, want foreign-journal refusal", err)
	}
	if got := readFile(t, target, "compose.yaml"); got != userEdit {
		t.Fatalf("user edit was overwritten: %q", got)
	}
}

func TestUpToDate(t *testing.T) {
	target := installedV1(t)

	ok, err := UpToDate(target, "official", "demo", "1.0.0", digestV1)
	if err != nil || !ok {
		t.Fatalf("pristine matching dir: ok=%v err=%v", ok, err)
	}
	if ok, _ := UpToDate(target, "official", "demo", "1.0.0", digestV2); ok {
		t.Fatal("digest mismatch must not be up to date")
	}
	// Identity guard: a different registry/name with the SAME digest must not
	// short-circuit (it must route through Update, which rejects it).
	if ok, _ := UpToDate(target, "acme", "demo", "1.0.0", digestV1); ok {
		t.Fatal("registry mismatch must not be up to date")
	}
	if ok, _ := UpToDate(target, "official", "other", "1.0.0", digestV1); ok {
		t.Fatal("name mismatch must not be up to date")
	}
	// Version identity: a new index version that reused the installed digest
	// must not short-circuit as current.
	if ok, _ := UpToDate(target, "official", "demo", "2.0.0", digestV1); ok {
		t.Fatal("version mismatch with reused digest must not be up to date")
	}

	mutate(t, target, "compose.yaml", "drifted\n")
	if ok, _ := UpToDate(target, "official", "demo", "1.0.0", digestV1); ok {
		t.Fatal("drifted tracked file must not be up to date")
	}

	if ok, err := UpToDate(t.TempDir(), "official", "demo", "1.0.0", digestV1); err != nil || ok {
		t.Fatalf("lock-less dir: ok=%v err=%v", ok, err)
	}
	if ok, err := UpToDate(filepath.Join(t.TempDir(), "nope"), "official", "demo", "1.0.0", digestV1); err != nil || ok {
		t.Fatalf("missing dir: ok=%v err=%v", ok, err)
	}
}

func TestUpdateInterruptedSameVersionRepairConverges(t *testing.T) {
	target := installedV1(t)
	// Delete a tracked file so a same-version get is a repair (has ops).
	if err := os.Remove(filepath.Join(target, "compose.yaml")); err != nil {
		t.Fatal(err)
	}

	// Interrupt the repair after the file is restored but before commit.
	abortAt(t, "applied compose.yaml")
	if _, err := updateTo(target, "1.0.0", digestV1, tarballV1(t)); !errors.Is(err, errAbort) {
		t.Fatalf("err = %v, want simulated crash", err)
	}
	afterStep = func(string) error { return nil }
	if !hasResidue(t, target, JournalFileName) {
		t.Fatal("setup: expected a dangling journal after the interrupted repair")
	}

	// Re-run the same version. The interrupted repair restored the tree before
	// crashing, so its journal's finalLock already matches the committed lock:
	// the journal is done, not pending, and recovery must CLEAR it (not inherit
	// its stale accepted chain) — never leave it dangling.
	res, err := updateTo(target, "1.0.0", digestV1, tarballV1(t))
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if hasResidue(t, target, JournalFileName) || hasResidue(t, target, StagingDirName) {
		t.Fatal("pending journal/staging not cleared after recovery")
	}
	// A second run is a genuine no-op with nothing left behind.
	res, err = updateTo(target, "1.0.0", digestV1, tarballV1(t))
	if err != nil || !res.NoChange {
		t.Fatalf("second run: res=%+v err=%v (want NoChange)", res, err)
	}
	if hasResidue(t, target, JournalFileName) {
		t.Fatal("second run left a journal behind")
	}
}

// TestUpdateCommittedJournalDoesNotOverwriteLaterEdit covers the case where an
// update commits but crashes before clearing its journal, and the user then
// edits a tracked file back to its pre-update content. The stale journal's
// accepted-state chain must NOT be inherited (it would classify the user's edit
// as vaka-owned and silently replace it); the update must block instead.
func TestUpdateCommittedJournalDoesNotOverwriteLaterEdit(t *testing.T) {
	target := installedV1(t) // compose.yaml at v1

	// Update v1 -> v2, crashing AFTER commit (lock is v2) but BEFORE the
	// journal is cleared. The journal's Plan[compose.yaml].accepted includes
	// the v1 state.
	abortAt(t, "committed")
	if _, err := updateTo(target, "2.0.0", digestV2, tarballV2(t)); !errors.Is(err, errAbort) {
		t.Fatalf("err = %v, want simulated crash after commit", err)
	}
	afterStep = func(string) error { return nil }
	if !hasResidue(t, target, JournalFileName) {
		t.Fatal("setup: expected a surviving journal after the post-commit crash")
	}
	if lockOf(t, target).Version != "2.0.0" {
		t.Fatal("setup: commit should have installed the v2 lock")
	}

	// The user edits compose.yaml back to its v1 content — a deliberate local
	// modification that happens to match the pre-update state.
	mutate(t, target, "compose.yaml", composeV1)

	// Re-run v2. The surviving journal is done (its finalLock == the committed
	// lock), so it must be cleared, and the user's edit to a tracked file must
	// BLOCK rather than be silently overwritten.
	_, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
	var blocked *blockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want blockedError (user edit must not be overwritten)", err)
	}
	if !strings.Contains(err.Error(), "compose.yaml") {
		t.Fatalf("block error should name compose.yaml: %v", err)
	}
	// The user's content is intact.
	if got := readFile(t, target, "compose.yaml"); got != composeV1 {
		t.Fatalf("user edit was overwritten: %q", got)
	}
}

func TestUpdateKeptUserCopyDeviationPersistsAndConverges(t *testing.T) {
	target := installedV1(t)
	// conf/app.yaml is tracked in v1. Modify it, then update to v2 (which
	// drops conf/app.yaml) → kept-user-copy deviation.
	mutate(t, target, "conf/app.yaml", "my edits\n")
	res, err := updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if err != nil {
		t.Fatalf("update to v2: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "user-owned") {
		t.Fatalf("v2 warnings = %v", res.Warnings)
	}
	if devs := lockOf(t, target).Deviations; len(devs) != 1 || devs[0].Kind != DeviationKeptUserCopy {
		t.Fatalf("v2 deviations = %+v", devs)
	}

	// A same-version re-get must NOT drop the deviation: it persists in the
	// lock and the warning repeats while the user's file remains.
	res, err = updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "user-owned") {
		t.Fatalf("re-get dropped the kept-user-copy warning: %v", res.Warnings)
	}
	if devs := lockOf(t, target).Deviations; len(devs) != 1 || devs[0].Kind != DeviationKeptUserCopy {
		t.Fatalf("re-get lost the deviation: %+v", devs)
	}

	// Resolution: remove the kept file → the deviation converges away.
	if err := os.Remove(filepath.Join(target, "conf/app.yaml")); err != nil {
		t.Fatal(err)
	}
	res, err = updateTo(target, "2.0.0", digestV2, tarballV2(t))
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(lockOf(t, target).Deviations) != 0 {
		t.Fatalf("deviation did not converge: %+v", lockOf(t, target).Deviations)
	}
}

func TestUpdateDurabilityOrdering(t *testing.T) {
	target := installedV1(t)

	var synced []string
	old := syncFn
	syncFn = func(f *os.File) error {
		synced = append(synced, f.Name())
		return f.Sync()
	}
	t.Cleanup(func() { syncFn = old })

	if _, err := updateTo(target, "2.0.0", digestV2, tarballV2(t)); err != nil {
		t.Fatal(err)
	}

	idx := func(substr string) int {
		for i, name := range synced {
			if strings.Contains(name, substr) {
				return i
			}
		}
		return -1
	}
	journalIdx := idx(StagingDirName + "/journal")
	lockIdx := idx(StagingDirName + "/lock")
	if journalIdx < 0 || lockIdx < 0 || journalIdx >= lockIdx {
		t.Fatalf("sync order wrong: journal@%d lock@%d in %d syncs", journalIdx, lockIdx, len(synced))
	}
	// The final syncs (post-commit, cleanup) are directory syncs of the root.
	if last := synced[len(synced)-1]; !strings.HasSuffix(last, "demo") && !strings.HasSuffix(last, "demo/.") {
		t.Fatalf("last sync = %s, want the recipe root directory", last)
	}
}

func readFile(t *testing.T, target, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(target, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sha256Of(content string) string {
	root, err := OpenSafeRoot(os.TempDir())
	if err != nil {
		panic(err)
	}
	defer root.Close()
	f, err := os.CreateTemp(os.TempDir(), "hash-*")
	if err != nil {
		panic(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(content)
	f.Close()
	state, err := EntryState(root, filepath.Base(f.Name()))
	if err != nil {
		panic(err)
	}
	return strings.TrimPrefix(state, "sha256:")
}
