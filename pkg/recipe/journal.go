package recipe

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// AbsentState is the Plan sentinel for "this path must not exist after the
// update" (dropped upstream) and, inside Accepted, "absence is an accepted
// prior state".
const AbsentState = "absent"

// JournalTarget identifies the version an interrupted update was applying.
type JournalTarget struct {
	Registry string   `yaml:"registry"`
	Name     string   `yaml:"name"`
	Version  string   `yaml:"version"`
	Digest   string   `yaml:"digest"`
	URLs     []string `yaml:"urls,omitempty"`
}

// PlanEntry is the per-path update plan: which prior states may be replaced
// (accepted-state chain, design §6) and what the path becomes.
type PlanEntry struct {
	Accepted []string `yaml:"accepted"`
	Final    string   `yaml:"final"` // an entry state, or AbsentState
}

// Journal is the update journal (kind: RecipeLockPending): an envelope of
// the operation plan plus the canonical final lock installed verbatim at
// commit. It is written before any tree mutation and removed after commit.
type Journal struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	// BaseGeneration is the generation nonce of the committed lock this update
	// started from. On recovery it must equal the installed lock's generation
	// (a genuine pending update) — otherwise the journal is stale, copied from
	// another installation, or forged, and must not steer ownership.
	BaseGeneration string               `yaml:"baseGeneration"`
	Target         JournalTarget        `yaml:"target"`
	Plan           map[string]PlanEntry `yaml:"plan"`
	FinalLock      *Lock                `yaml:"finalLock"`
}

// ParseJournal strictly decodes and validates a RecipeLockPending document.
func ParseJournal(data []byte) (*Journal, error) {
	var j Journal
	if err := strictDecode(data, &j); err != nil {
		return nil, fmt.Errorf("update journal: %w", err)
	}
	if err := j.validate(); err != nil {
		return nil, fmt.Errorf("update journal: %w", err)
	}
	return &j, nil
}

func (j *Journal) validate() error {
	if j.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, j.APIVersion)
	}
	if j.Kind != "RecipeLockPending" {
		return fmt.Errorf("kind must be %q, got %q", "RecipeLockPending", j.Kind)
	}
	if !lockDigestRE.MatchString(j.Target.Digest) {
		return fmt.Errorf("target digest %q is not sha256:<64 hex>", j.Target.Digest)
	}
	for p, entry := range j.Plan {
		if err := validRecipePath(p); err != nil {
			return fmt.Errorf("plan key: %w", err)
		}
		if entry.Final != AbsentState && !entryStateRE.MatchString(entry.Final) {
			return fmt.Errorf("plan %q has malformed final state %q", p, entry.Final)
		}
		for _, a := range entry.Accepted {
			if a != AbsentState && !entryStateRE.MatchString(a) {
				return fmt.Errorf("plan %q has malformed accepted state %q", p, a)
			}
		}
	}
	if j.FinalLock == nil {
		return fmt.Errorf("finalLock is required")
	}
	if err := j.FinalLock.validate(); err != nil {
		return fmt.Errorf("finalLock: %w", err)
	}
	if !generationRE.MatchString(j.BaseGeneration) {
		return fmt.Errorf("baseGeneration %q is not 32 hex chars", j.BaseGeneration)
	}
	// A journal always transitions between two distinct generations; equal base
	// and final would let a forged journal claim to be both pending and
	// completed for one lock.
	if j.BaseGeneration == j.FinalLock.Generation {
		return fmt.Errorf("baseGeneration must differ from finalLock generation")
	}
	// The envelope's Target must name the same install as the embedded
	// finalLock, so the two cannot disagree about what is being produced.
	if j.Target.Registry != j.FinalLock.Registry || j.Target.Name != j.FinalLock.Name ||
		j.Target.Version != j.FinalLock.Version || j.Target.Digest != j.FinalLock.Digest {
		return fmt.Errorf("target does not match finalLock identity")
	}
	// Every file the finalLock records must have a matching plan final state,
	// tying the plan to the lock it commits.
	for p, state := range j.FinalLock.Files {
		if pe, ok := j.Plan[p]; !ok || pe.Final != state {
			return fmt.Errorf("plan for %q is inconsistent with finalLock", p)
		}
	}
	return nil
}

// Marshal serializes the journal.
func (j *Journal) Marshal() ([]byte, error) {
	if err := j.validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(j)
}

// ReadJournal reads a dangling update journal from a recipe directory.
// exists is false when no journal is present.
func ReadJournal(root *SafeRoot) (j *Journal, exists bool, err error) {
	data, err := root.ReadFileLimited(JournalFileName, maxLockBytes)
	if err != nil {
		if isNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	j, err = ParseJournal(data)
	if err != nil {
		return nil, true, err
	}
	return j, true, nil
}
