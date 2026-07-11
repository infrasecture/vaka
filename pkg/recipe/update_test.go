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
	policyV1  = "apiVersion: agent.vaka/v1alpha1\n"
	newFileV2 = "added in v2\n"
)

func demoTarball(t *testing.T, compose string, withConf, withNewFile bool) string {
	t.Helper()
	entries := []tarEntry{
		{name: "demo/", typeflag: tar.TypeDir},
		{name: "demo/compose.yaml", typeflag: tar.TypeReg, content: compose},
		{name: "demo/docker-compose.yaml", typeflag: tar.TypeSymlink, linkname: "compose.yaml"},
		{name: "demo/vaka.yaml", typeflag: tar.TypeReg, content: policyV1},
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

func TestUpdateRequiresLock(t *testing.T) {
	dir := t.TempDir()
	_, err := updateTo(dir, "2.0.0", digestV2, tarballV2(t))
	if err == nil || !strings.Contains(err.Error(), "not a vaka-managed recipe directory") {
		t.Fatalf("err = %v", err)
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
