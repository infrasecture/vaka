// cmd/vaka/registry_cmd.go
package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage recipe registries",
		Long: `Manage the configured recipe registries. Phase 1 ships the read side;
add and remove registries by editing the registries.yaml configuration file
(shown by 'vaka registry list').`,
	}
	cmd.AddCommand(newRegistryListCmd())
	return cmd
}

func newRegistryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured registries and their cache state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadRegistriesConfig()
			if err != nil {
				return err
			}
			client := newRegistryClient(browseIndexMaxAge)

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tURL\tCACHE")
			for _, reg := range cfg.Registries {
				cache := "none"
				if age, ok := client.CacheAge(reg); ok {
					cache = age.Round(time.Second).String() + " old"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", reg.Name, reg.URL, cache)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if path, err := configPathHint(); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "\nConfiguration: %s\n", path)
			}
			return nil
		},
	}
}

// configPathHint is a test seam-free helper: it only renders the path.
func configPathHint() (string, error) {
	return registryConfigPath()
}
