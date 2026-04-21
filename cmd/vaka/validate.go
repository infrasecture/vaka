// cmd/vaka/validate.go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"
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
			p, _, err := loadAndValidate(vakaFile, composeFiles)
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
func loadAndValidate(vakaFile string, composeFiles []string) (*policy.ServicePolicy, *composetypes.Project, error) {
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
	if len(composeFiles) > 0 {
		opts, err := composecli.NewProjectOptions(composeFiles,
			composecli.WithWorkingDirectory("."),
			composecli.WithOsEnv,
			composecli.WithDotEnv,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("compose project options: %w", err)
		}
		project, err = opts.LoadProject(context.Background())
		if err != nil {
			return nil, nil, fmt.Errorf("load compose project: %w", err)
		}
		networkModes = make(map[string]string)
		for name, svc := range project.Services {
			networkModes[name] = svc.NetworkMode
		}
	}

	errs := policy.ValidateHost(p, networkModes)

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
