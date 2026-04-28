// cmd/vaka/show_compose.go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newShowComposeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show-compose",
		Short: "Print the generated compose override YAML used by vaka injection",
		Long:  "Print the generated compose override YAML used by vaka injection.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.Flags().Bool("build", false, "Pre-build eligible services before resolving image runtime metadata")
	cmd.Flags().StringP("output", "o", "", "Write override YAML to a file instead of stdout")
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		printShowComposeHelp(cmd.OutOrStdout())
	})
	return cmd
}

// runShowCompose builds the same compose override as runFull and prints it to
// stdout, or writes it to a file when -o/--output is provided.
func runShowCompose(vakaFile string, inv *Invocation, vakaInitPresent bool) error {
	output, passthrough, err := parseShowComposeFlags(inv)
	if err != nil {
		return err
	}

	ctx := context.Background()
	ds, err := newDockerServices(passthrough)
	if err != nil {
		return err
	}

	overrideYAML, _, err := buildInjectionOverride(ctx, ds, vakaFile, passthrough, vakaInitPresent)
	if err != nil {
		return err
	}

	if output == "" {
		_, err := os.Stdout.WriteString(overrideYAML)
		return err
	}
	return os.WriteFile(output, []byte(overrideYAML), 0o644)
}

// parseShowComposeFlags parses show-compose-specific flags from args.
// It preserves all non-output tokens so the shared builder receives the same
// input shape, while stripping -o/--output from the final passthrough argv.
func parseShowComposeFlags(inv *Invocation) (output string, passthrough *Invocation, err error) {
	if inv.Subcommand != "show-compose" {
		return "", nil, fmt.Errorf("show-compose: subcommand not found")
	}
	subcmdIdx := inv.SubcommandIdx
	if subcmdIdx < 0 {
		return "", nil, fmt.Errorf("show-compose: subcommand not found")
	}

	passthroughArgs := append([]string{}, inv.ComposeArgs[:subcmdIdx+1]...)

	for i := subcmdIdx + 1; i < len(inv.ComposeArgs); i++ {
		tok := inv.ComposeArgs[i]
		switch {
		case tok == "-o" || tok == "--output":
			if i+1 >= len(inv.ComposeArgs) {
				return "", nil, fmt.Errorf("%s requires a value", tok)
			}
			output = inv.ComposeArgs[i+1]
			i++
		case strings.HasPrefix(tok, "--output="):
			output = strings.TrimPrefix(tok, "--output=")
			if strings.TrimSpace(output) == "" {
				return "", nil, fmt.Errorf("--output requires a value")
			}
		case tok == "--":
			passthroughArgs = append(passthroughArgs, inv.ComposeArgs[i:]...)
			parsed, parseErr := ParseInvocation(passthroughArgs)
			if parseErr != nil {
				return "", nil, parseErr
			}
			return output, parsed, nil
		case tok == "--build":
			// Keep --build so Invocation.BuildRequested mirrors runFull behavior.
			passthroughArgs = append(passthroughArgs, tok)
		case strings.HasPrefix(tok, "-"):
			return "", nil, fmt.Errorf("unknown show-compose flag: %s", tok)
		default:
			passthroughArgs = append(passthroughArgs, tok)
		}
	}

	parsed, parseErr := ParseInvocation(passthroughArgs)
	if parseErr != nil {
		return "", nil, parseErr
	}
	return output, parsed, nil
}

func printShowComposeHelp(w io.Writer) {
	fmt.Fprintln(w, "Print the generated compose override YAML used by vaka injection.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  vaka [--vaka-file=<path>] [--vaka-init-present] [compose-global-flags...] show-compose [--build] [-o, --output <path>]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Notes:")
	fmt.Fprintln(w, "  - pass --vaka-file and --vaka-init-present before `show-compose`")
	fmt.Fprintln(w, "  - pass compose global flags before `show-compose`")
	fmt.Fprintln(w, "  - after `show-compose`, only --build and -o/--output are accepted")
	fmt.Fprintln(w, "  - VAKA_<SERVICE>_CONF values are never printed")
}
