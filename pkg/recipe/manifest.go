package recipe

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Manifest is the recipe.yaml model (design §3). The client parses and
// schema-validates it on the staged tree so a tarball whose manifest is
// malformed, mis-typed, or disagrees with the resolved index entry is refused
// before commit — not merely present. The rules mirror the registry CI
// (scripts/validate_recipe.py) so a recipe that passes publication also
// installs, and vice versa.
type Manifest struct {
	APIVersion     string        `yaml:"apiVersion"`
	Kind           string        `yaml:"kind"`
	Name           string        `yaml:"name"`
	Version        string        `yaml:"version"`
	Description    string        `yaml:"description"`
	Homepage       string        `yaml:"homepage"`
	Tags           []string      `yaml:"tags"`
	MinVakaVersion string        `yaml:"minVakaVersion"`
	Env            []ManifestEnv `yaml:"env"`
}

// ManifestEnv documents one compose interpolation input (documentation-grade
// metadata, not a template language — design §3).
type ManifestEnv struct {
	Name        string `yaml:"name"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

const (
	manifestAPIVersion = "recipes.vaka/v1alpha1"
	manifestKind       = "Recipe"
	// maxManifestBytes bounds the recipe.yaml read (a small document).
	maxManifestBytes = 1 << 20
)

// strictSemverRE matches strict X.Y.Z, exactly the registry CI's SEMVER_RE (no
// leading v, no prerelease/build metadata) so client and CI agree on which
// versions are legal.
var strictSemverRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

var recipeNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// Manifest schema key sets, mirroring the CI validator. riskAcknowledgements
// is allowed (CI consumes it) though the client does not model it; provides
// and requires are reserved for future composability and rejected until
// specified.
var (
	allowedManifestKeys = map[string]bool{
		"apiVersion": true, "kind": true, "name": true, "version": true,
		"description": true, "homepage": true, "tags": true,
		"minVakaVersion": true, "env": true, "riskAcknowledgements": true,
	}
	reservedManifestKeys = map[string]bool{"provides": true, "requires": true}
)

// ExpectedIdentity is the name/version the resolved index entry promised. When
// set, the staged manifest must match it, so a tarball cannot masquerade as a
// different recipe or version than the one the index (and digest) resolved.
type ExpectedIdentity struct {
	Name    string
	Version string
}

// ParseManifest parses and schema-validates a recipe.yaml document. It does
// not check identity; use Manifest.CheckIdentity for that.
func ParseManifest(data []byte) (*Manifest, error) {
	var top any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("recipe.yaml: not valid YAML: %w", err)
	}
	raw, ok := top.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("recipe.yaml: manifest must be a YAML mapping")
	}

	var problems []string
	for k := range raw {
		switch {
		case reservedManifestKeys[k]:
			problems = append(problems, fmt.Sprintf("field %q is reserved for future recipe composability and is not yet supported", k))
		case !allowedManifestKeys[k]:
			problems = append(problems, fmt.Sprintf("unknown field %q", k))
		}
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("recipe.yaml: %w", err)
	}
	if m.APIVersion != manifestAPIVersion {
		problems = append(problems, fmt.Sprintf("apiVersion must be %q", manifestAPIVersion))
	}
	if m.Kind != manifestKind {
		problems = append(problems, fmt.Sprintf("kind must be %q", manifestKind))
	}
	if !recipeNameRE.MatchString(m.Name) {
		problems = append(problems, "name must match [a-z0-9-]+")
	}
	if !strictSemverRE.MatchString(m.Version) {
		problems = append(problems, fmt.Sprintf("version %q must be strict SemVer (X.Y.Z)", m.Version))
	}
	if strings.TrimSpace(m.Description) == "" {
		problems = append(problems, "description is required and must be non-empty")
	}
	if m.MinVakaVersion != "" && !strictSemverRE.MatchString(m.MinVakaVersion) {
		problems = append(problems, fmt.Sprintf("minVakaVersion %q must be strict SemVer (X.Y.Z)", m.MinVakaVersion))
	}
	for i, e := range m.Env {
		if e.Name == "" {
			problems = append(problems, fmt.Sprintf("env[%d]: each entry needs a string 'name'", i))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("recipe.yaml is invalid:\n\t%s", strings.Join(problems, "\n\t"))
	}
	return &m, nil
}

// CheckIdentity verifies the manifest agrees with the resolved index entry.
// Empty fields in want are not checked (identity is optional at parse-only
// call sites).
func (m *Manifest) CheckIdentity(want ExpectedIdentity) error {
	var problems []string
	if want.Name != "" && m.Name != want.Name {
		problems = append(problems, fmt.Sprintf("manifest name %q does not match the resolved recipe %q", m.Name, want.Name))
	}
	if want.Version != "" && m.Version != want.Version {
		problems = append(problems, fmt.Sprintf("manifest version %q does not match the resolved version %q", m.Version, want.Version))
	}
	if len(problems) > 0 {
		return fmt.Errorf("recipe.yaml identity mismatch (the tarball does not match the index entry it was resolved from):\n\t%s",
			strings.Join(problems, "\n\t"))
	}
	return nil
}
