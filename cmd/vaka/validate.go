// cmd/vaka/validate.go
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"
	"vaka.dev/vaka/pkg/policy"
)

func newValidateCmd() *cobra.Command {
	var vakaFile string
	var composeFiles []string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate vaka.yaml and print per-service summary",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, _, err := loadAndValidate(vakaFile, composeFiles, "")
			if err != nil {
				return err
			}

			// Print per-service summary.
			for name, svc := range p.Services {
				e := svc.Network.Egress
				accept := 0
				drop := 0
				reject := 0
				if e != nil {
					accept = len(e.Accept)
					drop = len(e.Drop)
					reject = len(e.Reject)
				}
				action := "reject"
				if e != nil {
					action = e.DefaultAction
				}
				fmt.Printf("✓ %-20s — %d accept rule(s), %d drop rule(s), %d reject rule(s), defaultAction: %s\n",
					name, accept, drop, reject, action)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&vakaFile, "file", "f", "vaka.yaml", "Path to vaka.yaml")
	cmd.Flags().StringArrayVar(&composeFiles, "compose", nil, "Path(s) to compose file(s); repeat for multiple (omit to skip compose checks)")
	return cmd
}

// loadAndValidate reads and validates vaka.yaml, then loads the compose
// project (all composeFiles merged via compose-go) to extract network_mode
// per service for the host-network guard.
// composeFiles may be empty — compose checks are skipped in that case.
// Returns the parsed policy and the loaded compose project (nil when no
// compose files are given).
func loadAndValidate(vakaFile string, composeFiles []string, workingDir string) (*policy.ServicePolicy, *composetypes.Project, error) {
	return loadAndValidateResolved(vakaFile, &composeResolution{Files: composeFiles, WorkingDir: workingDir})
}

func loadAndValidateResolved(vakaFile string, input *composeResolution) (*policy.ServicePolicy, *composetypes.Project, error) {
	f, err := os.Open(vakaFile)
	if err != nil {
		return nil, nil, err
	}
	p, err := policy.Parse(f)
	f.Close()
	if err != nil {
		return nil, nil, err
	}

	// Load compose project for network_mode checks (authoritative merge via compose-go).
	// project is nil when no compose files are given — policy.Validate treats nil
	// networkModes as "no compose data available, skip compose-dependent checks".
	// When composeFiles is non-empty any loading error is surfaced immediately.
	var project *composetypes.Project
	var networkModes map[string]string
	if input != nil && len(input.Files) > 0 {
		opts, err := newComposeProjectOptions(input, false)
		if err != nil {
			return nil, nil, fmt.Errorf("compose project options: %w", err)
		}
		project, err = opts.LoadProject(context.Background())
		if err != nil {
			return nil, nil, fmt.Errorf("load compose project: %w", err)
		}
		networkModes = make(map[string]string)
		for name, svc := range project.AllServices() {
			networkModes[name] = svc.NetworkMode
		}
	}

	errs := policy.ValidateHost(p, networkModes)

	warnDegradedEnforcement(p, project)

	// Warn on defaultAction: accept.
	for name, svc := range p.Services {
		if svc.Network != nil && svc.Network.Egress != nil &&
			svc.Network.Egress.DefaultAction == "accept" {
			fmt.Fprintf(os.Stderr, "WARNING: service %s uses defaultAction: accept — all unmatched egress traffic is allowed.\n", name)
		}
	}

	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return nil, nil, fmt.Errorf("%s", strings.Join(msgs, "\n"))
	}

	return p, project, nil
}

func warnDegradedEnforcement(p *policy.ServicePolicy, project *composetypes.Project) {
	if p == nil || project == nil {
		return
	}

	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	allServices := project.AllServices()
	managedContainerNames := make(map[string]string)

	for _, name := range names {
		composeSvc, ok := allServices[name]
		if !ok {
			continue
		}
		if containerName := strings.TrimSpace(composeSvc.ContainerName); containerName != "" {
			managedContainerNames[containerName] = name
		}
		reasons := degradedEnforcementReasons(composeSvc, p.Services[name].Runtime)
		if len(reasons) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "Vaka warning: service %s %s. Vaka enforcement is best-effort for this service.\n",
			name, strings.Join(reasons, "; "))
	}

	// A service which joins a managed service's namespace can affect the same
	// enforcement boundary without itself being managed. This is an explicit,
	// trusted Compose choice, so make the degradation visible rather than
	// prohibiting it.
	allNames := make([]string, 0, len(allServices))
	for name := range allServices {
		allNames = append(allNames, name)
	}
	sort.Strings(allNames)
	for _, name := range allNames {
		if _, managed := p.Services[name]; managed {
			continue
		}
		svc := allServices[name]
		warnReverseNamespaceSharing(name, "network", svc.NetworkMode, p, managedContainerNames)
		warnReverseNamespaceSharing(name, "PID", svc.Pid, p, managedContainerNames)
	}
}

func degradedEnforcementReasons(svc composetypes.ServiceConfig, runtime *policy.RuntimeConfig) []string {
	var reasons []string
	if svc.Privileged {
		reasons = append(reasons, "is privileged and can bypass Vaka's runtime and egress policy")
	}
	if capabilityListContains(svc.CapAdd, "ALL") && !svc.Privileged && !runtimeDropsCapability(runtime, "ALL") {
		reasons = append(reasons, "requests all Linux capabilities and can weaken Vaka's runtime boundary")
	}
	if explicitlyRetainsCapability(svc, "SYS_ADMIN") && !runtimeDropsCapability(runtime, "SYS_ADMIN") {
		reasons = append(reasons, "retains SYS_ADMIN and can replace Vaka's runtime")
	}
	if explicitlyRetainsCapability(svc, "NET_ADMIN") && !runtimeDropsCapability(runtime, "NET_ADMIN") {
		reasons = append(reasons, "retains NET_ADMIN and can modify Vaka's nftables policy")
	}
	if explicitlyRetainsCapability(svc, "SYS_PTRACE") && !runtimeDropsCapability(runtime, "SYS_PTRACE") {
		reasons = append(reasons, "retains SYS_PTRACE and can interfere with Vaka's runtime processes")
	}
	if capabilities := retainedIsolationCapabilities(svc, runtime); len(capabilities) > 0 {
		reasons = append(reasons, fmt.Sprintf("retains powerful Linux capabilities (%s) that can weaken container isolation", strings.Join(capabilities, ", ")))
	}
	if svc.UseAPISocket || mountsDockerSocket(svc.Volumes) {
		reasons = append(reasons, "has Docker daemon access and can bypass the container security boundary")
	}
	if isSharedNamespaceMode(svc.Pid) {
		reasons = append(reasons, "shares a PID namespace and can interfere with Vaka's runtime processes")
	}
	if svc.Init != nil && *svc.Init {
		reasons = append(reasons, "uses init: true, leaving Docker's trusted init shim outside Vaka's capability-drop path")
	}
	return reasons
}

func retainedIsolationCapabilities(svc composetypes.ServiceConfig, runtime *policy.RuntimeConfig) []string {
	if svc.Privileged || capabilityListContains(svc.CapAdd, "ALL") {
		return nil // covered by the stronger privileged/all-capabilities warning
	}
	candidates := []string{
		"BPF",
		"CHECKPOINT_RESTORE",
		"DAC_READ_SEARCH",
		"PERFMON",
		"SYS_BOOT",
		"SYS_MODULE",
		"SYS_RAWIO",
		"SYS_TIME",
		"SYSLOG",
	}
	var retained []string
	for _, capability := range candidates {
		if explicitlyRetainsCapability(svc, capability) && !runtimeDropsCapability(runtime, capability) {
			retained = append(retained, capability)
		}
	}
	return retained
}

func explicitlyRetainsCapability(svc composetypes.ServiceConfig, name string) bool {
	if svc.Privileged {
		return false // privileged already has a stronger, more useful warning
	}
	want := normalizeCapabilityName(name)
	return containerHasCapability(svc, want) &&
		(capabilityListContains(svc.CapAdd, "ALL") || capabilityListContains(svc.CapAdd, want))
}

func runtimeDropsCapability(runtime *policy.RuntimeConfig, name string) bool {
	if runtime == nil {
		return false
	}
	want := normalizeCapabilityName(name)
	for _, dropped := range runtime.DropCaps {
		capability := normalizeCapabilityName(dropped)
		if capability == "ALL" || capability == want {
			return true
		}
	}
	return false
}

func mountsDockerSocket(volumes []composetypes.ServiceVolumeConfig) bool {
	for _, volume := range volumes {
		if isDockerSocketPath(volume.Source) || isDockerSocketPath(volume.Target) {
			return true
		}
		if volume.Type == composetypes.VolumeTypeBind && pathContainsDockerSocket(volume.Source) {
			return true
		}
	}
	return false
}

func pathContainsDockerSocket(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	for _, socket := range []string{"/run/docker.sock", "/var/run/docker.sock"} {
		relative, err := filepath.Rel(path, socket)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func isDockerSocketPath(path string) bool {
	switch strings.TrimSuffix(strings.TrimSpace(path), "/") {
	case "/var/run/docker.sock", "/run/docker.sock":
		return true
	default:
		return false
	}
}

func isSharedNamespaceMode(mode string) bool {
	mode = strings.TrimSpace(mode)
	return mode == "host" || strings.HasPrefix(mode, composetypes.ServicePrefix) ||
		strings.HasPrefix(mode, composetypes.ContainerPrefix)
}

func warnReverseNamespaceSharing(name, kind, mode string, p *policy.ServicePolicy, managedContainerNames map[string]string) {
	mode = strings.TrimSpace(mode)
	target, serviceMode := strings.CutPrefix(mode, composetypes.ServicePrefix)
	if serviceMode {
		if _, managed := p.Services[target]; !managed {
			return
		}
	} else {
		containerName, containerMode := strings.CutPrefix(mode, composetypes.ContainerPrefix)
		if !containerMode {
			return
		}
		var managed bool
		target, managed = managedContainerNames[containerName]
		if !managed {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "Vaka warning: unmanaged service %s joins managed service %s's %s namespace. Vaka enforcement is shared and best-effort for service %s.\n",
		name, target, kind, target)
}
