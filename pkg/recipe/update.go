package recipe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// afterStep is the interruption seam: tests set it to abort the transaction
// at a step boundary, simulating a crash (no cleanup runs). Step names:
// "locked", "stale-cleaned", "prechecked", "journal-written",
// "applied <path>", "deleted <path>", "committed", "cleaned".
var afterStep = func(step string) error { return nil }

// UpdateSpec describes one update of an existing recipe directory to a
// fetched, digest-verified tarball.
type UpdateSpec struct {
	Registry    string
	Name        string
	Version     string
	Digest      string
	TarballPath string
	Target      string // existing recipe directory containing a lock
	VakaVersion string // running vaka version, for the manifest minVakaVersion check
	// SourceRevision is optional Git preview provenance.
	SourceRevision string
}

// UpdateResult reports what an update did.
type UpdateResult struct {
	Lock *Lock
	// Warnings are the §6 matrix-row notices (reinstalls, kept user copies,
	// skipped collisions). They repeat on every update until resolved.
	Warnings []string
	// NoChange is true when the directory already matches the requested
	// version exactly and nothing was written.
	NoChange bool
}

// ErrUpdateInProgress is returned when another vaka process holds the
// update lock of the target directory.
var ErrUpdateInProgress = errors.New("another vaka get is updating this directory")

// blockedError is the single blocking case of the §6 decision matrix.
type blockedError struct{ paths []string }

func (e *blockedError) Error() string {
	return fmt.Sprintf(
		"update rejected: locally modified tracked file(s) would be replaced and vaka does not merge:\n\t%s\nkeep customizations in untracked files (.env, compose.override.yaml, files the recipe does not ship); there is no override flag",
		strings.Join(e.paths, "\n\t"))
}

// obstructionError reports recipe paths whose parent directory has been
// replaced by a non-directory, so the recipe file cannot be installed.
type obstructionError struct{ paths []string }

func (e *obstructionError) Error() string {
	return fmt.Sprintf(
		"update rejected: a parent directory of recipe file(s) has been replaced by a non-directory:\n\t%s\nrestore the directory (or move your file aside) and re-run vaka get",
		strings.Join(e.paths, "\n\t"))
}

// planAction is what the update will do to one path.
type planAction int

const (
	actNone planAction = iota
	actPut             // install or replace from the staged new tree
	actDelete
	actTrackOnly // content already correct; only the lock records it
)

type planRow struct {
	action    planAction
	tracked   bool // present in the final lock
	accepted  []string
	final     string // entry state or AbsentState
	deviation DeviationKind
	warning   string
}

// UpToDate reports whether the recipe directory at target is already exactly
// this registry/name/digest and fully pristine, so `vaka get` can skip the
// download and update entirely. It is conservative: any doubt (missing lock,
// identity mismatch, digest mismatch, dangling journal, recorded deviation,
// drifted or missing tracked file) returns false, and the caller falls
// through to the authoritative Update.
//
// Like Update, it takes the update lock before reading anything, honoring the
// "lock before anything is read" / "concurrent get fails fast" contract: a
// concurrent updater holding the lock makes this return ErrUpdateInProgress
// (which the caller surfaces) rather than reporting a snapshot that the other
// updater is about to invalidate. It never mutates.
func UpToDate(target, registryName, name, version, digest string) (bool, error) {
	root, err := OpenSafeRoot(target)
	if err != nil {
		if isNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer root.Close()

	// Cheap recipe-directory gate before locking (mirrors Update): a
	// non-recipe directory is simply not up to date, and we do not create
	// vaka state in it.
	if _, err := root.Lstat(LockFileName); err != nil {
		if isNotExist(err) {
			return false, nil
		}
		return false, err
	}
	unlock, err := acquireUpdateLock(root)
	if err != nil {
		return false, err
	}
	defer unlock()

	lock, exists, err := ReadLock(root)
	if err != nil || !exists {
		return false, err
	}
	// Identity must match, so the fast path never bypasses the registry/name
	// guard that Update enforces (a different recipe with a colliding digest
	// must still be routed through Update, which rejects it).
	if lock.Registry != registryName || lock.Name != name {
		return false, nil
	}
	// Version identity too: a new index version reusing a prior digest must not
	// short-circuit as "current" — it has to route through Update so the lock
	// records the requested version and staged manifest validation runs.
	if lock.Version != version || lock.Digest != digest || len(lock.Deviations) > 0 {
		return false, nil
	}
	if _, hasJournal, err := ReadJournal(root); err != nil || hasJournal {
		return false, err
	}
	for p, want := range lock.Files {
		got, absent, err := diskStateOf(root, p)
		if err != nil {
			return false, err
		}
		if absent || got != want {
			return false, nil
		}
	}
	return true, nil
}

// Update applies the §6 journaled update transaction to spec.Target.
func Update(spec UpdateSpec) (*UpdateResult, error) {
	root, err := OpenSafeRoot(spec.Target)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	// A recipe directory is one that carries a lock. Gate on its presence
	// with a cheap stat for a friendly error before taking the update lock;
	// the authoritative read happens under the lock below.
	if _, err := root.Lstat(LockFileName); err != nil {
		if isNotExist(err) {
			return nil, fmt.Errorf("%s has no %s; it is not a vaka-managed recipe directory", spec.Target, LockFileName)
		}
		return nil, err
	}

	// Step 1 — single updater: flock on the dedicated, never-replaced,
	// never-deleted lock file (flock binds to the inode; the data lock is
	// replaced at commit and cannot serve as the exclusion point). Held from
	// before every authoritative read through cleanup.
	unlock, err := acquireUpdateLock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := afterStep("locked"); err != nil {
		return nil, err
	}

	// Read the committed lock only now, under the update lock: a concurrent
	// updater that committed while we waited for the lock must not leave us
	// planning the decision matrix against a stale pre-lock snapshot.
	lock, exists, err := ReadLock(root)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%s has no %s; it is not a vaka-managed recipe directory", spec.Target, LockFileName)
	}
	if lock.Name != spec.Name || lock.Registry != spec.Registry {
		return nil, fmt.Errorf(
			"%s holds %s/%s, not %s/%s; refusing to update a different recipe in place",
			spec.Target, lock.Registry, lock.Name, spec.Registry, spec.Name)
	}

	// Step 2 — recover any interrupted prior update, then clear staging.
	//
	// A surviving journal is bound to a lock generation on both ends: its
	// baseGeneration is the lock it started from, and its finalLock carries the
	// generation it produces. Relative to the installed lock:
	//   - finalLock generation matches  → the prior update committed but crashed
	//     before cleanup: it is DONE. Clear it WITHOUT inheriting its now-stale
	//     accepted chain (which would let a user edit made after the commit be
	//     silently overwritten — the accepted chain resets at the first commit,
	//     design §6).
	//   - baseGeneration matches         → a genuine pending update from this
	//     exact lock: honor it (its accepted chain is inherited below and this
	//     transaction re-commits and clears it).
	//   - neither matches                → the journal belongs to a different
	//     installation lineage (stale, copied, or forged). Refuse to act on it,
	//     rather than let its accepted hashes drive ownership classification.
	journal, _, err := ReadJournal(root)
	if err != nil {
		return nil, err
	}
	if journal != nil {
		switch {
		case journal.FinalLock.Generation == lock.Generation:
			if err := root.Remove(JournalFileName); err != nil {
				return nil, err
			}
			if err := root.SyncDir("."); err != nil {
				return nil, err
			}
			journal = nil
		case journal.BaseGeneration == lock.Generation:
			// keep: pending update from this lock
		default:
			return nil, fmt.Errorf(
				"%s: the update journal %s does not belong to this installation (generation mismatch); it is stale, copied from another install, or corrupt — remove it and re-run vaka get",
				spec.Target, JournalFileName)
		}
	}
	pendingJournal := journal != nil
	if err := root.RemoveAll(StagingDirName); err != nil {
		return nil, err
	}
	if err := root.SyncDir("."); err != nil {
		return nil, err
	}
	if err := afterStep("stale-cleaned"); err != nil {
		return nil, err
	}

	// Stage the incoming version so its states are known before anything in
	// the visible tree is touched. The staging root is nested under the held
	// root, so extraction and its renames stay within the confinement we
	// already own — no path is reconstructed or re-resolved from the
	// filesystem root.
	if err := root.MkdirAll(path.Join(StagingDirName, "new"), 0o755); err != nil {
		return nil, err
	}
	newRoot, err := root.OpenRoot(path.Join(StagingDirName, "new"))
	if err != nil {
		return nil, err
	}
	defer newRoot.Close()
	tarball, err := os.Open(spec.TarballPath)
	if err != nil {
		return nil, err
	}
	err = ExtractRecipe(tarball, spec.Name, newRoot)
	tarball.Close()
	if err != nil {
		return nil, err
	}
	newStates := map[string]string{}
	if err := newRoot.WalkFiles(func(p string, _ fs.DirEntry) error {
		state, err := EntryState(newRoot, p)
		if err != nil {
			return err
		}
		newStates[p] = state
		return nil
	}); err != nil {
		return nil, err
	}
	if err := syncTree(newRoot); err != nil {
		return nil, err
	}

	// Step 3 — pre-check: classify every path per the decision matrix.
	rows, blocked, err := buildPlan(root, lock, journal, newStates)
	if err != nil {
		return nil, err
	}
	if len(blocked) > 0 {
		sort.Strings(blocked)
		return nil, &blockedError{paths: blocked}
	}
	if obstructed := obstructedParents(root, rows); len(obstructed) > 0 {
		return nil, &obstructionError{paths: obstructed}
	}

	// Validate the staged new version immediately before it can be committed:
	// a malformed or policy-invalid published version must never replace a
	// working recipe (fail closed). Placed as late as possible — right before
	// the journal write — to minimize the window between validation and apply.
	//
	// Residual (bounded, documented in the design's §6 filesystem note):
	// compose-go loads by path, so validation re-resolves the staged path
	// rather than reading through the newRoot descriptor; a concurrent
	// non-vaka writer swapping vaka's reserved staging during this window
	// could make validation and apply disagree. The damage is confined to the
	// recipe directory (os.Root cannot escape) and requires racing the
	// flock-holding updater. Closing it fully needs descriptor-relative,
	// no-follow traversal (openat2), which is not yet implemented.
	if err := ValidateStaged(context.Background(), filepath.Join(spec.Target, StagingDirName, "new"),
		ExpectedIdentity{Name: spec.Name, Version: spec.Version, VakaVersion: spec.VakaVersion}); err != nil {
		return nil, err
	}
	if err := afterStep("prechecked"); err != nil {
		return nil, err
	}

	finalLock := NewLock(spec.Registry, spec.Name, spec.Version, spec.Digest)
	finalLock.SourceRevision = spec.SourceRevision
	var warnings []string
	hasOps := false
	paths := sortedKeys(rows)
	for _, p := range paths {
		row := rows[p]
		if row.tracked {
			finalLock.Files[p] = newStates[p]
		}
		if row.deviation != "" {
			finalLock.Deviations = append(finalLock.Deviations, Deviation{Path: p, Kind: row.deviation})
		}
		if row.warning != "" {
			warnings = append(warnings, row.warning)
		}
		if row.action == actPut || row.action == actDelete {
			hasOps = true
		}
	}

	// Idempotent no-op: same version, same digest, nothing to write — and no
	// pending journal to commit and clear.
	if !hasOps && !pendingJournal && lock.Version == spec.Version && lock.Digest == spec.Digest &&
		locksEquivalent(lock, finalLock) {
		if err := root.RemoveAll(StagingDirName); err != nil {
			return nil, err
		}
		return &UpdateResult{Lock: lock, Warnings: warnings, NoChange: true}, nil
	}

	// Step 4 — journal first, durably, before any visible-tree mutation.
	j := &Journal{
		APIVersion:     APIVersion,
		Kind:           "RecipeLockPending",
		BaseGeneration: lock.Generation, // the lock this update is based on
		Target: JournalTarget{
			Registry: spec.Registry, Name: spec.Name,
			Version: spec.Version, Digest: spec.Digest,
		},
		Plan:      map[string]PlanEntry{},
		FinalLock: finalLock,
	}
	for _, p := range paths {
		row := rows[p]
		if row.action == actNone && !row.tracked {
			continue
		}
		j.Plan[p] = PlanEntry{Accepted: row.accepted, Final: row.final}
	}
	jdata, err := j.Marshal()
	if err != nil {
		return nil, err
	}
	if err := root.WriteFileSync(path.Join(StagingDirName, "journal"), jdata, 0o644); err != nil {
		return nil, err
	}
	if err := root.Rename(path.Join(StagingDirName, "journal"), JournalFileName); err != nil {
		return nil, err
	}
	if err := root.SyncDir("."); err != nil {
		return nil, err
	}
	if err := afterStep("journal-written"); err != nil {
		return nil, err
	}

	// Step 5 — apply the matrix: staged renames and deletions.
	dirsToSync := map[string]bool{".": true}
	for _, p := range paths {
		row := rows[p]
		switch row.action {
		case actPut:
			if dir := path.Dir(p); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
			}
			if err := root.Rename(path.Join(StagingDirName, "new", p), p); err != nil {
				return nil, err
			}
			// Sync the target's directory and every ancestor MkdirAll may
			// have newly created, so a nested new subtree is durable before
			// the commit rename (not just the file's immediate parent).
			addDirAndParents(dirsToSync, path.Dir(p))
			if err := afterStep("applied " + p); err != nil {
				return nil, err
			}
		case actDelete:
			if err := root.Remove(p); err != nil {
				return nil, err
			}
			dirsToSync[path.Dir(p)] = true
			if err := afterStep("deleted " + p); err != nil {
				return nil, err
			}
		}
	}
	for _, dir := range sortedKeys(dirsToSync) {
		if _, err := root.Lstat(dir); err != nil {
			continue
		}
		if err := root.SyncDir(dir); err != nil {
			return nil, err
		}
	}

	// Step 6 — commit: install the canonical final lock via rename.
	lockData, err := finalLock.Marshal()
	if err != nil {
		return nil, err
	}
	if err := root.WriteFileSync(path.Join(StagingDirName, "lock"), lockData, 0o644); err != nil {
		return nil, err
	}
	if err := root.Rename(path.Join(StagingDirName, "lock"), LockFileName); err != nil {
		return nil, err
	}
	if err := root.SyncDir("."); err != nil {
		return nil, err
	}
	if err := afterStep("committed"); err != nil {
		return nil, err
	}

	// Step 7 — cleanup.
	if err := root.Remove(JournalFileName); err != nil {
		return nil, err
	}
	if err := root.RemoveAll(StagingDirName); err != nil {
		return nil, err
	}
	if err := root.SyncDir("."); err != nil {
		return nil, err
	}
	if err := afterStep("cleaned"); err != nil {
		return nil, err
	}

	return &UpdateResult{Lock: finalLock, Warnings: warnings}, nil
}

// buildPlan classifies every relevant path per the §6 decision matrix.
func buildPlan(root *SafeRoot, lock *Lock, journal *Journal, newStates map[string]string) (map[string]planRow, []string, error) {
	// Prior kept-user-copy deviations must be reconsidered every update, or
	// they silently vanish from the rebuilt lock (their paths are untracked
	// and no longer shipped, so they are in neither lock.Files nor newStates).
	priorKeptUserCopy := map[string]bool{}
	for _, d := range lock.Deviations {
		if d.Kind == DeviationKeptUserCopy {
			priorKeptUserCopy[d.Path] = true
		}
	}

	universe := map[string]bool{}
	for p := range lock.Files {
		universe[p] = true
	}
	for p := range newStates {
		universe[p] = true
	}
	if journal != nil {
		for p := range journal.Plan {
			universe[p] = true
		}
	}
	for p := range priorKeptUserCopy {
		universe[p] = true
	}

	rows := map[string]planRow{}
	var blocked []string
	for p := range universe {
		disk, absent, err := diskStateOf(root, p)
		if err != nil {
			return nil, nil, err
		}
		lockState, tracked := lock.Files[p]
		newState, inNew := newStates[p]

		accepted := map[string]bool{}
		if tracked {
			accepted[lockState] = true
		}
		if journal != nil {
			if je, ok := journal.Plan[p]; ok {
				for _, a := range je.Accepted {
					accepted[a] = true
				}
				accepted[je.Final] = true
			}
		}
		if absent {
			accepted[AbsentState] = true
		}

		vakaOwned := !absent && (accepted[disk] || disk == newState)
		row := planRow{accepted: acceptedList(accepted, newState), final: AbsentState}
		if inNew {
			row.final = newState
		}

		switch {
		case tracked || (journal != nil && journalKnows(journal, p)):
			switch {
			case absent && inNew:
				row.action, row.tracked = actPut, true
				if tracked {
					row.warning = fmt.Sprintf("%s: locally deleted tracked file was restored from the recipe", p)
				}
			case absent && !inNew:
				// Dropped upstream and already gone: nothing to do.
			case vakaOwned && inNew:
				if disk == newState {
					row.action, row.tracked = actTrackOnly, true
				} else {
					row.action, row.tracked = actPut, true
				}
			case vakaOwned && !inNew:
				row.action = actDelete
			case !vakaOwned && inNew:
				if tracked {
					blocked = append(blocked, p) // the single blocking case
				} else {
					// User content at a path only the journal knew.
					row.deviation = DeviationSkippedCollision
					row.warning = collisionWarning(p)
				}
			case !vakaOwned && !inNew:
				if tracked {
					row.deviation = DeviationKeptUserCopy
					row.warning = keptUserCopyWarning(p)
				}
			}

		default: // untracked by lock and journal
			switch {
			case !inNew:
				// Only reachable for a prior kept-user-copy deviation (added
				// to the universe above). While the user's file is still
				// present and still not shipped, carry the deviation forward
				// so warnings and the render notice persist until resolved;
				// once the user removes it (absent), it converges away.
				if !absent && priorKeptUserCopy[p] {
					row.deviation = DeviationKeptUserCopy
					row.warning = keptUserCopyWarning(p)
				}
			case absent:
				row.action, row.tracked = actPut, true
			case disk == newState:
				// Byte-identical to what would be installed: adopting it is
				// indistinguishable from installing it, and converges.
				row.action, row.tracked = actTrackOnly, true
			default:
				row.deviation = DeviationSkippedCollision
				row.warning = collisionWarning(p)
			}
		}
		rows[p] = row
	}
	return rows, blocked, nil
}

func collisionWarning(p string) string {
	return fmt.Sprintf("%s: kept your file; the recipe's version was not installed (rename or remove it and re-run vaka get to receive it)", p)
}

func keptUserCopyWarning(p string) string {
	return fmt.Sprintf("%s: no longer shipped by the recipe; your modified copy is now user-owned", p)
}

// obstructedParents reports actPut targets whose nearest existing ancestor
// directory is not a directory (the user replaced it with a file or
// symlink). Such a put would fail with ENOTDIR mid-apply, after the journal
// is written — catching it in the pre-check refuses the update cleanly with
// nothing written and no wedged journal.
func obstructedParents(root *SafeRoot, rows map[string]planRow) []string {
	var bad []string
	for p, row := range rows {
		if row.action != actPut {
			continue
		}
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			fi, err := root.Lstat(dir)
			if err == nil {
				if !fi.IsDir() {
					bad = append(bad, p)
				}
				break // the nearest existing ancestor decides
			}
			if !isNotExist(err) && !errors.Is(err, unix.ENOTDIR) {
				break // unexpected; let the apply surface it
			}
		}
	}
	sort.Strings(bad)
	return bad
}

// addDirAndParents adds dir and every ancestor up to (but excluding) "." to
// the set, so newly created intermediate directories are all fsynced.
func addDirAndParents(set map[string]bool, dir string) {
	for dir != "." && dir != "" {
		set[dir] = true
		dir = path.Dir(dir)
	}
}

func journalKnows(j *Journal, p string) bool {
	_, ok := j.Plan[p]
	return ok
}

// diskStateOf returns the entry state of p, or absent=true when nothing
// (reachable) exists there. Unsupported types (directories where a file is
// expected, sockets, ...) yield a sentinel state that matches nothing.
func diskStateOf(root *SafeRoot, p string) (state string, absent bool, err error) {
	fi, err := root.Lstat(p)
	if err != nil {
		if isNotExist(err) || errors.Is(err, unix.ENOTDIR) {
			return "", true, nil
		}
		return "", false, err
	}
	if fi.Mode()&fs.ModeSymlink == 0 && !fi.Mode().IsRegular() {
		return "other:" + fi.Mode().Type().String(), false, nil
	}
	s, err := EntryState(root, p)
	if err != nil {
		return "", false, err
	}
	return s, false, nil
}

func acquireUpdateLock(root *SafeRoot) (func(), error) {
	// Refuse a symlinked lock path: os.Root follows in-tree symlinks during
	// resolution (O_NOFOLLOW is not honored), so a symlink planted at the
	// lock path would otherwise redirect the flock to another inode and
	// defeat the stable-lock-file exclusion. The lock must be a regular file
	// (or absent, created below).
	if fi, err := root.Lstat(UpdateLockFileName); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to lock through it", UpdateLockFileName)
	} else if err != nil && !isNotExist(err) {
		return nil, err
	}
	f, err := root.r.OpenFile(UpdateLockFileName, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrUpdateInProgress
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

func acceptedList(set map[string]bool, newState string) []string {
	if newState != "" {
		set[newState] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func locksEquivalent(a, b *Lock) bool {
	if len(a.Files) != len(b.Files) || len(a.Deviations) != len(b.Deviations) {
		return false
	}
	for p, s := range a.Files {
		if b.Files[p] != s {
			return false
		}
	}
	da := append([]Deviation{}, a.Deviations...)
	db := append([]Deviation{}, b.Deviations...)
	sort.Slice(da, func(i, j int) bool { return da[i].Path < da[j].Path })
	sort.Slice(db, func(i, j int) bool { return db[i].Path < db[j].Path })
	for i := range da {
		if da[i] != db[i] {
			return false
		}
	}
	return true
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
