// cmd/vaka/search.go
package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search [term]",
		Short: "Search recipes across all configured registries",
		Long: `Search recipe names, descriptions, and tags in the cached registry
catalogs. The RISK column counts the registry-reported risk flags of the
newest version ('-' none, '?' not reported); vaka recomputes them locally
on every get.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			term := ""
			if len(args) == 1 {
				term = strings.ToLower(args[0])
			}
			world, err := loadRegistryWorld(browseIndexMaxAge, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tVERSION\tRISK\tDESCRIPTION")
			matches := 0
			for _, row := range world.catalogRows() {
				if term != "" && !matchesTerm(term, row) {
					continue
				}
				matches++
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					world.displayName(row), row.entry.Version, riskColumn(row.entry), row.entry.Description)
			}
			tw.Flush()
			if matches == 0 {
				return fmt.Errorf("no recipes match %q in the configured registries", term)
			}
			return nil
		},
	}
}

func matchesTerm(term string, row catalogRow) bool {
	if strings.Contains(strings.ToLower(row.name), term) ||
		strings.Contains(strings.ToLower(row.entry.Description), term) {
		return true
	}
	for _, tag := range row.entry.Tags {
		if strings.Contains(strings.ToLower(tag), term) {
			return true
		}
	}
	return false
}
