// cmd/vaka/compose_cmd.go
package main

import (
	"fmt"
	"os"
	"strings"

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
		if err := runReference(inv); err != nil {
			return err
		}
		if spec.ensureRuntime && !root.VakaInitPresent {
			return ensureReferenceRuntime(inv)
		}
		return nil
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
		switch inv.Subcommand {
		case "run":
			if _, err := parseRun(inv.PostSubcommand); err != nil {
				return err
			}
			if _, _, err := composePullOption(inv); err != nil {
				return err
			}
		case "up", "create":
			if _, err := scanCreateServiceTargets(inv); err != nil {
				return err
			}
			if err := validateCreateComposeOptions(inv); err != nil {
				return err
			}
			for _, option := range []string{"--build", "--no-build"} {
				if _, err := composeBoolOptionEnabled(inv.PostSubcommand, option, ""); err != nil {
					return err
				}
			}
			if _, _, err := composePullOption(inv); err != nil {
				return err
			}
		case "watch":
			if err := rejectUnsupportedImageRefreshOptions(inv); err != nil {
				return err
			}
			if _, err := scanWatchServiceTargets(inv.PostSubcommand); err != nil {
				return err
			}
		case "scale":
			if err := rejectUnsupportedImageRefreshOptions(inv); err != nil {
				return err
			}
			if _, err := scanScaleServiceTargets(inv); err != nil {
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

func validateCreateComposeOptions(inv *ComposeInvocation) error {
	enabled := func(long, short string) (bool, error) {
		return composeBoolOptionEnabled(inv.PostSubcommand, long, short)
	}
	build, err := enabled("--build", "")
	if err != nil {
		return err
	}
	noBuild, err := enabled("--no-build", "")
	if err != nil {
		return err
	}
	if build && noBuild {
		return fmt.Errorf("compose options --build and --no-build are incompatible")
	}
	forceRecreate, err := enabled("--force-recreate", "")
	if err != nil {
		return err
	}
	noRecreate, err := enabled("--no-recreate", "")
	if err != nil {
		return err
	}
	if forceRecreate && noRecreate {
		return fmt.Errorf("compose options --force-recreate and --no-recreate are incompatible")
	}
	if inv.Subcommand == "create" {
		return nil
	}

	abortStop, err := enabled("--abort-on-container-exit", "")
	if err != nil {
		return err
	}
	abortFail, err := enabled("--abort-on-container-failure", "")
	if err != nil {
		return err
	}
	if composeValueOptionPresent(inv.PostSubcommand, "--exit-code-from") && !abortFail {
		abortStop = true
	}
	if abortStop && abortFail {
		return fmt.Errorf("compose options --abort-on-container-failure and --abort-on-container-exit are incompatible")
	}
	attachDeps, err := enabled("--attach-dependencies", "")
	if err != nil {
		return err
	}
	hasAttach := composeValueOptionPresent(inv.PostSubcommand, "--attach")
	if hasAttach && attachDeps {
		return fmt.Errorf("compose options --attach and --attach-dependencies are incompatible")
	}
	wait, err := enabled("--wait", "")
	if err != nil {
		return err
	}
	if wait && (abortStop || hasAttach || attachDeps) {
		return fmt.Errorf("compose option --wait cannot be combined with --abort-on-container-exit, --attach, or --attach-dependencies")
	}
	detach, err := enabled("--detach", "d")
	if err != nil {
		return err
	}
	watch, err := enabled("--watch", "w")
	if err != nil {
		return err
	}
	if wait && (abortFail || watch) {
		return fmt.Errorf("compose option --wait cannot be combined with --abort-on-container-failure or --watch")
	}
	if detach && (attachDeps || abortStop || abortFail || hasAttach || watch) {
		return fmt.Errorf("compose option --detach cannot be combined with --abort-on-container-exit, --abort-on-container-failure, --attach, --attach-dependencies, or --watch")
	}
	renewVolumes, err := enabled("--renew-anon-volumes", "V")
	if err != nil {
		return err
	}
	if renewVolumes && noRecreate {
		return fmt.Errorf("compose options --no-recreate and --renew-anon-volumes are incompatible")
	}
	recreateDeps, err := enabled("--always-recreate-deps", "")
	if err != nil {
		return err
	}
	if recreateDeps && noRecreate {
		return fmt.Errorf("compose options --always-recreate-deps and --no-recreate are incompatible")
	}
	if noBuild && watch {
		return fmt.Errorf("compose options --no-build and --watch are incompatible")
	}
	return nil
}

func composeValueOptionPresent(args []string, option string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return false
		}
		if args[i] == option {
			return true
		}
		if strings.HasPrefix(args[i], option+"=") {
			return true
		}
	}
	return false
}

func rejectUnsupportedImageRefreshOptions(inv *ComposeInvocation) error {
	for _, tok := range inv.PostSubcommand {
		if tok == "--" {
			break
		}
		name, _, _ := strings.Cut(tok, "=")
		switch name {
		case "--build", "--no-build", "--pull":
			return fmt.Errorf("compose %s does not support image refresh option %s", inv.Subcommand, name)
		}
	}
	return nil
}

// isProxySubcommandHelp reports whether the invocation is `<subcommand> --help`
// (or -h), which vaka proxies to docker compose without override injection.
func isProxySubcommandHelp(inv *ComposeInvocation) bool {
	if inv.GlobalHelp || inv.GlobalVersion {
		return true
	}
	for i := 0; i < len(inv.PostSubcommand); i++ {
		tok := inv.PostSubcommand[i]
		if tok == "--" {
			return false
		}
		if tok == "--help" || tok == "-h" {
			return true
		}
		// run and exec disable interspersed flag parsing. Once SERVICE is
		// reached, --help belongs to the requested command, not Compose.
		if (inv.Subcommand == "run" || inv.Subcommand == "exec") && (!strings.HasPrefix(tok, "-") || tok == "-") {
			return false
		}
		valueOptions := map[string]bool(nil)
		switch inv.Subcommand {
		case "run":
			valueOptions = runOptionsWithValue
		case "exec":
			valueOptions = execOptionsWithValue
		}
		if valueOptions != nil {
			if _, _, consumed, _, ok := parseValueTakingToken(inv.PostSubcommand, i, valueOptions); ok {
				i += consumed - 1
			}
		}
	}
	return false
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
