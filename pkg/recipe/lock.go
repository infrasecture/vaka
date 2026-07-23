package recipe

import (
	"bytes"
	"fmt"
	"reflect"
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
	lockDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	entryStateRE = regexp.MustCompile(`^(sha256:[0-9a-f]{64}(\+x)?|link:.+)$`)
)

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
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Registry   string            `yaml:"registry"`
	Name       string            `yaml:"name"`
	Version    string            `yaml:"version"`
	Digest     string            `yaml:"digest"`
	Fetched    string            `yaml:"fetched"`
	Files      map[string]string `yaml:"files"`
	Deviations []Deviation       `yaml:"deviations,omitempty"`
}

// sameInstall reports whether two locks describe the same installed state:
// equal identity, digest, tracked files, and deviations. The Fetched timestamp
// is ignored, since it differs between the lock written at commit and any
// re-derived lock for the same content. It is used to detect a journal whose
// finalLock has already been committed (so the accepted-state chain must reset
// rather than be inherited).
func (l *Lock) sameInstall(other *Lock) bool {
	if l == nil || other == nil {
		return false
	}
	return l.Registry == other.Registry &&
		l.Name == other.Name &&
		l.Version == other.Version &&
		l.Digest == other.Digest &&
		reflect.DeepEqual(l.Files, other.Files) &&
		reflect.DeepEqual(l.Deviations, other.Deviations)
}

// NewLock returns a lock skeleton with identity fields set and the fetch
// time stamped.
func NewLock(registryName, name, version, digest string) *Lock {
	return &Lock{
		APIVersion: APIVersion,
		Kind:       "RecipeLock",
		Registry:   registryName,
		Name:       name,
		Version:    version,
		Digest:     digest,
		Fetched:    time.Now().UTC().Format(time.RFC3339),
		Files:      map[string]string{},
	}
}

// ParseLock strictly decodes and validates a RecipeLock document.
func ParseLock(data []byte) (*Lock, error) {
	var l Lock
	if err := strictDecode(data, &l); err != nil {
		return nil, fmt.Errorf("recipe lock: %w", err)
	}
	if err := l.validate(); err != nil {
		return nil, fmt.Errorf("recipe lock: %w", err)
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
		return nil, true, err
	}
	return l, true, nil
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
