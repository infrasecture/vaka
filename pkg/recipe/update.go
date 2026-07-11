package recipe

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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

// Update applies the §6 journaled update transaction to spec.Target.
func Update(spec UpdateSpec) (*UpdateResult, error) {
	root, err := OpenSafeRoot(spec.Target)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	lock, exists, err := ReadLock(root)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%s has no %s; it is not a vaka-managed recipe directory", spec.Target, LockFileName)
	}

	// Step 1 — single updater: flock on the dedicated, never-replaced,
	// never-deleted lock file (flock binds to the inode; the data lock is
	// replaced at commit and cannot serve as the exclusion point).
	unlock, err := acquireUpdateLock(root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := afterStep("locked"); err != nil {
		return nil, err
	}

	// Step 2 — stale journal and staging cleanup.
	journal, hasJournal, err := ReadJournal(root)
	if err != nil {
		return nil, err
	}
	if hasJournal && journal.Target.Version == lock.Version && journal.Target.Digest == lock.Digest {
		// The previous update committed but crashed before cleanup.
		if err := root.Remove(JournalFileName); err != nil {
			return nil, err
		}
		journal, hasJournal = nil, false
	}
	if err := root.RemoveAll(StagingDirName); err != nil {
		return nil, err
	}
	if err := root.SyncDir("."); err != nil {
		return nil, err
	}
	if err := afterStep("stale-cleaned"); err != nil {
		return nil, err
	}

	// Stage the incoming version so its states are known before anything
	// in the visible tree is touched.
	if err := root.MkdirAll(path.Join(StagingDirName, "new"), 0o755); err != nil {
		return nil, err
	}
	tarball, err := os.Open(spec.TarballPath)
	if err != nil {
		return nil, err
	}
	err = ExtractRecipe(tarball, spec.Name, root.Name()+"/"+StagingDirName+"/new")
	tarball.Close()
	if err != nil {
		return nil, err
	}
	newRoot, err := OpenSafeRoot(root.Name() + "/" + StagingDirName + "/new")
	if err != nil {
		return nil, err
	}
	defer newRoot.Close()
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
	if err := afterStep("prechecked"); err != nil {
		return nil, err
	}

	finalLock := NewLock(spec.Registry, spec.Name, spec.Version, spec.Digest)
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

	// Idempotent no-op: same version, same digest, nothing to write.
	if !hasOps && lock.Version == spec.Version && lock.Digest == spec.Digest &&
		locksEquivalent(lock, finalLock) {
		if err := root.RemoveAll(StagingDirName); err != nil {
			return nil, err
		}
		return &UpdateResult{Lock: lock, Warnings: warnings, NoChange: true}, nil
	}

	// Step 4 — journal first, durably, before any visible-tree mutation.
	j := &Journal{
		APIVersion: APIVersion,
		Kind:       "RecipeLockPending",
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
			dirsToSync[path.Dir(p)] = true
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
					row.warning = fmt.Sprintf("%s: no longer shipped by the recipe; your modified copy is now user-owned", p)
				}
			}

		default: // untracked by lock and journal
			switch {
			case !inNew:
				// Not in the universe unless tracked or in new; unreachable.
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
