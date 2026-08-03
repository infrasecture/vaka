// cmd/vaka/recipes.go
package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"vaka.dev/vaka/pkg/registry"
)

func newRecipesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recipes",
		Short: "Browse the recipe catalogs of the configured registries",
	}
	cmd.AddCommand(newRecipesListCmd(), newRecipesInfoCmd())
	return cmd
}

func newRecipesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all recipes published by the configured registries",
		Long: `List the registry catalogs (newest version per recipe). This is a
registry-bound command: it reads the published indexes and never scans the
local filesystem for instantiated recipes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			world, err := loadRegistryWorld(browseIndexMaxAge, "", false, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVERSION\tRISK\tDESCRIPTION")
			for _, row := range world.catalogRows() {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					world.displayName(row), row.entry.Version, riskColumn(row.entry), row.entry.Description)
			}
			return tw.Flush()
		},
	}
}

func newRecipesInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <[registry/]name>[@version]",
		Short: "Show a recipe's published metadata",
		Long: `Show the registry-published metadata of a recipe: description, tags,
versions, documented environment variables, and the advisory policy summary
computed by the registry's CI. vaka recomputes the policy locally on every
get; this view is what the registry claims.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: firstArgComplete(completeRecipeRefs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := registry.ParseRef(args[0])
			if err != nil {
				return err
			}
			world, err := loadRegistryWorld(browseIndexMaxAge, "", false, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := registry.Resolve(world.cfg, world.indexes, ref)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			e := res.Entry
			fmt.Fprintf(out, "Name:        %s/%s\n", res.Registry.Name, res.Name)
			fmt.Fprintf(out, "Version:     %s (published %s)\n", e.Version, e.Created)
			fmt.Fprintf(out, "Description: %s\n", e.Description)
			if len(e.Tags) > 0 {
				fmt.Fprintf(out, "Tags:        %s\n", strings.Join(e.Tags, ", "))
			}
			if e.MinVakaVersion != "" {
				fmt.Fprintf(out, "Requires:    vaka >= %s\n", e.MinVakaVersion)
			}
			fmt.Fprintf(out, "Digest:      %s\n", e.Digest)
			if e.SourceRevision != "" {
				fmt.Fprintf(out, "Git commit:  %s\n", e.SourceRevision)
			}

			if versions := world.indexes[res.Registry.Name].Recipes[res.Name]; len(versions) > 1 {
				fmt.Fprintf(out, "Versions (%d indexed, newest first):\n", len(versions))
				tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
				for _, v := range versions {
					marker := "  "
					if v.Version == e.Version {
						marker = "* " // the one this info is about
					}
					fmt.Fprintf(tw, "%s%s\t%s\t%s\n", marker, v.Version, v.Created, v.Digest)
				}
				tw.Flush()
			}

			if len(e.Env) > 0 {
				fmt.Fprintln(out, "Environment:")
				tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
				for _, v := range e.Env {
					req := "optional"
					if v.Required {
						req = "required"
					}
					def := ""
					if v.Default != "" {
						def = " (default: " + v.Default + ")"
					}
					fmt.Fprintf(tw, "  %s\t%s\t%s%s\n", v.Name, req, v.Description, def)
				}
				tw.Flush()
			}

			if e.Policy != nil {
				fmt.Fprintln(out, "Policy (advisory, per registry CI; vaka recomputes locally on get):")
				for svc, act := range e.Policy.DefaultActions {
					fmt.Fprintf(out, "  %s: %s\n", svc, act)
				}
				if len(e.Policy.RiskFlags) > 0 {
					fmt.Fprintf(out, "  RISK FLAGS: %s\n", strings.Join(e.Policy.RiskFlags, ", "))
				}
			}
			fmt.Fprintf(out, "Install:     vaka get %s\n", args[0])
			return nil
		},
	}
}
