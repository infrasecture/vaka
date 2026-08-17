// cmd/vaka/validate.go
package main

import (
	"context"
	"fmt"
	"os"
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

	for _, name := range names {
		composeSvc, ok := allServices[name]
		if !ok {
			continue
		}
		reasons := degradedEnforcementReasons(composeSvc, p.Services[name].Runtime)
		if len(reasons) == 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "Vaka warning: service %s %s. Egress enforcement is best-effort for this service.\n",
			name, strings.Join(reasons, "; "))
	}
}

func degradedEnforcementReasons(svc composetypes.ServiceConfig, runtime *policy.RuntimeConfig) []string {
	var reasons []string
	if svc.Privileged {
		reasons = append(reasons, "is privileged and can bypass Vaka's runtime and egress policy")
	}
	if explicitlyAddsCapability(svc, "SYS_ADMIN") && !runtimeDropsCapability(runtime, "SYS_ADMIN") {
		reasons = append(reasons, "retains SYS_ADMIN and can replace Vaka's runtime")
	}
	if explicitlyAddsCapability(svc, "NET_ADMIN") && !runtimeDropsCapability(runtime, "NET_ADMIN") {
		reasons = append(reasons, "retains NET_ADMIN and can modify Vaka's nftables policy")
	}
	return reasons
}

func explicitlyAddsCapability(svc composetypes.ServiceConfig, name string) bool {
	want := normalizeCapabilityName(name)
	for _, added := range svc.CapAdd {
		capability := normalizeCapabilityName(added)
		if capability == "ALL" || capability == want {
			return true
		}
	}
	return false
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
