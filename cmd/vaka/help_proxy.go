package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

var dockerComposeHelpOutput = func() ([]byte, error) {
	return exec.Command("docker", "compose", "--help").CombinedOutput()
}

func configureRootHelp(root *cobra.Command) {
	defaultHelpFn := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		defaultHelpFn(cmd, args)
		if cmd != root {
			return
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out)
		fmt.Fprintln(out, proxiedComposeCommandsHelpSection())
	})
}

func proxiedComposeCommandsHelpSection() string {
	lines, err := discoverComposeCommandHelpLines()
	if err != nil || len(lines) == 0 {
		return "Proxied docker compose commands: unavailable; run `docker compose --help`."
	}

	var b strings.Builder
	b.WriteString("Proxied docker compose commands (from `docker compose --help`):\n")
	for _, line := range lines {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func discoverComposeCommandHelpLines() ([]string, error) {
	out, err := dockerComposeHelpOutput()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	inCommands := false
	commands := make([]string, 0, 32)

	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			if inCommands && len(commands) > 0 {
				break
			}
			continue
		}
		if !inCommands {
			if strings.HasPrefix(trimmed, "Commands:") {
				inCommands = true
			}
			continue
		}

		if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "-") {
			break
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		cmdName := fields[0]
		if strings.HasPrefix(cmdName, "-") {
			continue
		}
		desc := strings.TrimSpace(strings.TrimPrefix(trimmed, cmdName))
		if desc == "" {
			commands = append(commands, cmdName)
			continue
		}
		commands = append(commands, fmt.Sprintf("%-14s %s", cmdName, desc))
	}
	if len(commands) == 0 {
		return nil, fmt.Errorf("could not parse docker compose command list")
	}
	return commands, nil
}
