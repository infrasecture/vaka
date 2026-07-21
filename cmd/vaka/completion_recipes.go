// cmd/vaka/completion_recipes.go
package main

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// completeRecipeRefs completes recipe references for `get` and `recipes info`
// from cached indexes only (no network — completion must be fast). Names are
// qualified (registry/name) when more than one registry is configured, so the
// completion matches the reference the command would resolve.
func completeRecipeRefs(toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := loadRegistriesConfig()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client := newRegistryClient(0)
	qualify := len(cfg.Registries) > 1

	seen := map[string]bool{}
	var out []string
	for _, reg := range cfg.Registries {
		idx, ok := client.CachedIndex(reg)
		if !ok {
			continue
		}
		for name := range idx.Recipes {
			ref := name
			if qualify {
				ref = reg.Name + "/" + name
			}
			if !seen[ref] && strings.HasPrefix(ref, toComplete) {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeRegistryNames completes configured registry names (for `registry
// remove` / `refresh`).
func completeRegistryNames(toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := loadRegistriesConfig()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, reg := range cfg.Registries {
		if strings.HasPrefix(reg.Name, toComplete) {
			out = append(out, reg.Name)
		}
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

// firstArgComplete returns a ValidArgsFunction that runs fn only for the first
// positional argument and otherwise offers nothing (no file fallback).
func firstArgComplete(fn func(toComplete string) ([]string, cobra.ShellCompDirective)) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return fn(toComplete)
	}
}
