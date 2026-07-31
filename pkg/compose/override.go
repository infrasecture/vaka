package compose

import (
	"encoding/hex"
	"fmt"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
)

const (
	vakaInitPath        = "/opt/vaka/sbin/vaka-init"
	runtimeMountTarget  = "/opt/vaka"
	runtimeImageSubpath = "opt/vaka"
	// Engine 29.0 and 29.1 hex-encode the container ID, mount source, and
	// destination into one filesystem component. Forty hex characters retain
	// 160 bits of immutable image identity while keeping that component below
	// NAME_MAX. Docker rejects an ambiguous local image-ID prefix.
	runtimeMountSourceLength = 40

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
	// Source is either ImageID or its Engine-compatible immutable prefix. An
	// empty value defaults to the complete ImageID.
	Source  string
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
// image is mounted by its complete immutable local ID or an Engine-compatible
// prefix; its mutable lookup tag is never included in the generated Compose
// model. Metadata and labels retain the complete ID for auditing and
// container-state comparisons.
func BuildOverride(entries []ServiceEntry, runtime RuntimeMount) (string, error) {
	if strings.TrimSpace(runtime.Version) == "" {
		return "", fmt.Errorf("build compose override: runtime version is required")
	}
	mountSource := ""
	if runtime.ImageID != "" {
		var err error
		mountSource, err = runtimeImageMountSource(runtime.ImageID, runtime.Source)
		if err != nil {
			return "", fmt.Errorf("build compose override: %w", err)
		}
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
				Source:   mountSource,
				Target:   runtimeMountTarget,
				ReadOnly: true,
				Image:    &composetypes.ServiceVolumeImage{SubPath: runtimeImageSubpath},
			}}
		}

		override.Services[e.Name] = svc
	}

	return marshalOverride(override)
}

func runtimeImageMountSource(imageID, source string) (string, error) {
	digest, ok := strings.CutPrefix(imageID, "sha256:")
	if !ok || len(digest) != 64 {
		return "", fmt.Errorf("runtime image ID %q is not sha256:<64 hex>", imageID)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("runtime image ID %q is not sha256:<64 hex>", imageID)
	}
	if source == "" || source == imageID {
		return imageID, nil
	}
	wantCompact := digest[:runtimeMountSourceLength]
	if source != wantCompact {
		return "", fmt.Errorf("runtime mount source %q does not identify image %s", source, imageID)
	}
	return source, nil
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
