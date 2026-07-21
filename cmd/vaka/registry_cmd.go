// cmd/vaka/registry_cmd.go
package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"vaka.dev/vaka/pkg/registry"
)

func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage recipe registries",
		Long: `Manage the configured recipe registries: list them, add or remove one,
or refresh cached indexes. Registries live in the registries.yaml
configuration file (shown by 'vaka registry list').`,
	}
	cmd.AddCommand(
		newRegistryListCmd(),
		newRegistryAddCmd(),
		newRegistryRemoveCmd(),
		newRegistryRefreshCmd(),
	)
	return cmd
}

func newRegistryAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <name> <index-url>",
		Short: "Add a registry to the configuration",
		Long: `Add a registry. The name must match [a-z0-9-]+ and be unique; the index
URL must be https:// (file:// is allowed for local/air-gapped registries).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadRegistriesConfig()
			if err != nil {
				return err
			}
			if err := cfg.Add(registry.Registry{Name: args[0], URL: args[1]}); err != nil {
				return err
			}
			if err := saveRegistriesConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added registry %q (%s)\n", args[0], args[1])
			return nil
		},
	}
}

func newRegistryRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a registry from the configuration",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadRegistriesConfig()
			if err != nil {
				return err
			}
			if err := cfg.Remove(args[0]); err != nil {
				return err
			}
			if err := saveRegistriesConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed registry %q\n", args[0])
			return nil
		},
	}
}

func newRegistryRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh [name]",
		Short: "Re-fetch registry indexes, updating the local cache",
		Long: `Force a revalidation of every configured registry's index (or just the
named one), updating the local cache.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadRegistriesConfig()
			if err != nil {
				return err
			}
			only := ""
			if len(args) == 1 {
				only = args[0]
				if _, ok := cfg.Lookup(only); !ok {
					return fmt.Errorf("registry %q is not configured", only)
				}
			}
			client := newRegistryClient(0) // maxAge 0 → always revalidate
			out := cmd.OutOrStdout()
			fail := false
			for _, reg := range cfg.Registries {
				if only != "" && reg.Name != only {
					continue
				}
				res, err := client.FetchIndex(reg)
				if err != nil {
					fmt.Fprintf(out, "%-16s error: %v\n", reg.Name, err)
					fail = true
					continue
				}
				status := "ok"
				if res.Stale {
					status = fmt.Sprintf("unreachable (cache %s old)", res.Age.Round(time.Second))
				}
				fmt.Fprintf(out, "%-16s %d recipe(s), %s\n", reg.Name, len(res.Index.Recipes), status)
			}
			if fail {
				return fmt.Errorf("one or more registries could not be refreshed")
			}
			return nil
		},
	}
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
