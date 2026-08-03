package registry

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// Ref is a parsed recipe reference: [registry/]name[@version].
type Ref struct {
	Registry string // empty: resolve across all configured registries
	Name     string
	Version  string // empty: highest indexed version
}

// ParseRef parses `[registry/]name[@version]`. It accepts exact versions
// only; constraint expressions (e.g. `^1.2`, `>=0.2`) are rejected explicitly
// (a deliberately deferred feature).
func ParseRef(s string) (Ref, error) {
	var ref Ref
	rest := s

	if i := strings.IndexByte(rest, '@'); i >= 0 {
		version := rest[i+1:]
		rest = rest[:i]
		if strings.ContainsAny(version, "@") {
			return ref, fmt.Errorf("invalid reference %q: more than one '@'", s)
		}
		if _, err := semver.StrictNewVersion(version); err != nil {
			if strings.ContainsAny(version, "^~><=*xX |,") {
				return ref, fmt.Errorf(
					"invalid version %q in %q: version constraints are not supported; use an exact version like 1.2.3", version, s)
			}
			return ref, fmt.Errorf("invalid version %q in %q: must be exact SemVer (X.Y.Z)", version, s)
		}
		ref.Version = version
	}

	if i := strings.IndexByte(rest, '/'); i >= 0 {
		ref.Registry = rest[:i]
		rest = rest[i+1:]
		if strings.ContainsAny(rest, "/") {
			return ref, fmt.Errorf("invalid reference %q: more than one '/'", s)
		}
		if !nameRE.MatchString(ref.Registry) {
			return ref, fmt.Errorf("invalid registry name %q in %q: must match [a-z0-9-]+", ref.Registry, s)
		}
	}

	ref.Name = rest
	if !nameRE.MatchString(ref.Name) {
		return ref, fmt.Errorf("invalid recipe name %q in %q: must match [a-z0-9-]+", ref.Name, s)
	}
	return ref, nil
}

// Resolved is the outcome of resolving a Ref against configured registries.
type Resolved struct {
	Registry Registry
	Name     string
	Entry    IndexEntry
}

// Resolve finds the recipe a Ref denotes. Unqualified names resolve only
// when they are unique across all configured registries (anti-typosquatting:
// adding a registry can never silently hijack a name already in use).
func Resolve(cfg *Config, indexes map[string]*Index, ref Ref) (*Resolved, error) {
	if ref.Registry != "" {
		reg, ok := cfg.Lookup(ref.Registry)
		if !ok {
			return nil, fmt.Errorf("registry %q is not configured", ref.Registry)
		}
		idx, ok := indexes[ref.Registry]
		if !ok {
			return nil, fmt.Errorf("registry %q: index unavailable", ref.Registry)
		}
		versions, ok := idx.Recipes[ref.Name]
		if !ok {
			return nil, fmt.Errorf("recipe %q not found in registry %q", ref.Name, ref.Registry)
		}
		entry, err := selectVersion(ref, versions)
		if err != nil {
			return nil, err
		}
		selected := *entry
		// sourceRevision is local Git-preview provenance, not an extension a
		// published index can inject into user output or an installation lock.
		if !reg.IsGit() {
			selected.SourceRevision = ""
		}
		return &Resolved{Registry: reg, Name: ref.Name, Entry: selected}, nil
	}

	var candidates, previewCandidates []string
	for _, reg := range cfg.Registries {
		idx, ok := indexes[reg.Name]
		if !ok {
			continue
		}
		if _, ok := idx.Recipes[ref.Name]; ok {
			if reg.IsGit() {
				previewCandidates = append(previewCandidates, reg.Name)
				continue
			}
			candidates = append(candidates, reg.Name)
		}
	}
	sort.Strings(candidates)
	switch len(candidates) {
	case 0:
		if len(previewCandidates) > 0 {
			sort.Strings(previewCandidates)
			qualified := make([]string, len(previewCandidates))
			for i, candidate := range previewCandidates {
				qualified[i] = candidate + "/" + ref.Name
			}
			return nil, fmt.Errorf(
				"recipe %q exists only in a Git preview registry; preview recipes must be qualified: %s",
				ref.Name, strings.Join(qualified, ", "))
		}
		return nil, fmt.Errorf("recipe %q not found in any configured registry", ref.Name)
	case 1:
		qualified := ref
		qualified.Registry = candidates[0]
		return Resolve(cfg, indexes, qualified)
	default:
		qualifiedRefs := make([]string, len(candidates))
		for i, c := range candidates {
			qualifiedRefs[i] = c + "/" + ref.Name
		}
		return nil, fmt.Errorf(
			"recipe name %q exists in more than one configured registry; qualify it: %s",
			ref.Name, strings.Join(qualifiedRefs, ", "))
	}
}

// selectVersion picks the exact requested version, or the highest indexed
// one when the ref carries no version.
func selectVersion(ref Ref, versions []IndexEntry) (*IndexEntry, error) {
	if ref.Version != "" {
		for i := range versions {
			if versions[i].Version == ref.Version {
				return &versions[i], nil
			}
		}
		return nil, fmt.Errorf("recipe %q has no indexed version %s (published indexes may keep only recent versions; older releases remain downloadable from the registry's release page)", ref.Name, ref.Version)
	}

	var best *IndexEntry
	var bestVer *semver.Version
	for i := range versions {
		v, err := semver.StrictNewVersion(versions[i].Version)
		if err != nil {
			continue // tolerate malformed entries from foreign registries
		}
		if bestVer == nil || v.GreaterThan(bestVer) {
			best, bestVer = &versions[i], v
		}
	}
	if best == nil {
		return nil, fmt.Errorf("recipe %q has no parseable indexed versions", ref.Name)
	}
	return best, nil
}
