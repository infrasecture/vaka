package main

import (
	"fmt"
	"os/exec"
	"strings"
)

var dockerComposeHelpOutput = func() ([]byte, error) {
	return exec.Command("docker", "compose", "--help").CombinedOutput()
}

// discoverComposeVerbs parses the live `docker compose --help` output into the
// set of available compose subcommand names. It is used only to shape
// unknown-command errors; compose execution never depends on it.
func discoverComposeVerbs() (map[string]bool, error) {
	out, err := dockerComposeHelpOutput()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	inCommands := false
	verbs := make(map[string]bool, 32)

	for _, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			if inCommands && len(verbs) > 0 {
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
		verb := fields[0]
		if strings.HasPrefix(verb, "-") {
			continue
		}
		verbs[verb] = true
	}
	if len(verbs) == 0 {
		return nil, fmt.Errorf("could not parse docker compose command list")
	}
	return verbs, nil
}
