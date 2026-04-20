// pkg/compose/override.go
package compose

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const vakaInitServiceName = "__vaka-init"
const vakaInitPath = "/opt/vaka/sbin/vaka-init"

// vakaInitNotAttached is the `attach: false` pointer used on __vaka-init.
// With attach=false, compose does not stream __vaka-init's logs and does not
// wait on it for foreground exit — so `vaka up` preserves the same UX as a
// plain `docker compose up` on the user's own services (no spurious foreground
// exit when the short-lived helper completes).
var vakaInitNotAttached = func() *bool { b := false; return &b }()

// ServiceEntry holds per-service data needed to build the compose override.
type ServiceEntry struct {
	Name       string
	Entrypoint []string
	Command    []string
	CapDelta   []string
	EnvVarName string
	// OptOut is true when the service carries the agent.vaka.init: present label,
	// meaning vaka-init is already baked into the image at /opt/vaka/sbin/.
	OptOut bool
}

// secretKey returns the compose secret key for a service name.
// "llm-gateway" → "vaka_llm_gateway_conf"
func secretKey(serviceName string) string {
	return "vaka_" + strings.ReplaceAll(strings.ToLower(serviceName), "-", "_") + "_conf"
}

type composeOverride struct {
	Secrets  map[string]secretDef       `yaml:"secrets,omitempty"`
	Services map[string]serviceOverride `yaml:"services,omitempty"`
}

type secretDef struct {
	Environment string `yaml:"environment"`
}

type serviceOverride struct {
	Image       string             `yaml:"image,omitempty"`
	User        string             `yaml:"user,omitempty"`
	Entrypoint  []string           `yaml:"entrypoint,omitempty"`
	Command     []string           `yaml:"command,omitempty"`
	CapAdd      []string           `yaml:"cap_add,omitempty"`
	Secrets     []secretMount      `yaml:"secrets,omitempty"`
	VolumesFrom []string           `yaml:"volumes_from,omitempty"`
	DependsOn   map[string]depCond `yaml:"depends_on,omitempty"`
	Restart     string             `yaml:"restart,omitempty"`
	// Attach, when set, controls whether compose streams this service's logs
	// in the foreground and waits on it for `docker compose up`'s foreground
	// exit logic. Pointer so the zero-value omits the key (default: true).
	Attach *bool `yaml:"attach,omitempty"`
}

type secretMount struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

type depCond struct {
	Condition string `yaml:"condition"`
}

// BuildOverride constructs the compose override YAML string from entries.
// imageRef is the fully-qualified image reference for the __vaka-init container
// (e.g. "emsi/vaka-init:v0.1.2"). Pass "" to disable injection globally
// (--vaka-init-present flag).
func BuildOverride(entries []ServiceEntry, imageRef string) (string, error) {
	override := composeOverride{
		Secrets:  make(map[string]secretDef),
		Services: make(map[string]serviceOverride),
	}

	injectVakaInit := imageRef != "" && anyNeedsInjection(entries)
	if injectVakaInit {
		override.Services[vakaInitServiceName] = serviceOverride{
			Image:      imageRef,
			Entrypoint: []string{vakaInitPath},
			Restart:    "no",
			Attach:     vakaInitNotAttached,
		}
	}

	for _, e := range entries {
		key := secretKey(e.Name)
		override.Secrets[key] = secretDef{Environment: e.EnvVarName}

		cmd := make([]string, 0, len(e.Entrypoint)+len(e.Command))
		cmd = append(cmd, e.Entrypoint...)
		cmd = append(cmd, e.Command...)

		svc := serviceOverride{
			User:       "0:0",
			Entrypoint: []string{vakaInitPath, "--"},
			Command:    cmd,
			CapAdd:     e.CapDelta,
			Secrets:    []secretMount{{Source: key, Target: "vaka.yaml"}},
		}

		if injectVakaInit && !e.OptOut {
			svc.VolumesFrom = []string{vakaInitServiceName + ":ro"}
			svc.DependsOn = map[string]depCond{
				vakaInitServiceName: {Condition: "service_completed_successfully"},
			}
		}

		override.Services[e.Name] = svc
	}

	data, err := yaml.Marshal(override)
	if err != nil {
		return "", fmt.Errorf("marshal compose override: %w", err)
	}
	return string(data), nil
}

// BuildVakaInitOnlyOverride returns a minimal compose override YAML containing
// only the __vaka-init service definition. Used by vaka down to include the
// __vaka-init container in teardown even though the full policy override is not re-generated.
func BuildVakaInitOnlyOverride(imageRef string) (string, error) {
	override := composeOverride{
		Services: map[string]serviceOverride{
			vakaInitServiceName: {
				Image:      imageRef,
				Entrypoint: []string{vakaInitPath},
				Restart:    "no",
				Attach:     vakaInitNotAttached,
			},
		},
	}
	data, err := yaml.Marshal(override)
	if err != nil {
		return "", fmt.Errorf("marshal vaka-init container override: %w", err)
	}
	return string(data), nil
}

func anyNeedsInjection(entries []ServiceEntry) bool {
	for _, e := range entries {
		if !e.OptOut {
			return true
		}
	}
	return false
}
