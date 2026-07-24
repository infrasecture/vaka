// cmd/vaka/root.go
package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const (
	groupVaka    = "vaka"
	groupCompose = "compose"
	groupRecipe  = "recipe"
)

// composeShorthands is the permanent set of top-level commands that delegate
// 1:1 to the equivalent `vaka compose <name>` invocation. It is intentionally
// small and fixed: every other compose verb requires the `compose` namespace
// so future vaka-native commands cannot collide with docker compose.
var composeShorthands = []string{"up", "down", "start", "stop", "run", "exec", "logs", "ps"}

// composeVerbBaseline is the built-in list of docker compose subcommands used
// to shape unknown-top-level-command errors. It is a hint table only — compose
// execution never consults it — so staleness is cosmetic. A live list from
// `docker compose --help` supplements it at error time.
var composeVerbBaseline = map[string]bool{
	"attach": true, "build": true, "commit": true, "config": true, "cp": true,
	"create": true, "down": true, "events": true, "exec": true, "export": true,
	"images": true, "kill": true, "logs": true, "ls": true, "pause": true,
	"port": true, "ps": true, "publish": true, "pull": true, "push": true,
	"restart": true, "rm": true, "run": true, "scale": true, "start": true,
	"stats": true, "stop": true, "top": true, "unpause": true, "up": true,
	"version": true, "wait": true, "watch": true,
}

// newRootCmd builds the vaka command tree. root.Rest is consulted to shape
// flag errors for unknown commands such as `vaka pull -q`.
func newRootCmd(root *RootInvocation) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vaka [--vaka-file=<path>] [--vaka-init-present] <command>",
		Short: "Secure container layer for AI agentic harnesses",
		Long: `vaka enforces nftables egress policy inside Docker containers running
AI agentic harnesses. Run 'vaka up' instead of 'docker compose up'.

Full docker compose syntax (compose global flags such as -f, -p, --profile)
lives under 'vaka compose'; run 'vaka compose --help' for the compose surface.

Vaka flags precede the command and value-taking ones require '=' form:
--vaka-file=<path> selects the policy file (default vaka.yaml);
--vaka-init-present skips helper injection for images that bundle vaka-init.`,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		Args:                  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return unknownCommandError(cmd, args[0])
		},
	}
	cmd.SetErrPrefix("vaka:")

	// The root is runnable only so unknown commands get the compose-namespace
	// hint; cobra's default template would therefore print both "vaka [flags]"
	// and "vaka [command]" usage lines. Show a single line: the UseLine for
	// runnable commands, the generic subcommand form otherwise. The literal
	// match keeps this a no-op (default template) if cobra's template changes.
	cmd.SetUsageTemplate(strings.Replace(cmd.UsageTemplate(),
		"Usage:{{if .Runnable}}\n  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}\n  {{.CommandPath}} [command]{{end}}",
		"Usage:{{if .Runnable}}\n  {{.UseLine}}{{else if .HasAvailableSubCommands}}\n  {{.CommandPath}} [command]{{end}}",
		1))

	cmd.AddGroup(
		&cobra.Group{ID: groupVaka, Title: "Vaka Commands:"},
		&cobra.Group{ID: groupRecipe, Title: "Recipe Commands:"},
		&cobra.Group{ID: groupCompose, Title: "Compose Commands:"},
	)

	version := &cobra.Command{
		Use:     "version",
		Short:   "Print version",
		GroupID: groupVaka,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "vaka", version)
		},
	}

	for _, c := range []*cobra.Command{
		newValidateCmd(),
		newShowNftCmd(),
		newDoctorCmd(),
		newShowComposeCmd(root),
		newCompletionCmd(),
		version,
	} {
		c.GroupID = groupVaka
		cmd.AddCommand(c)
	}

	for _, c := range []*cobra.Command{
		newGetCmd(),
		newSearchCmd(),
		newRecipesCmd(),
		newRegistryCmd(),
	} {
		c.GroupID = groupRecipe
		cmd.AddCommand(c)
	}

	cmd.AddCommand(newComposeCmd(root))
	for _, c := range newShorthandCmds(root) {
		cmd.AddCommand(c)
	}

	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		// A flag error on the root command means cobra never resolved a
		// subcommand (e.g. `vaka pull -q`): if the first bare word is a
		// compose verb, explain the namespace instead of the raw flag error.
		if c == cmd {
			for _, tok := range root.Rest {
				if tok == "--" {
					break
				}
				if !strings.HasPrefix(tok, "-") {
					if isComposeVerb(tok) {
						return unknownCommandError(cmd, tok)
					}
					break
				}
			}
		}
		return err
	})

	return cmd
}

// unknownCommandError builds the error for an unrecognized top-level command,
// pointing compose verbs at the `vaka compose` namespace.
func unknownCommandError(cmd *cobra.Command, name string) error {
	if isComposeVerb(name) {
		return fmt.Errorf("unknown command %q: %q is a docker compose command, use `vaka compose %s`\nTop-level compose shorthands: %s",
			name, name, name, strings.Join(composeShorthands, ", "))
	}
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		return fmt.Errorf("unknown command %q for %q\n\nDid you mean this?\n\t%s",
			name, cmd.CommandPath(), strings.Join(suggestions, "\n\t"))
	}
	return fmt.Errorf("unknown command %q for %q; run 'vaka --help' for usage", name, cmd.CommandPath())
}

// isComposeVerb reports whether name looks like a docker compose subcommand.
// The baseline table is supplemented with the live list from
// `docker compose --help` when available.
func isComposeVerb(name string) bool {
	if composeVerbBaseline[name] {
		return true
	}
	verbs, err := discoverComposeVerbs()
	if err != nil {
		return false
	}
	return verbs[name]
}
