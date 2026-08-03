package recipe

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// writeTestTarball materializes the goodRecipe archive as a file.
func writeTestTarball(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "demo-1.0.0.tar.gz")
	if err := os.WriteFile(path, goodRecipe(t).Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func installSpec(t *testing.T, target string) InstallSpec {
	t.Helper()
	return InstallSpec{
		Registry:    "official",
		Name:        "demo",
		Version:     "1.0.0",
		Digest:      testDigest,
		TarballPath: writeTestTarball(t),
		Target:      target,
	}
}

func listParent(t *testing.T, parent string) []string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestInstallHappyPath(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "demo")
	spec := installSpec(t, target)
	spec.SourceRevision = strings.Repeat("a", 40)
	lock, err := Install(spec)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// The lock returned matches the lock on disk, strictly parseable.
	root, err := OpenSafeRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	onDisk, exists, err := ReadLock(root)
	if err != nil || !exists {
		t.Fatalf("ReadLock: exists=%v err=%v", exists, err)
	}
	if onDisk.Registry != "official" || onDisk.Name != "demo" ||
		onDisk.Version != "1.0.0" || onDisk.Digest != testDigest ||
		onDisk.SourceRevision != spec.SourceRevision {
		t.Fatalf("lock identity = %+v", onDisk)
	}
	if len(onDisk.Deviations) != 0 {
		t.Fatalf("fresh install must have no deviations, got %+v", onDisk.Deviations)
	}

	// Every extracted file is tracked with the right state shape.
	wantStates := map[string]string{
		"recipe.yaml":         "sha256:",
		"compose.yaml":        "sha256:",
		"docker-compose.yaml": "link:compose.yaml",
		"vaka.yaml":           "sha256:",
		"run.sh":              "+x",
		"conf/app.yaml":       "sha256:",
	}
	if len(onDisk.Files) != len(wantStates) {
		t.Fatalf("tracked files = %v", onDisk.Files)
	}
	for path, want := range wantStates {
		state, ok := onDisk.Files[path]
		if !ok {
			t.Fatalf("lock does not track %s (files: %v)", path, onDisk.Files)
		}
		if want == "link:compose.yaml" && state != want {
			t.Fatalf("%s state = %q, want %q", path, state, want)
		}
		if !strings.Contains(state, strings.TrimSuffix(want, ":")) {
			t.Fatalf("%s state = %q, want it to contain %q", path, state, want)
		}
	}
	if lock.Files["run.sh"] != onDisk.Files["run.sh"] {
		t.Fatal("returned lock and on-disk lock differ")
	}

	// No staging residue: the parent holds exactly the target.
	if names := listParent(t, parent); len(names) != 1 || names[0] != "demo" {
		t.Fatalf("parent contains %v, want [demo]", names)
	}
}

func TestInstallRefusesExistingTargets(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, target string)
	}{
		{"existing file", func(t *testing.T, target string) {
			if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"existing non-empty dir", func(t *testing.T, target string) {
			if err := os.MkdirAll(filepath.Join(target, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"existing empty dir", func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling symlink", func(t *testing.T, target string) {
			if err := os.Symlink("nowhere", target); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "demo")
			tc.setup(t, target)
			before := listParent(t, parent)

			_, err := Install(installSpec(t, target))
			if err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if !strings.Contains(err.Error(), "never adopts or writes into an existing path") {
				t.Fatalf("error %q missing refusal wording", err.Error())
			}
			after := listParent(t, parent)
			if strings.Join(before, ",") != strings.Join(after, ",") {
				t.Fatalf("refusal left residue: before=%v after=%v", before, after)
			}
		})
	}
}

func TestInstallCommitIsNoReplaceEvenWhenRaced(t *testing.T) {
	// Simulate the race the early Lstat cannot see: something appears at
	// target between the check and the rename. The commit itself must
	// refuse to replace it — even an empty directory.
	parent := t.TempDir()
	staging := filepath.Join(parent, ".vaka-get-race")
	target := filepath.Join(parent, "demo")
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil { // planted empty dir
		t.Fatal(err)
	}

	err := commitNewDir(staging, target)
	if err == nil || !strings.Contains(err.Error(), "never adopts or writes into an existing path") {
		t.Fatalf("err = %v, want no-replace refusal", err)
	}
	// The planted directory is untouched and staging still exists.
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("planted target vanished: %v", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("staging vanished: %v", err)
	}
}

func TestInstallFailureLeavesNothing(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "demo")

	// An adversarial tarball fails extraction mid-way.
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	buf := makeTarGz(t, []tarEntry{
		{name: "demo/ok", typeflag: tar.TypeReg, content: "fine"},
		{name: "demo/.vaka-recipe.lock", typeflag: tar.TypeReg, content: "forged"},
	})
	if err := os.WriteFile(bad, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := installSpec(t, target)
	spec.TarballPath = bad
	if _, err := Install(spec); err == nil {
		t.Fatal("expected extraction failure")
	}
	if names := listParent(t, parent); len(names) != 0 {
		t.Fatalf("failed install left residue: %v", names)
	}
}

func TestParseLockStrictness(t *testing.T) {
	valid := `apiVersion: recipes.vaka/v1alpha1
kind: RecipeLock
registry: official
name: demo
version: 1.0.0
digest: ` + testDigest + `
generation: 0123456789abcdef0123456789abcdef
fetched: "2026-07-11T00:00:00Z"
files:
  compose.yaml: sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
  run.sh: sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824+x
  docker-compose.yaml: link:compose.yaml
deviations:
  - {path: litellm.yaml, kind: skipped-collision}
`
	if _, err := ParseLock([]byte(valid)); err != nil {
		t.Fatalf("valid lock rejected: %v", err)
	}

	rejects := []struct{ name, doc, want string }{
		{"unknown field", valid + "surprise: true\n", "surprise"},
		{"bad generation", strings.Replace(valid, "0123456789abcdef0123456789abcdef", "xyz", 1), "generation"},
		{"wrong kind", strings.Replace(valid, "RecipeLock", "Recipe", 1), "kind must be"},
		{"bad digest", strings.Replace(valid, testDigest, "sha256:zz", 1), "sha256:<64 hex>"},
		{"bad source revision", strings.Replace(valid, "digest: "+testDigest, "digest: "+testDigest+"\nsourceRevision: branch-name", 1), "full Git commit ID"},
		{"bad state", strings.Replace(valid, "link:compose.yaml", "md5:nope", 1), "malformed state"},
		{"bad deviation kind", strings.Replace(valid, "skipped-collision", "merged", 1), "unknown kind"},
		// File/deviation path keys must be canonical relative paths outside .vaka-*.
		{"reserved file key", strings.Replace(valid, "compose.yaml:", ".vaka-recipe.update.lock:", 1), "reserved"},
		{"absolute file key", strings.Replace(valid, "compose.yaml:", "/etc/passwd:", 1), "must be relative"},
		{"escaping file key", strings.Replace(valid, "compose.yaml:", "../../etc/x:", 1), "escapes"},
		{"reserved deviation path", strings.Replace(valid, "litellm.yaml", ".vaka-staging", 1), "reserved"},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseLock([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestReadLockActionableError checks that a lock which exists but cannot be used
// — the common case being a lock written by an older vaka, before the
// generation nonce — surfaces an error a user can act on: the directory, the
// specific defect, the likely cause, and how to recover. (Regression for the
// bare `generation "" is not 32 hex chars`.)
func TestReadLockActionableError(t *testing.T) {
	dir := t.TempDir()
	// A lock exactly as an older vaka wrote it: valid except no generation.
	legacy := "apiVersion: recipes.vaka/v1alpha1\nkind: RecipeLock\n" +
		"registry: official\nname: codex\nversion: 0.1.0\ndigest: " + testDigest + "\n" +
		"fetched: \"2026-07-11T00:00:00Z\"\nfiles: {}\n"
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	_, exists, err := LockForDir(dir)
	if !exists || err == nil {
		t.Fatalf("exists=%v err=%v (want a lock that exists but errors)", exists, err)
	}
	msg := err.Error()
	for _, want := range []string{dir, "missing the generation field", "incompatible vaka", "Reinstall", LockFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q; got:\n%s", want, msg)
		}
	}

	// A malformed (non-empty) generation is phrased as a different situation.
	bad := strings.Replace(legacy, "files: {}", "generation: nothex\nfiles: {}", 1)
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LockForDir(dir); err == nil || !strings.Contains(err.Error(), "not 32 hex") {
		t.Fatalf("malformed generation err = %v", err)
	}
}

// TestReadJournalActionableError: an unreadable dangling journal (e.g. written
// by an older vaka, no baseGeneration) explains itself and how to recover.
func TestReadJournalActionableError(t *testing.T) {
	dir := t.TempDir()
	j := "apiVersion: recipes.vaka/v1alpha1\nkind: RecipeLockPending\n" +
		"target:\n  registry: official\n  name: codex\n  version: 0.1.0\n  digest: " + testDigest + "\n" +
		"plan: {}\nfinalLock:\n  apiVersion: recipes.vaka/v1alpha1\n  kind: RecipeLock\n" +
		"  registry: official\n  name: codex\n  version: 0.1.0\n  digest: " + testDigest + "\n" +
		"  generation: ffffffffffffffffffffffffffffffff\n  fetched: \"2026-07-11T00:00:00Z\"\n  files: {}\n"
	if err := os.WriteFile(filepath.Join(dir, JournalFileName), []byte(j), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := OpenSafeRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, exists, err := ReadJournal(root)
	if !exists || err == nil {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	msg := err.Error()
	for _, want := range []string{dir, "missing the baseGeneration field", "interrupted update", JournalFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("journal error lacks %q; got:\n%s", want, msg)
		}
	}
}

func TestJournalRoundTripAndStrictness(t *testing.T) {
	final, err := ParseLock([]byte(`apiVersion: recipes.vaka/v1alpha1
kind: RecipeLock
registry: official
name: demo
version: 2.0.0
digest: ` + testDigest + `
generation: ffffffffffffffffffffffffffffffff
fetched: "2026-07-11T00:00:00Z"
files:
  compose.yaml: sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
`))
	if err != nil {
		t.Fatal(err)
	}
	j := &Journal{
		APIVersion:     APIVersion,
		Kind:           "RecipeLockPending",
		BaseGeneration: "00000000000000000000000000000000",
		Target: JournalTarget{
			Registry: "official", Name: "demo", Version: "2.0.0", Digest: testDigest,
			URLs: []string{"https://example.com/demo-2.0.0.tar.gz"},
		},
		Plan: map[string]PlanEntry{
			"compose.yaml": {
				Accepted: []string{
					"sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
					AbsentState,
				},
				Final: "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
			},
			"legacy.sh": {
				Accepted: []string{"sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824+x"},
				Final:    AbsentState,
			},
		},
		FinalLock: final,
	}

	data, err := j.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := ParseJournal(data)
	if err != nil {
		t.Fatalf("ParseJournal: %v", err)
	}
	if back.Plan["legacy.sh"].Final != AbsentState || back.FinalLock.Version != "2.0.0" {
		t.Fatalf("round trip lost data: %+v", back)
	}

	if _, err := ParseJournal(append(data, []byte("extra: field\n")...)); err == nil {
		t.Fatal("journal accepted an unknown field")
	}
	j.FinalLock = nil
	if _, err := j.Marshal(); err == nil || !strings.Contains(err.Error(), "finalLock is required") {
		t.Fatalf("err = %v, want finalLock requirement", err)
	}
}

// A journal whose Plan names vaka's own reserved state (e.g. the held update
// lock) must be refused — otherwise a corrupt-but-valid-looking journal could
// classify the lock as deletable and unlink it mid-update.
func TestJournalRejectsReservedPlanPath(t *testing.T) {
	final, err := ParseLock([]byte(`apiVersion: recipes.vaka/v1alpha1
kind: RecipeLock
registry: official
name: demo
version: 1.0.0
digest: ` + testDigest + `
generation: ffffffffffffffffffffffffffffffff
fetched: "2026-07-11T00:00:00Z"
files:
  compose.yaml: sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
`))
	if err != nil {
		t.Fatal(err)
	}
	j := &Journal{
		APIVersion:     APIVersion,
		Kind:           "RecipeLockPending",
		BaseGeneration: "00000000000000000000000000000000",
		Target:         JournalTarget{Registry: "official", Name: "demo", Version: "1.0.0", Digest: testDigest},
		Plan: map[string]PlanEntry{
			UpdateLockFileName: {Accepted: []string{AbsentState}, Final: AbsentState},
		},
		FinalLock: final,
	}
	if _, err := j.Marshal(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want reserved-path refusal for %q", err, UpdateLockFileName)
	}
}
