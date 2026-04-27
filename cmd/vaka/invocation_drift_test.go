package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestComposeGlobalFlagsWithValueDrift(t *testing.T) {
	out, err := exec.Command("docker", "compose", "--help").CombinedOutput()
	if err != nil {
		t.Skipf("docker compose --help unavailable: %v", err)
	}

	fromHelp := parseComposeHelpValueFlags(string(out))
	if len(fromHelp) == 0 {
		t.Fatalf("could not parse value-taking compose globals from docker compose --help")
	}

	// Vaka intentionally rejects Docker top-level globals in argv, even where
	// compose also exposes them.
	ignored := map[string]bool{
		"--context": true,
		"-c":        true,
	}

	for flag := range composeGlobalFlagsWithValue {
		if !fromHelp[flag] {
			t.Fatalf("compose global flag table contains %q but docker compose --help does not mark it as value-taking", flag)
		}
	}
	for flag := range fromHelp {
		if ignored[flag] {
			continue
		}
		if !composeGlobalFlagsWithValue[flag] {
			t.Fatalf("docker compose --help marks %q as value-taking but parser table is missing it", flag)
		}
	}
}

func parseComposeHelpValueFlags(help string) map[string]bool {
	valueTypeTokens := map[string]bool{
		"string":      true,
		"stringArray": true,
		"int":         true,
		"float":       true,
		"duration":    true,
	}

	flags := map[string]bool{}
	lines := strings.Split(help, "\n")
	inOptions := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Options:") {
			inOptions = true
			continue
		}
		if !inOptions {
			continue
		}
		if !strings.HasPrefix(trimmed, "-") {
			// End of options block once the next section starts.
			if strings.HasSuffix(trimmed, ":") {
				break
			}
			continue
		}

		parts := strings.Fields(trimmed)
		if len(parts) == 0 {
			continue
		}

		flagTokens := []string{}
		i := 0
		for ; i < len(parts); i++ {
			p := parts[i]
			if strings.HasPrefix(p, "-") || strings.HasSuffix(p, ",") {
				flagTokens = append(flagTokens, strings.TrimSuffix(p, ","))
				continue
			}
			break
		}
		if len(flagTokens) == 0 || i >= len(parts) {
			continue
		}
		valueType := parts[i]
		if !valueTypeTokens[valueType] {
			continue
		}
		for _, f := range flagTokens {
			if f == "" {
				continue
			}
			flags[f] = true
		}
	}
	return flags
}
