// pkg/compose/override.go
package compose

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ServiceEntry holds per-service data needed to build the compose override.
type ServiceEntry struct {
	// Name is the docker-compose service name.
	Name string
	// Entrypoint is the harness's original entrypoint (from image or compose).
	Entrypoint []string
	// Command is the harness's original command (from image or compose).
	Command []string
	// CapDelta is the list of capabilities vaka adds that must later be dropped.
	CapDelta []string
	// EnvVarName is the VAKA_<SERVICE>_CONF env var name for the secret.
	EnvVarName string
}

// secretKey returns the compose secret key for a service name.
// "llm-gateway" → "vaka_llm_gateway_conf"
func secretKey(serviceName string) string {
	return "vaka_" + strings.ReplaceAll(strings.ToLower(serviceName), "-", "_") + "_conf"
}

// composeOverride is the typed struct marshaled to YAML.
type composeOverride struct {
	Secrets  map[string]secretDef       `yaml:"secrets,omitempty"`
	Services map[string]serviceOverride `yaml:"services,omitempty"`
}

type secretDef struct {
	Environment string `yaml:"environment"`
}

type serviceOverride struct {
	Entrypoint []string      `yaml:"entrypoint,omitempty"`
	Command    []string      `yaml:"command,omitempty"`
	CapAdd     []string      `yaml:"cap_add,omitempty"`
	Secrets    []secretMount `yaml:"secrets,omitempty"`
}

type secretMount struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// BuildOverride constructs the compose override YAML string from entries.
// The result is passed to docker compose via stdin (-f -).
func BuildOverride(entries []ServiceEntry) (string, error) {
	override := composeOverride{
		Secrets:  make(map[string]secretDef),
		Services: make(map[string]serviceOverride),
	}

	for _, e := range entries {
		key := secretKey(e.Name)
		override.Secrets[key] = secretDef{Environment: e.EnvVarName}

		// vaka-init replaces the entrypoint; the original entrypoint+command
		// is passed as arguments after "--".
		cmd := append(e.Entrypoint, e.Command...)

		svc := serviceOverride{
			Entrypoint: []string{"vaka-init", "--"},
			Command:    cmd,
			CapAdd:     e.CapDelta,
			Secrets: []secretMount{{
				Source: key,
				Target: "vaka.yaml",
			}},
		}
		override.Services[e.Name] = svc
	}

	data, err := yaml.Marshal(override)
	if err != nil {
		return "", fmt.Errorf("marshal compose override: %w", err)
	}
	return string(data), nil
}
