package compose

import (
	"fmt"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

const (
	vakaInitPath        = "/opt/vaka/sbin/vaka-init"
	runtimeMountTarget  = "/opt/vaka"
	runtimeImageSubpath = "opt/vaka"

	ManagedLabel        = "agent.vaka.managed"
	PolicyRevisionLabel = "agent.vaka.policy-revision"
	RuntimeImageLabel   = "agent.vaka.runtime-image"
	RuntimeVersionLabel = "agent.vaka.runtime.version"
)

// RuntimeMount identifies the exact runtime image mounted into managed
// services. ImageID is empty only when the binaries are already present in the
// service image.
type RuntimeMount struct {
	ImageID string
	Version string
}

// ServiceEntry holds per-service data needed to build the compose override.
type ServiceEntry struct {
	Name           string
	Entrypoint     []string
	Command        []string
	CapDelta       []string
	EnvVarName     string
	PolicyRevision string
	// OptOut is true when the service carries the agent.vaka.init: present label,
	// meaning vaka-init is already baked into the image at /opt/vaka/sbin/.
	OptOut bool
}

// secretKey returns the compose secret key for a service name.
// "llm-gateway" -> "vaka_llm_gateway_conf"
func secretKey(serviceName string) string {
	return "vaka_" + strings.ReplaceAll(strings.ToLower(serviceName), "-", "_") + "_conf"
}

type composeOverride struct {
	Metadata *runtimeMetadata           `yaml:"x-vaka,omitempty"`
	Secrets  map[string]secretDef       `yaml:"secrets,omitempty"`
	Services map[string]serviceOverride `yaml:"services,omitempty"`
}

type runtimeMetadata struct {
	RuntimeVersion string `yaml:"runtime-version"`
	RuntimeImage   string `yaml:"runtime-image,omitempty"`
}

type secretDef struct {
	Environment string `yaml:"environment"`
}

type serviceOverride struct {
	User       string                             `yaml:"user,omitempty"`
	Entrypoint []string                           `yaml:"entrypoint,omitempty"`
	Command    []string                           `yaml:"command,omitempty"`
	CapAdd     []string                           `yaml:"cap_add,omitempty"`
	Labels     map[string]string                  `yaml:"labels,omitempty"`
	Secrets    []secretMount                      `yaml:"secrets,omitempty"`
	Volumes    []composetypes.ServiceVolumeConfig `yaml:"volumes,omitempty"`
}

type secretMount struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

// BuildOverride constructs the policy-enforcing compose override. The runtime
// image is mounted by immutable local ID; its mutable lookup tag is never
// included in the generated Compose model.
func BuildOverride(entries []ServiceEntry, runtime RuntimeMount) (string, error) {
	if strings.TrimSpace(runtime.Version) == "" {
		return "", fmt.Errorf("build compose override: runtime version is required")
	}
	override := composeOverride{
		Metadata: &runtimeMetadata{
			RuntimeVersion: runtime.Version,
			RuntimeImage:   runtime.ImageID,
		},
		Secrets:  make(map[string]secretDef),
		Services: make(map[string]serviceOverride),
	}

	for _, e := range entries {
		if strings.TrimSpace(e.PolicyRevision) == "" {
			return "", fmt.Errorf("build compose override: service %s has no policy revision", e.Name)
		}
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
			Labels: map[string]string{
				ManagedLabel:        "true",
				PolicyRevisionLabel: e.PolicyRevision,
				RuntimeVersionLabel: runtime.Version,
			},
			Secrets: []secretMount{{Source: key, Target: "vaka.yaml"}},
		}

		if runtime.ImageID != "" && !e.OptOut {
			svc.Labels[RuntimeImageLabel] = runtime.ImageID
			svc.Volumes = []composetypes.ServiceVolumeConfig{{
				Type:     composetypes.VolumeTypeImage,
				Source:   runtime.ImageID,
				Target:   runtimeMountTarget,
				ReadOnly: true,
				Image:    &composetypes.ServiceVolumeImage{SubPath: runtimeImageSubpath},
			}}
		}

		override.Services[e.Name] = svc
	}

	return marshalOverride(override)
}

// BuildReferenceOverride returns the metadata-only override used for Compose
// commands that do not create containers. Keeping an injected file on these
// paths preserves a single Compose invocation mechanism without fabricating a
// helper service or requiring policy evaluation.
func BuildReferenceOverride(runtimeVersion string) (string, error) {
	if strings.TrimSpace(runtimeVersion) == "" {
		return "", fmt.Errorf("build reference override: runtime version is required")
	}
	return marshalOverride(composeOverride{
		Metadata: &runtimeMetadata{RuntimeVersion: runtimeVersion},
	})
}

func marshalOverride(override composeOverride) (string, error) {
	data, err := yaml.Marshal(override)
	if err != nil {
		return "", fmt.Errorf("marshal compose override: %w", err)
	}
	return string(data), nil
}
