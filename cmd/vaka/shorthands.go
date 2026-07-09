// cmd/vaka/shorthands.go
package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newShorthandCmds builds the permanent top-level shorthand commands. Each is
// pure delegation: `vaka up [args...]` executes exactly like
// `vaka compose up [args...]`. Compose global flags (-f, -p, --profile, ...)
// are not accepted at the top level; they require the `vaka compose` form.
func newShorthandCmds(root *RootInvocation) []*cobra.Command {
	cmds := make([]*cobra.Command, 0, len(composeShorthands))
	for _, name := range composeShorthands {
		name := name
		cmd := &cobra.Command{
			Use:                fmt.Sprintf("%s [args...]", name),
			Short:              fmt.Sprintf("Shorthand for `vaka compose %s`", name),
			GroupID:            groupCompose,
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runComposeCLI(root, append([]string{name}, args...))
			},
			ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
				return nil, cobra.ShellCompDirectiveNoFileComp
			},
		}
		cmd.SetHelpFunc(proxyComposeHelp(name))
		cmds = append(cmds, cmd)
	}
	return cmds
}
