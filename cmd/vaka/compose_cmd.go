// cmd/vaka/compose_cmd.go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

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
	if err := rejectComposeDryRun(inv); err != nil {
		return err
	}

	// No subcommand (`vaka compose`, `vaka compose --help`, globals only) and
	// explicit `<verb> --help` forms proxy straight to docker compose without
	// override injection.
	if inv.Subcommand == "" || isProxySubcommandHelp(inv) {
		return execDockerComposeFn(inv, "", nil)
	}
	spec := composeCommandSpecFor(inv.Subcommand)
	if err := validateConsumedComposeBooleans(inv, spec); err != nil {
		return err
	}
	switch spec.class {
	case verbMetadata:
		return execDockerComposeFn(inv, "", nil)
	case verbRender:
		// One-line notice while an instantiated recipe deviates from its
		// published version (design §6): the render verbs are the moment
		// trust is exercised.
		printDeviationNotice(os.Stderr, inv.ProjectDirectory)
		return runFull(root.VakaFile, inv, root.VakaInitPresent, root.PullPolicy)
	case verbReference:
		requiresValidation, err := referenceRequiresExecutionValidation(inv)
		if err != nil {
			return err
		}
		if requiresValidation {
			if err := validateReferenceExecutionSurfaces(root.VakaFile, inv); err != nil {
				return err
			}
		}
		if spec.ensureRuntime && !root.VakaInitPresent {
			if err := ensureReferenceRuntime(inv); err != nil {
				return err
			}
		}
		return runReference(inv)
	case verbExec:
		return runSecureExec(root.VakaFile, inv)
	case verbUnknown:
		return fmt.Errorf("docker compose command %q has not been reviewed for Vaka's process security boundary; upgrade Vaka for support", inv.Subcommand)
	default:
		return fmt.Errorf("internal error: unhandled compose command class for %q", inv.Subcommand)
	}
}

func rejectComposeDryRun(inv *ComposeInvocation) error {
	state, err := scanComposeBoolOption(inv.PreSubcommand, "--dry-run", "")
	if err != nil {
		return err
	}

	if isProxySubcommandHelp(inv) {
		if state.enabled {
			return unsupportedComposeDryRunError()
		}
		return nil
	}

	local := composeBoolOptionState{}
	switch inv.Subcommand {
	case "exec":
		parsed, err := parseExec(inv.PostSubcommand)
		if err != nil {
			return err
		}
		local = composeBoolOptionState{present: parsed.dryRunSet, enabled: parsed.dryRun}
	case "run":
		parsed, err := parseRun(inv.PostSubcommand)
		if err != nil {
			return err
		}
		local = composeBoolOptionState{present: parsed.dryRunSet, enabled: parsed.dryRun}
	default:
		local, err = scanComposeBoolOption(inv.PostSubcommand, "--dry-run", "")
		if err != nil {
			return err
		}
	}
	if local.present {
		state = local
	}
	if state.enabled {
		return unsupportedComposeDryRunError()
	}
	return nil
}

func unsupportedComposeDryRunError() error {
	return fmt.Errorf("compose --dry-run is not supported by Vaka-controlled commands because Vaka performs security preparation outside Compose; use raw `docker compose --dry-run` for an explicit simulation")
}

func validateConsumedComposeBooleans(inv *ComposeInvocation, spec composeCommandSpec) error {
	if spec.class == verbRender {
		if inv.Subcommand == "run" {
			_, err := parseRun(inv.PostSubcommand)
			return err
		}
		for _, option := range []string{"--build", "--no-build"} {
			if _, err := composeBoolOptionEnabled(inv.PostSubcommand, option, ""); err != nil {
				return err
			}
		}
	}
	if inv.Subcommand == "up" || inv.Subcommand == "create" {
		if _, err := composeBoolOptionEnabled(inv.PostSubcommand, "--no-recreate", ""); err != nil {
			return err
		}
	}
	if inv.Subcommand == "watch" {
		if _, err := composeBoolOptionEnabled(inv.PostSubcommand, "--no-up", ""); err != nil {
			return err
		}
	}
	if inv.Subcommand == "rm" {
		if _, err := composeBoolOptionEnabled(inv.PostSubcommand, "--stop", "s"); err != nil {
			return err
		}
	}
	return nil
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
