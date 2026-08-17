package compose

import (
	"encoding/hex"
	"fmt"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/internal/runtimebundle"
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
	ServiceImageLabel   = "agent.vaka.service-image"
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
	Name             string
	ImageID          string
	Entrypoint       []string
	Command          []string
	CapDelta         []string
	PolicyPayload    string
	PolicyRevision   string
	Healthcheck      []string
	HealthcheckShell []string
	// OptOut is retained for legacy callers. Managed-service validation rejects
	// this mutable baked-runtime mode before building an override.
	OptOut bool
}

type composeOverride struct {
	Metadata *runtimeMetadata           `yaml:"x-vaka,omitempty"`
	Services map[string]serviceOverride `yaml:"services,omitempty"`
}

type runtimeMetadata struct {
	RuntimeVersion string `yaml:"runtime-version"`
	RuntimeImage   string `yaml:"runtime-image,omitempty"`
}

type serviceOverride struct {
	Image       string                             `yaml:"image,omitempty"`
	PullPolicy  string                             `yaml:"pull_policy,omitempty"`
	User        string                             `yaml:"user,omitempty"`
	Entrypoint  []string                           `yaml:"entrypoint,omitempty"`
	Command     []string                           `yaml:"command,omitempty"`
	CapAdd      []string                           `yaml:"cap_add,omitempty"`
	Labels      map[string]string                  `yaml:"labels,omitempty"`
	Environment map[string]string                  `yaml:"environment,omitempty"`
	Volumes     []composetypes.ServiceVolumeConfig `yaml:"volumes,omitempty"`
	Healthcheck *healthcheckOverride               `yaml:"healthcheck,omitempty"`
}

type healthcheckOverride struct {
	Test    []string `yaml:"test,omitempty"`
	Disable bool     `yaml:"disable,omitempty"`
}

// BuildOverride constructs the policy-enforcing compose override. The runtime
// image is mounted by its complete immutable local ID or an Engine-compatible
// prefix; its mutable lookup tag is never included in the generated Compose
// model. Metadata and labels retain the complete ID for auditing and
// container-state comparisons.
func BuildOverride(entries []ServiceEntry, runtime RuntimeMount, preparedUnmanaged ...string) (string, error) {
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
		Services: make(map[string]serviceOverride),
	}
	for _, name := range preparedUnmanaged {
		if strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("build compose override: prepared service name is empty")
		}
		override.Services[name] = serviceOverride{PullPolicy: "never"}
	}

	for _, e := range entries {
		if strings.TrimSpace(e.PolicyRevision) == "" {
			return "", fmt.Errorf("build compose override: service %s has no policy revision", e.Name)
		}
		if strings.TrimSpace(e.PolicyPayload) == "" {
			return "", fmt.Errorf("build compose override: service %s has no policy payload", e.Name)
		}
		if _, err := runtimeImageMountSource(e.ImageID, ""); err != nil {
			return "", fmt.Errorf("build compose override: service %s has invalid inspected image identity: %w", e.Name, err)
		}
		entrypoint := make([]string, 0, 2+len(e.Entrypoint))
		entrypoint = append(entrypoint, vakaInitPath, "--")
		entrypoint = append(entrypoint, e.Entrypoint...)

		svc := serviceOverride{
			Image:      e.ImageID,
			PullPolicy: "never",
			User:       "0:0",
			Entrypoint: entrypoint,
			Command:    append([]string(nil), e.Command...),
			CapAdd:     e.CapDelta,
			Labels: map[string]string{
				ManagedLabel:        "true",
				PolicyRevisionLabel: e.PolicyRevision,
				ServiceImageLabel:   e.ImageID,
				RuntimeVersionLabel: runtime.Version,
			},
			Environment: map[string]string{
				runtimebundle.PolicyEnvironment:         e.PolicyPayload,
				runtimebundle.PolicyRevisionEnvironment: e.PolicyRevision,
			},
		}
		wrappedHealthcheck, err := wrapHealthcheck(e.Healthcheck, e.HealthcheckShell)
		if err != nil {
			return "", fmt.Errorf("build compose override: service %s: %w", e.Name, err)
		}
		if len(wrappedHealthcheck) > 0 {
			svc.Healthcheck = &healthcheckOverride{Test: wrappedHealthcheck}
		} else {
			// Compose merge semantics otherwise inherit a healthcheck from a
			// different image selected after inspection.
			svc.Healthcheck = &healthcheckOverride{Disable: true}
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

func wrapHealthcheck(test, imageShell []string) ([]string, error) {
	if len(test) == 0 || (len(test) == 1 && test[0] == "NONE") {
		return nil, nil
	}
	switch test[0] {
	case "CMD":
		if len(test) == 1 {
			return nil, fmt.Errorf("healthcheck CMD has no command")
		}
		out := []string{"CMD", vakaInitPath, "exec", "--"}
		return append(out, test[1:]...), nil
	case "CMD-SHELL":
		if len(test) < 2 {
			return nil, fmt.Errorf("healthcheck CMD-SHELL has no command")
		}
		shell := append([]string{}, imageShell...)
		if len(shell) == 0 {
			shell = []string{"/bin/sh", "-c"}
		}
		out := []string{"CMD", vakaInitPath, "exec", "--"}
		out = append(out, shell...)
		return append(out, test[1:]...), nil
	default:
		return nil, fmt.Errorf("unsupported healthcheck test type %q", test[0])
	}
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
