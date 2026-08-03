// Package registry implements vaka recipe registry consumption: configuration,
// published index fetching with ETag caching, explicit Git preview catalog
// generation, and recipe reference resolution.
//
// Format and security model: docs/design/recipes-registry.md.
package registry

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the schema version shared by all registry documents.
const APIVersion = "recipes.vaka/v1alpha1"

// Official is the built-in default registry, used when no registries
// configuration file exists.
var Official = Registry{
	Name: "official",
	URL:  "https://infrasecture.github.io/vaka-registry/index.yaml",
}

// nameRE constrains registry and recipe names; it keeps the
// `registry/name@version` reference grammar unambiguous.
var nameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// GitSource identifies a mutable Git ref used as an explicitly configured
// development/preview registry. A refresh resolves Ref to one immutable commit
// before any recipe content is read.
type GitSource struct {
	URL string `yaml:"url"`
	Ref string `yaml:"ref"`
}

// Registry is one configured registry. Exactly one source must be set: URL is
// a published index.yaml, while Git is an opt-in preview source.
type Registry struct {
	Name string     `yaml:"name"`
	URL  string     `yaml:"url,omitempty"`
	Git  *GitSource `yaml:"git,omitempty"`
}

// IsGit reports whether this registry is backed by a mutable Git ref.
func (r Registry) IsGit() bool { return r.Git != nil }

// SourceDescription returns the configured source in a human-readable form.
func (r Registry) SourceDescription() string {
	if r.Git != nil {
		return fmt.Sprintf("git %s#%s", r.Git.URL, r.Git.Ref)
	}
	return r.URL
}

// sourceIdentity binds cached data to the complete configured source. It is
// deliberately not a fetch URL: changing either a Git URL or ref invalidates
// the old cache just as changing an index URL does.
func (r Registry) sourceIdentity() string {
	if r.Git != nil {
		return "git\x00" + r.Git.URL + "\x00" + r.Git.Ref
	}
	return "index\x00" + r.URL
}

// Config is the registries configuration document (kind: RegistriesConfig).
type Config struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Registries []Registry `yaml:"registries"`
}

// DefaultConfig returns the built-in configuration: the official registry.
func DefaultConfig() *Config {
	return &Config{
		APIVersion: APIVersion,
		Kind:       "RegistriesConfig",
		Registries: []Registry{Official},
	}
}

// ConfigPath returns the registries configuration file path.
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "vaka", "registries.yaml"), nil
}

// LoadConfig loads the registries configuration from the default path.
// A missing file yields the built-in default (official registry only).
func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	return LoadConfigFrom(path)
}

// LoadConfigFrom loads and validates a registries configuration file.
// vaka owns this document, so unknown fields are rejected.
func LoadConfigFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registries config: %w", err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != "RegistriesConfig" {
		return fmt.Errorf("kind must be %q, got %q", "RegistriesConfig", c.Kind)
	}
	seen := map[string]bool{}
	for _, r := range c.Registries {
		if seen[r.Name] {
			return fmt.Errorf("duplicate registry name %q", r.Name)
		}
		seen[r.Name] = true
		if err := validateRegistry(r); err != nil {
			return fmt.Errorf("registry %q: %w", r.Name, err)
		}
	}
	return nil
}

// Lookup returns the configured registry with the given name.
func (c *Config) Lookup(name string) (Registry, bool) {
	for _, r := range c.Registries {
		if r.Name == name {
			return r, true
		}
	}
	return Registry{}, false
}

// Add appends a registry, validating its name and source and rejecting a
// duplicate name.
func (c *Config) Add(reg Registry) error {
	if err := validateRegistry(reg); err != nil {
		return err
	}
	if _, ok := c.Lookup(reg.Name); ok {
		return fmt.Errorf("registry %q is already configured", reg.Name)
	}
	c.Registries = append(c.Registries, reg)
	return nil
}

func validateRegistry(reg Registry) error {
	if !nameRE.MatchString(reg.Name) {
		return fmt.Errorf("registry name %q must match [a-z0-9-]+", reg.Name)
	}
	if (reg.URL == "") == (reg.Git == nil) {
		return fmt.Errorf("exactly one registry source is required: url or git")
	}
	if reg.URL != "" {
		return validateIndexURL(reg.URL)
	}
	if err := validateGitURL(reg.Git.URL); err != nil {
		return err
	}
	return validateGitRef(reg.Git.Ref)
}

var scpGitURLRE = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._~][A-Za-z0-9._~/-]*$`)

// validateGitURL limits Git previews to transports with understood security
// properties. In particular, git://, plain HTTP, custom remote helpers, and
// credentials embedded in HTTPS URLs are refused.
func validateGitURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("git URL is required")
	}
	if scpGitURLRE.MatchString(raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid git URL %q: %w", raw, err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("git URL %q must not contain a query or fragment", raw)
	}
	switch u.Scheme {
	case "https":
		if u.User != nil {
			return fmt.Errorf("git HTTPS URL %q must not embed credentials; use the Git credential helper", raw)
		}
		if u.Host == "" || u.Path == "" {
			return fmt.Errorf("git HTTPS URL %q must include a host and repository path", raw)
		}
	case "ssh":
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				return fmt.Errorf("git SSH URL %q must not embed a password", raw)
			}
		}
		if u.Host == "" || u.Path == "" {
			return fmt.Errorf("git SSH URL %q must include a host and repository path", raw)
		}
	case "file":
		if u.User != nil || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) {
			return fmt.Errorf("git file URL %q must be an absolute local file:// URL", raw)
		}
	case "http":
		return fmt.Errorf("plain HTTP git URLs are not allowed (%q); use HTTPS or SSH", raw)
	default:
		return fmt.Errorf("unsupported git URL %q (HTTPS, SSH, scp-style SSH, and file:// are allowed)", raw)
	}
	return nil
}

// validateGitRef accepts ordinary branch/tag names and full refs while
// rejecting option-like or ambiguous revision syntax. The ref is passed to
// `git fetch` as data and is resolved to FETCH_HEAD^{commit}.
func validateGitRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("git ref is required")
	}
	unsafeWhitespace := false
	for _, r := range ref {
		if r <= 0x20 || r == 0x7f {
			unsafeWhitespace = true
			break
		}
	}
	if len(ref) > 255 || ref == "@" || unsafeWhitespace ||
		strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, ".") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.Contains(ref, "//") || strings.ContainsAny(ref, "~^:?*[\\") {
		return fmt.Errorf("git ref %q is not a safe branch, tag, or full ref name", ref)
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("git ref %q is not a safe branch, tag, or full ref name", ref)
		}
	}
	return nil
}

// Remove deletes the named registry, erroring if it is not configured.
func (c *Config) Remove(name string) error {
	for i, r := range c.Registries {
		if r.Name == name {
			c.Registries = append(c.Registries[:i], c.Registries[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("registry %q is not configured", name)
}

// SaveConfig writes the configuration to the default path, creating the
// parent directory. The config's identity fields are set if empty.
func SaveConfig(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return SaveConfigTo(path, cfg)
}

// SaveConfigTo writes the configuration to path atomically (temp file +
// rename), after validating it.
func SaveConfigTo(path string, cfg *Config) error {
	if cfg.APIVersion == "" {
		cfg.APIVersion = APIVersion
	}
	if cfg.Kind == "" {
		cfg.Kind = "RegistriesConfig"
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".registries-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
