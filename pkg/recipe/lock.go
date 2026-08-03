package recipe

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// APIVersion is the schema version shared by all recipe documents.
const APIVersion = "recipes.vaka/v1alpha1"

// Reserved vaka state paths inside a recipe directory (design §6).
const (
	LockFileName       = ".vaka-recipe.lock"
	JournalFileName    = ".vaka-recipe.lock.new"
	UpdateLockFileName = ".vaka-recipe.update.lock"
	StagingDirName     = ".vaka-staging"
)

var (
	lockDigestRE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	sourceRevisionRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	entryStateRE     = regexp.MustCompile(`^(sha256:[0-9a-f]{64}(\+x)?|link:.+)$`)
	generationRE     = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// newGeneration returns a fresh 128-bit random installation-generation nonce.
// Every committed lock carries one so that an update journal can be bound to
// the exact lock it was created from (base) and the lock it produces (final):
// a copied or stale journal from another installation lineage matches neither
// and is refused, instead of contributing its accepted hashes to ownership
// classification.
func newGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is not recoverable for a security nonce.
		panic("recipe: cannot read random bytes for lock generation: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// DeviationKind classifies how an installed directory diverges from the
// published recipe (design §6 decision matrix).
type DeviationKind string

const (
	// DeviationSkippedCollision: a file the recipe ships was not installed
	// because an untracked user file already occupied its path.
	DeviationSkippedCollision DeviationKind = "skipped-collision"
	// DeviationKeptUserCopy: a locally modified tracked file that upstream
	// dropped was kept and untracked.
	DeviationKeptUserCopy DeviationKind = "kept-user-copy"
)

// Deviation records one divergence from the published recipe.
type Deviation struct {
	Path string        `yaml:"path"`
	Kind DeviationKind `yaml:"kind"`
}

// Lock is the provenance record written into every instantiated recipe
// directory (kind: RecipeLock). vaka owns this document: strict decoding.
type Lock struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Registry   string `yaml:"registry"`
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Digest     string `yaml:"digest"`
	// SourceRevision is the immutable Git commit that produced a preview
	// artifact. Published registry installs leave it empty.
	SourceRevision string `yaml:"sourceRevision,omitempty"`
	// Generation is a per-commit random nonce identifying this exact installed
	// state, so an update journal can be bound to the lock it belongs to.
	Generation string            `yaml:"generation"`
	Fetched    string            `yaml:"fetched"`
	Files      map[string]string `yaml:"files"`
	Deviations []Deviation       `yaml:"deviations,omitempty"`
}

// NewLock returns a lock skeleton with identity fields set, a fresh generation
// nonce, and the fetch time stamped.
func NewLock(registryName, name, version, digest string) *Lock {
	return &Lock{
		APIVersion: APIVersion,
		Kind:       "RecipeLock",
		Registry:   registryName,
		Name:       name,
		Version:    version,
		Digest:     digest,
		Generation: newGeneration(),
		Fetched:    time.Now().UTC().Format(time.RFC3339),
		Files:      map[string]string{},
	}
}

// ParseLock strictly decodes and validates a RecipeLock document. It returns
// the bare defect (which field, and whether missing or malformed); the file/
// directory context and the recovery guidance are added by ReadLock, which
// knows where the document came from.
func ParseLock(data []byte) (*Lock, error) {
	var l Lock
	if err := strictDecode(data, &l); err != nil {
		return nil, err
	}
	if err := l.validate(); err != nil {
		return nil, err
	}
	return &l, nil
}

func (l *Lock) validate() error {
	if l.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, l.APIVersion)
	}
	if l.Kind != "RecipeLock" {
		return fmt.Errorf("kind must be %q, got %q", "RecipeLock", l.Kind)
	}
	if l.Registry == "" || l.Name == "" || l.Version == "" {
		return fmt.Errorf("registry, name, and version are required")
	}
	if !lockDigestRE.MatchString(l.Digest) {
		return fmt.Errorf("digest %q is not sha256:<64 hex>", l.Digest)
	}
	if l.SourceRevision != "" && !sourceRevisionRE.MatchString(l.SourceRevision) {
		return fmt.Errorf("sourceRevision %q is not a full Git commit ID", l.SourceRevision)
	}
	if l.Generation == "" {
		// Distinguish the legacy case (field absent → written by a vaka from
		// before the generation nonce existed) from a malformed value, since the
		// user's situation and the phrasing differ.
		return fmt.Errorf("missing the generation field")
	}
	if !generationRE.MatchString(l.Generation) {
		return fmt.Errorf("generation %q is not 32 hex characters", l.Generation)
	}
	for p, state := range l.Files {
		if err := validRecipePath(p); err != nil {
			return fmt.Errorf("file key: %w", err)
		}
		if !entryStateRE.MatchString(state) {
			return fmt.Errorf("file %q has malformed state %q", p, state)
		}
	}
	for _, d := range l.Deviations {
		if err := validRecipePath(d.Path); err != nil {
			return fmt.Errorf("deviation path: %w", err)
		}
		if d.Kind != DeviationSkippedCollision && d.Kind != DeviationKeptUserCopy {
			return fmt.Errorf("deviation %q has unknown kind %q", d.Path, d.Kind)
		}
	}
	return nil
}

// Marshal serializes the lock.
func (l *Lock) Marshal() ([]byte, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	return yaml.Marshal(l)
}

// maxLockBytes caps lock/journal reads. These documents are small (one line
// per file) and may live in untrusted directories, so the read is bounded.
const maxLockBytes = 4 << 20

// ReadLock reads the lock from a recipe directory. exists is false when the
// directory has no lock (i.e. it is not a vaka-managed recipe directory).
func ReadLock(root *SafeRoot) (l *Lock, exists bool, err error) {
	data, err := root.ReadFileLimited(LockFileName, maxLockBytes)
	if err != nil {
		if isNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	l, err = ParseLock(data)
	if err != nil {
		// The lock exists but cannot be used. Give the user the whole picture:
		// which directory, the specific defect (from ParseLock), the likely
		// cause, and how to recover. This single boundary covers every reader —
		// LockForDir, UpToDate, Update, and the render deviation notice.
		return nil, true, fmt.Errorf(
			"the recipe lock in %s is unusable: %w\n"+
				"It is stale or was written by an incompatible vaka version (or was "+
				"edited/corrupted), so vaka will not update this directory. Reinstall "+
				"the recipe into a fresh directory, or restore the original %s, then re-run.",
			dirLabel(root.Name()), err, LockFileName)
	}
	return l, true, nil
}

// dirLabel renders a directory for user-facing messages.
func dirLabel(dir string) string {
	if dir == "" || dir == "." {
		return "the current directory"
	}
	return dir
}

// LockForDir reads the recipe lock of an on-disk directory through a
// confinement root with a bounded read — the safe way to inspect the lock of
// a possibly untrusted project directory (symlinks are not followed, size is
// capped). exists is false when the directory has no lock.
func LockForDir(dir string) (l *Lock, exists bool, err error) {
	root, err := OpenSafeRoot(dir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	return ReadLock(root)
}

func strictDecode(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(v)
}
