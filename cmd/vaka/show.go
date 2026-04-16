// cmd/vaka/show.go
package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"vaka.dev/vaka/pkg/nft"
)

func newShowCmd() *cobra.Command {
	var vakaFile string
	var composeFiles []string

	cmd := &cobra.Command{
		Use:   "show <service>",
		Short: "Print the nft ruleset that would be applied for a service (dry-run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]

			p, _, err := loadAndValidate(vakaFile, composeFiles)
			if err != nil {
				return err
			}

			svc, ok := p.Services[service]
			if !ok {
				return fmt.Errorf("service %q not found in %s", service, vakaFile)
			}
			if svc.Network == nil || svc.Network.Egress == nil {
				return fmt.Errorf("service %q has no network.egress policy", service)
			}

			// Generate ruleset without DNS resolution.
			// Hostnames in to: appear as unresolved comments.
			out, err := nft.Generate(svc.Network.Egress)
			if err != nil {
				return fmt.Errorf("generate: %w", err)
			}

			fmt.Print(out)
			return nil
		},
	}

	cmd.Flags().StringVarP(&vakaFile, "file", "f", "vaka.yaml", "Path to vaka.yaml")
	cmd.Flags().StringArrayVar(&composeFiles, "compose", nil, "Path(s) to compose file(s); repeat for multiple (omit to skip compose checks)")
	return cmd
}
