// Package registry implements read-only consumption of vaka recipe
// registries: the registries configuration, the published index catalog,
// fetching with ETag caching, and recipe reference resolution.
//
// Format and security model: docs/design/recipes-registry.md.
package registry

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

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

// Registry is one configured registry: a name and the URL of its index.yaml.
type Registry struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
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
		if !nameRE.MatchString(r.Name) {
			return fmt.Errorf("registry name %q must match [a-z0-9-]+", r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate registry name %q", r.Name)
		}
		seen[r.Name] = true
		if err := validateIndexURL(r.URL); err != nil {
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
