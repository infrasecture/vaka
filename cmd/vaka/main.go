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
		newShowCmd(),
		// up and run stubs exist only for --help visibility.
		// Actual execution is handled by the manual dispatch switch below
		// and never reaches these cobra commands.
		&cobra.Command{
			Use:                "up [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose up",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "run [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose run",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:   "version",
			Short: "Print version",
			Run: func(cmd *cobra.Command, args []string) {
				fmt.Println("vaka", version)
			},
		},
	)

	raw := os.Args[1:]

	// Step 1: Extract vaka-specific flags (--vaka-file).
	vakaFlags, rest := extractVakaFlags(raw)
	vakaFile := vakaFlags["--vaka-file"]
	if vakaFile == "" {
		vakaFile = "vaka.yaml"
	}

	// Step 2: Find the subcommand (first non-flag, non-value token).
	subcmd := findSubcmd(rest)

	// Step 3: Route.
	switch subcmd {
	case "validate", "show", "version", "":
		// cobra-handled commands. SetArgs so cobra sees a clean argv
		// (--vaka-file already stripped by extractVakaFlags).
		rootCmd.SetArgs(rest)
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}

	case "up", "run":
		// Injection path: validate vaka policy and inject -f - into compose argv.
		if err := runInjection(vakaFile, rest); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}

	default:
		// Pure passthrough: prepend "compose" and exec docker verbatim.
		c := exec.Command("docker", append([]string{"compose"}, rest...)...)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			os.Exit(exitCode(err))
		}
	}
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
