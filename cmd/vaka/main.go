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
		// The up/run/create/volumes/down/stop/kill/rm stubs exist only for --help
		// visibility. Actual execution is handled by the manual dispatch switch
		// below and never reaches these cobra commands.
		&cobra.Command{
			Use:                "up [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose up",
			Long:               "Use --vaka-init-present to skip __vaka-init container injection (binaries baked into image).",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "run [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose run",
			Long:               "Use --vaka-init-present to skip __vaka-init container injection (binaries baked into image).",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "create [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose create",
			Long:               "Use --vaka-init-present to skip __vaka-init container injection (binaries baked into image).",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "volumes [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose volumes",
			Long:               "Uses the same full injection path as up/run/create so __vaka-init resources are visible.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "down [compose-flags...]",
			Short:              "Tear down the stack including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "stop [compose-flags...]",
			Short:              "Stop services including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "kill [compose-flags...]",
			Short:              "Kill services including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "rm [compose-flags...]",
			Short:              "Remove stopped containers including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
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

	// Step 1: Extract vaka-specific flags (--vaka-file, --vaka-init-present).
	vakaFlags, rest := extractVakaFlags(raw)
	vakaFile := vakaFlags["--vaka-file"]
	if vakaFile == "" {
		vakaFile = "vaka.yaml"
	}
	vakaInitPresent := vakaFlags["--vaka-init-present"] == "true"

	// Step 2: Find the subcommand (first non-flag, non-value token).
	subcmd := findSubcmd(rest)

	// Step 3: Three-path dispatch.
	switch classifySubcmd(subcmd) {
	case pathCobra:
		// cobra-handled commands (validate, show, version, empty).
		// SetArgs so cobra sees a clean argv (--vaka-* already stripped).
		rootCmd.SetArgs(rest)
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}

	case pathFull:
		// Full-override path: up, run, create.
		if err := runFull(vakaFile, rest, vakaInitPresent); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}

	case pathLifecycle:
		// Lifecycle path: down, stop, kill, rm.
		if err := runLifecycle(rest, vakaInitPresent); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}

	default: // pathPassthrough
		// Pure passthrough: forward unchanged to docker compose.
		if err := execDockerCompose(rest, "", nil); err != nil {
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
