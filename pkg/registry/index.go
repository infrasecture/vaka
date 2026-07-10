package registry

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Index is a registry's published catalog (kind: RegistryIndex).
//
// Unlike documents vaka owns, the index is decoded leniently: registries may
// be newer than this client and carry fields we do not know yet.
type Index struct {
	APIVersion string                  `yaml:"apiVersion"`
	Kind       string                  `yaml:"kind"`
	Generated  string                  `yaml:"generated"`
	Recipes    map[string][]IndexEntry `yaml:"recipes"`
}

// IndexEntry is one published version of one recipe.
type IndexEntry struct {
	Version        string         `yaml:"version"`
	Description    string         `yaml:"description"`
	Tags           []string       `yaml:"tags"`
	Created        string         `yaml:"created"`
	Digest         string         `yaml:"digest"`
	URLs           []string       `yaml:"urls"`
	MinVakaVersion string         `yaml:"minVakaVersion"`
	Env            []EnvVar       `yaml:"env"`
	Policy         *PolicySummary `yaml:"policy"`
}

// EnvVar documents one compose interpolation input of a recipe.
type EnvVar struct {
	Name        string `yaml:"name"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

// PolicySummary is the advisory policy digest computed by registry CI.
// vaka always recomputes it locally after fetching; this copy only makes
// search and info useful before downloading.
type PolicySummary struct {
	DefaultActions map[string]string `yaml:"defaultActions"`
	RiskFlags      []string          `yaml:"riskFlags"`
}

// ParseIndex decodes and minimally validates a registry index document.
func ParseIndex(data []byte) (*Index, error) {
	var idx Index
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	if idx.APIVersion != APIVersion {
		return nil, fmt.Errorf("index apiVersion must be %q, got %q", APIVersion, idx.APIVersion)
	}
	if idx.Kind != "RegistryIndex" {
		return nil, fmt.Errorf("index kind must be %q, got %q", "RegistryIndex", idx.Kind)
	}
	if idx.Recipes == nil {
		idx.Recipes = map[string][]IndexEntry{}
	}
	return &idx, nil
}
