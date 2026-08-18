package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
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

type liveNamespaceContainer struct {
	id          string
	service     string
	names       []string
	managed     bool
	networkMode string
	pidMode     string
}

func liveNamespaceContainerFromInspect(summary containertypes.Summary, inspect containertypes.InspectResponse) liveNamespaceContainer {
	labels := summary.Labels
	if inspect.Config != nil && inspect.Config.Labels != nil {
		labels = inspect.Config.Labels
	}
	record := liveNamespaceContainer{
		id:      inspect.ID,
		service: labels[composeServiceLabel],
		managed: labels[compose.ManagedLabel] == "true",
	}
	if record.service == "" {
		record.service = summary.Labels[composeServiceLabel]
	}
	if inspect.HostConfig != nil {
		record.networkMode = string(inspect.HostConfig.NetworkMode)
		record.pidMode = string(inspect.HostConfig.PidMode)
	}
	record.names = appendContainerName(record.names, inspect.Name)
	for _, name := range summary.Names {
		record.names = appendContainerName(record.names, name)
	}
	sort.Strings(record.names)
	return record
}

func appendContainerName(names []string, candidate string) []string {
	candidate = strings.TrimPrefix(candidate, "/")
	if candidate == "" {
		return names
	}
	for _, existing := range names {
		if existing == candidate {
			return names
		}
	}
	return append(names, candidate)
}

// reverseNamespaceWarningReasons reconstructs live container:<ID/name>
// relationships. Docker has already resolved Compose service: references to
// container identities by this point, so the current Compose model is neither
// necessary nor authoritative.
func reverseNamespaceWarningReasons(containers []liveNamespaceContainer, selected map[string]bool) map[string][]string {
	reasons := make(map[string][]string)
	seen := make(map[string]map[string]bool)
	for _, peer := range containers {
		if peer.managed {
			continue
		}
		for _, namespace := range []struct {
			kind string
			mode string
		}{
			{kind: "network", mode: peer.networkMode},
			{kind: "PID", mode: peer.pidMode},
		} {
			ref, ok := liveContainerNamespaceReference(namespace.mode)
			if !ok {
				continue
			}
			target, ok := resolveLiveContainerReference(ref, containers)
			if !ok || !target.managed || !selected[target.id] {
				continue
			}
			reason := fmt.Sprintf("shares its %s namespace with unmanaged live container %s", namespace.kind, shortContainerID(peer.id))
			if peer.service != "" {
				reason += " for service " + peer.service
			}
			if seen[target.id] == nil {
				seen[target.id] = make(map[string]bool)
			}
			if seen[target.id][reason] {
				continue
			}
			seen[target.id][reason] = true
			reasons[target.id] = append(reasons[target.id], reason)
		}
	}
	for id := range reasons {
		sort.Strings(reasons[id])
	}
	return reasons
}

func liveContainerNamespaceReference(mode string) (string, bool) {
	mode = strings.TrimSpace(mode)
	kind, ref, ok := strings.Cut(mode, ":")
	if !ok || !strings.EqualFold(kind, "container") || ref == "" {
		return "", false
	}
	return strings.TrimPrefix(ref, "/"), true
}

func resolveLiveContainerReference(ref string, containers []liveNamespaceContainer) (liveNamespaceContainer, bool) {
	ref = strings.TrimPrefix(ref, "/")
	if ref == "" {
		return liveNamespaceContainer{}, false
	}
	for _, candidate := range containers {
		if candidate.id == ref {
			return candidate, true
		}
		for _, name := range candidate.names {
			if name == ref {
				return candidate, true
			}
		}
	}

	var match liveNamespaceContainer
	matches := 0
	for _, candidate := range containers {
		if candidate.id != "" && strings.HasPrefix(candidate.id, ref) {
			match = candidate
			matches++
		}
	}
	return match, matches == 1
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "<unknown>"
	}
	return id
}

func appendLiveWarningReasons(warnings *liveEnforcementWarnings, reasons ...string) {
	seen := make(map[string]bool, len(warnings.reasons)+len(reasons))
	for _, reason := range warnings.reasons {
		seen[reason] = true
	}
	for _, reason := range reasons {
		if reason == "" || seen[reason] {
			continue
		}
		seen[reason] = true
		warnings.reasons = append(warnings.reasons, reason)
	}
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
		container := shortContainerID(target.ContainerID)
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
