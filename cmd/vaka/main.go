// cmd/vaka/main.go
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:   "vaka",
	Short: "Secure container layer for AI agentic harnesses",
	Long: `vaka enforces nftables egress policy inside Docker containers running
AI agentic harnesses. Run 'vaka up' instead of 'docker compose up'.`,
	SilenceUsage: true,
}

func main() {
	rootCmd.AddCommand(
		newValidateCmd(),
		newShowNftCmd(),
		newDoctorCmd(),
		newShowComposeCmd(),
		&cobra.Command{
			Use:   "version",
			Short: "Print version",
			Run: func(cmd *cobra.Command, args []string) {
				fmt.Println("vaka", version)
			},
		},
	)
	configureRootHelp(rootCmd)

	raw := os.Args[1:]
	inv, err := ParseInvocation(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaka:", err)
		os.Exit(1)
	}

	// Step 1: Extract vaka-specific flags (--vaka-file, --vaka-init-present).
	vakaFile := inv.VakaFlags["--vaka-file"]
	if vakaFile == "" {
		vakaFile = "vaka.yaml"
	}
	vakaInitPresent := inv.VakaFlags["--vaka-init-present"] == "true"

	if inv.Subcommand == "show-compose" {
		if isProxySubcommandHelp(inv) {
			printShowComposeHelp(os.Stdout)
			return
		}
		if err := runShowCompose(vakaFile, inv, vakaInitPresent); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}
		return
	}

	// Dispatch by parsed subcommand path.
	switch classifySubcmd(inv.Subcommand) {
	case pathNative:
		// cobra-handled commands (validate, show-nft, doctor, version, help/completion).
		// SetArgs so cobra sees a clean argv (--vaka-* already stripped).
		rootCmd.SetArgs(inv.ComposeArgs)
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}

	case pathRender:
		if isProxySubcommandHelp(inv) {
			if err := execDockerCompose(inv, "", nil); err != nil {
				os.Exit(exitCode(err))
			}
			return
		}
		// Full-override path: up, run, create.
		if err := runFull(vakaFile, inv, vakaInitPresent); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}

	case pathReference:
		if isProxySubcommandHelp(inv) {
			if err := execDockerCompose(inv, "", nil); err != nil {
				os.Exit(exitCode(err))
			}
			return
		}
		if err := runReference(inv, vakaInitPresent); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}
	}
}

func isProxySubcommandHelp(inv *Invocation) bool {
	if len(inv.PostSubcommand) == 0 {
		return false
	}
	first := inv.PostSubcommand[0]
	return first == "--help" || first == "-h"
}

// exitCode extracts the process exit code from an *exec.ExitError so that
// vaka propagates docker's exit code faithfully.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
