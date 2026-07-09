// cmd/vaka/completion.go
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion",
		Short: "Generate the autocompletion script for the specified shell",
		Long:  "Generate the autocompletion script for vaka for the specified shell.",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:                   "bash",
			Short:                 "Generate the autocompletion script for bash",
			Args:                  cobra.NoArgs,
			DisableFlagsInUseLine: true,
			ValidArgsFunction:     cobra.NoFileCompletions,
			RunE: func(cmd *cobra.Command, args []string) error {
				var buf bytes.Buffer
				if err := cmd.Root().GenBashCompletionV2(&buf, true); err != nil {
					return err
				}
				script, err := pinBashCompletionRequestCommand(buf.String())
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write([]byte(script))
				return err
			},
		},
		&cobra.Command{
			Use:               "zsh",
			Short:             "Generate the autocompletion script for zsh",
			Args:              cobra.NoArgs,
			ValidArgsFunction: cobra.NoFileCompletions,
			RunE: func(cmd *cobra.Command, args []string) error {
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use:               "fish",
			Short:             "Generate the autocompletion script for fish",
			Args:              cobra.NoArgs,
			ValidArgsFunction: cobra.NoFileCompletions,
			RunE: func(cmd *cobra.Command, args []string) error {
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			},
		},
		&cobra.Command{
			Use:               "powershell",
			Short:             "Generate the autocompletion script for powershell",
			Args:              cobra.NoArgs,
			ValidArgsFunction: cobra.NoFileCompletions,
			RunE: func(cmd *cobra.Command, args []string) error {
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			},
		},
	)

	return cmd
}

func pinBashCompletionRequestCommand(script string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable for bash completion: %w", err)
	}
	from := `requestComp="${words[0]} __complete ${args[*]}"`
	to := fmt.Sprintf(`requestComp="%s __complete ${args[*]}"`, shellQuote(exe))
	out := strings.Replace(script, from, to, 1)
	if out == script {
		return "", fmt.Errorf("patch bash completion request command: template marker not found")
	}
	return out, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
