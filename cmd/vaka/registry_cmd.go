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
or refresh published indexes and Git preview snapshots. Registries live in the
registries.yaml configuration file (shown by 'vaka registry list').`,
	}
	cmd.AddCommand(
		newRegistryListCmd(),
		newRegistryAddCmd(),
		newRegistryAddGitCmd(),
		newRegistryRemoveCmd(),
		newRegistryRefreshCmd(),
	)
	return cmd
}

func newRegistryAddGitCmd() *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "add-git <name> <repository-url>",
		Short: "Add a Git branch as a development preview registry",
		Long: `Add an explicitly qualified development registry backed by a Git ref.
The ref is resolved to an immutable commit immediately. Only committed files
inside top-level recipe directories are packaged; worktree and ignored files,
hooks, and submodules are never used. The ref advances only when you run
'vaka registry refresh'. Shorthand refs name branches; use a full refs/... name
for a tag or other ref.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadRegistriesConfig()
			if err != nil {
				return err
			}
			reg := registry.Registry{
				Name: args[0],
				Git:  &registry.GitSource{URL: args[1], Ref: ref},
			}
			if err := cfg.Add(reg); err != nil {
				return err
			}
			client := newRegistryClient(0)
			res, err := client.RefreshIndex(cmd.Context(), reg)
			if err != nil {
				return err
			}
			if res.Stale {
				return fmt.Errorf("registry %q could not be resolved from Git: %s", reg.Name, res.FallbackReason)
			}
			if err := saveRegistriesConfig(cfg); err != nil {
				if cleanupErr := client.RemoveCache(reg); cleanupErr != nil {
					return fmt.Errorf("save registry configuration: %w (and remove newly generated cache: %v)", err, cleanupErr)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added Git preview registry %q (%s#%s at %s)\n",
				reg.Name, reg.Git.URL, reg.Git.Ref, shortCommit(res.Revision))
			return nil
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "branch name or full Git ref")
	_ = cmd.MarkFlagRequired("ref")
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
		Use:               "remove <name>",
		Aliases:           []string{"rm"},
		Short:             "Remove a registry and its cached data",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: firstArgComplete(completeRegistryNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadRegistriesConfig()
			if err != nil {
				return err
			}
			reg, ok := cfg.Lookup(args[0])
			if !ok {
				return fmt.Errorf("registry %q is not configured", args[0])
			}
			if err := cfg.Remove(args[0]); err != nil {
				return err
			}
			if err := newRegistryClient(0).RemoveCache(reg); err != nil {
				return fmt.Errorf("remove registry %q cache: %w", args[0], err)
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
		Short: "Refresh registry indexes and Git preview snapshots",
		Long: `Force a revalidation of every configured published index (or just the
named one). For a Git preview registry, resolve its configured ref to one commit
and atomically replace the generated local catalog.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: firstArgComplete(completeRegistryNames),
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
				res, err := client.RefreshIndex(cmd.Context(), reg)
				if err != nil {
					fmt.Fprintf(out, "%-16s error: %v\n", reg.Name, err)
					fail = true
					continue
				}
				status := "ok"
				if res.Stale {
					status = fmt.Sprintf("refresh failed (retained cache %s old)", res.Age.Round(time.Second))
					if res.FallbackReason != "" {
						status += ": " + res.FallbackReason
					}
					if reg.IsGit() {
						fail = true
					}
				} else if reg.IsGit() {
					status = "commit " + shortCommit(res.Revision)
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
			fmt.Fprintln(tw, "NAME\tSOURCE\tCACHE\tREVISION")
			for _, reg := range cfg.Registries {
				cache := "none"
				if age, ok := client.CacheAge(reg); ok {
					cache = age.Round(time.Second).String() + " old"
				}
				revision := "-"
				if got, ok := client.CacheRevision(reg); ok {
					revision = shortCommit(got)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", reg.Name, reg.SourceDescription(), cache, revision)
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

func shortCommit(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

// configPathHint is a test seam-free helper: it only renders the path.
func configPathHint() (string, error) {
	return registryConfigPath()
}
