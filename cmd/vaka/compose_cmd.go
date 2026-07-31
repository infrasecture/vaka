// cmd/vaka/compose_cmd.go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// composeMetadataVerbs are compose subcommands that never interact with a
// compose project. They are proxied without any vaka override so they work
// outside project directories.
var composeMetadataVerbs = map[string]bool{
	"version": true,
	"ls":      true,
}

// newComposeCmd exposes the full docker compose surface under `vaka compose`.
// Flag parsing is disabled so the compose payload reaches the engine verbatim;
// note that with DisableFlagParsing cobra also stops handling -h/--help, so
// help forms are routed through the docker compose help proxy instead.
func newComposeCmd(root *RootInvocation) *cobra.Command {
	cmd := &cobra.Command{
		Use:                "compose [compose-global-flags...] COMMAND [ARGS...]",
		Short:              "Run any docker compose command with vaka policy injection",
		GroupID:            groupCompose,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runComposeCLI(root, args)
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.SetHelpFunc(proxyComposeHelp(""))
	return cmd
}

// runComposeCLI is the single execution entry point for `vaka compose ...`
// and every top-level shorthand. argv is the compose payload: compose global
// flags, the compose subcommand, and its args.
func runComposeCLI(root *RootInvocation, argv []string) error {
	inv, err := ParseComposeInvocation(argv)
	if err != nil {
		return err
	}

	// No subcommand (`vaka compose`, `vaka compose --help`, globals only) and
	// explicit `<verb> --help` forms proxy straight to docker compose without
	// override injection.
	if inv.Subcommand == "" || isProxySubcommandHelp(inv) {
		return execDockerComposeFn(inv, "", nil)
	}
	if composeMetadataVerbs[inv.Subcommand] {
		return execDockerComposeFn(inv, "", nil)
	}

	switch classifyComposeVerb(inv.Subcommand) {
	case verbRender:
		// One-line notice while an instantiated recipe deviates from its
		// published version (design §6): the render verbs are the moment
		// trust is exercised.
		printDeviationNotice(os.Stderr, inv.ProjectDirectory)
		return runFull(root.VakaFile, inv, root.VakaInitPresent, root.PullPolicy)
	default:
		return runReference(inv)
	}
}

// isProxySubcommandHelp reports whether the invocation is `<subcommand> --help`
// (or -h), which vaka proxies to docker compose without override injection.
func isProxySubcommandHelp(inv *ComposeInvocation) bool {
	if len(inv.PostSubcommand) == 0 {
		return false
	}
	first := inv.PostSubcommand[0]
	return first == "--help" || first == "-h"
}

// proxyComposeHelp returns a cobra help func that proxies to
// `docker compose [verb] --help`. It is wired via SetHelpFunc so `vaka help up`
// and `vaka help compose` show docker's help for the proxied surface.
func proxyComposeHelp(verb string) func(*cobra.Command, []string) {
	return func(cmd *cobra.Command, _ []string) {
		argv := []string{}
		if verb != "" {
			argv = append(argv, verb)
		}
		argv = append(argv, "--help")
		inv, err := ParseComposeInvocation(argv)
		if err == nil {
			err = execDockerComposeFn(inv, "", nil)
		}
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "vaka: cannot proxy docker compose help: %v\n", err)
		}
	}
}
