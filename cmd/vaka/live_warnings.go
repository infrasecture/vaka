package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/docker/docker/api/types/container"
	"vaka.dev/vaka/internal/runtimebundle"
	"vaka.dev/vaka/pkg/compose"
	"vaka.dev/vaka/pkg/policy"
)

type liveEnforcementWarnings struct {
	reasons             []string
	defaultActionAccept bool
}

func deriveLiveEnforcementWarnings(inspect containertypes.InspectResponse) liveEnforcementWarnings {
	if inspect.HostConfig == nil {
		return liveEnforcementWarnings{reasons: []string{"has no live HostConfig metadata, so Vaka cannot assess degraded enforcement settings"}}
	}

	liveService := composeServiceFromContainerInspect(inspect)
	injected, err := injectedServiceFromContainerConfig(inspect.Config)
	if err != nil {
		// Privilege, namespace, LSM, init, and daemon-socket checks do not
		// depend on the injected drop contract. Avoid reporting Vaka's own
		// temporary CapAdd entries as retained when that contract is unreadable.
		liveService.CapAdd = nil
		liveService.CapDrop = nil
		reasons := degradedEnforcementReasons(liveService, nil)
		reasons = append(reasons, "has live capability settings whose injected Vaka drop contract cannot be verified")
		return liveEnforcementWarnings{reasons: reasons}
	}

	warnings := liveEnforcementWarnings{
		reasons: degradedEnforcementReasons(liveService, injected.Runtime),
	}
	warnings.defaultActionAccept = injected.Network != nil &&
		injected.Network.Egress != nil &&
		injected.Network.Egress.DefaultAction == "accept"
	return warnings
}

func composeServiceFromContainerInspect(inspect containertypes.InspectResponse) composetypes.ServiceConfig {
	host := inspect.HostConfig
	service := composetypes.ServiceConfig{
		CapAdd:      append([]string(nil), host.CapAdd...),
		CapDrop:     append([]string(nil), host.CapDrop...),
		Init:        host.Init,
		Pid:         string(host.PidMode),
		Privileged:  host.Privileged,
		SecurityOpt: append([]string(nil), host.SecurityOpt...),
	}

	if profile := strings.TrimSpace(inspect.AppArmorProfile); profile != "" && !hasSecurityOption(service.SecurityOpt, "apparmor") {
		service.SecurityOpt = append(service.SecurityOpt, "apparmor="+profile)
	}
	for _, mounted := range inspect.Mounts {
		service.Volumes = append(service.Volumes, composetypes.ServiceVolumeConfig{
			Type:     string(mounted.Type),
			Source:   mounted.Source,
			Target:   mounted.Destination,
			ReadOnly: !mounted.RW,
		})
	}
	return service
}

func hasSecurityOption(options []string, want string) bool {
	for _, raw := range options {
		name, _, ok := splitSecurityOption(raw)
		if ok && name == want {
			return true
		}
	}
	return false
}

func injectedServiceFromContainerConfig(config *containertypes.Config) (*policy.ServiceConfig, error) {
	if config == nil {
		return nil, fmt.Errorf("container has no Config metadata")
	}
	encoded, ok := containerEnvironmentValue(config.Env, runtimebundle.PolicyEnvironment)
	if !ok || strings.TrimSpace(encoded) == "" {
		return nil, fmt.Errorf("container configuration has no %s", runtimebundle.PolicyEnvironment)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", runtimebundle.PolicyEnvironment, err)
	}
	p, err := policy.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	// Match vaka-init's order exactly: validation normalizes capability names
	// in place, while the stored revision covers the pre-normalized document.
	revision, err := policy.Revision(p)
	if err != nil {
		return nil, fmt.Errorf("compute injected policy revision: %w", err)
	}
	expectedRevision, ok := containerEnvironmentValue(config.Env, runtimebundle.PolicyRevisionEnvironment)
	if !ok || strings.TrimSpace(expectedRevision) != revision {
		return nil, fmt.Errorf("injected policy revision does not match container environment")
	}
	if labelRevision := strings.TrimSpace(config.Labels[compose.PolicyRevisionLabel]); labelRevision != revision {
		return nil, fmt.Errorf("injected policy revision does not match container label")
	}
	if errs := policy.ValidateInjected(p); len(errs) > 0 {
		return nil, fmt.Errorf("invalid injected policy: %v", errs)
	}
	if len(p.Services) != 1 {
		return nil, fmt.Errorf("injected policy has %d services, expected one", len(p.Services))
	}

	for _, service := range p.Services {
		return service, nil
	}
	return nil, fmt.Errorf("injected policy has no service")
}

func containerEnvironmentValue(environment []string, name string) (string, bool) {
	for _, entry := range environment {
		key, candidate, _ := strings.Cut(entry, "=")
		if key == name {
			// vaka-init reads these values with os.Getenv. Go deliberately
			// preserves the first occurrence of a duplicate environment key,
			// so live inspection must use the same rule.
			return candidate, true
		}
	}
	return "", false
}

func warnLiveDegradedEnforcement(serviceName string, targets []execTarget) {
	for _, target := range targets {
		if !target.Managed {
			continue
		}
		container := target.ContainerID
		if len(container) > 12 {
			container = container[:12]
		}
		if container == "" {
			container = "<unknown>"
		}
		if len(target.LiveWarnings.reasons) > 0 {
			fmt.Fprintf(os.Stderr, "Vaka warning: live container %s for service %s %s. Vaka enforcement is best-effort for this container.\n",
				container, serviceName, strings.Join(target.LiveWarnings.reasons, "; "))
		}
		if target.LiveWarnings.defaultActionAccept {
			fmt.Fprintf(os.Stderr, "WARNING: live container %s for service %s uses defaultAction: accept — all unmatched egress traffic is allowed.\n",
				container, serviceName)
		}
	}
}
