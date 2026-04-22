// cmd/vaka/show_compose.go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// runShowCompose builds the same compose override as runFull and prints it to
// stdout, or writes it to a file when -o/--output is provided.
func runShowCompose(vakaFile string, args []string, vakaInitPresent bool) error {
	output, passthrough, err := parseShowComposeFlags(args)
	if err != nil {
		return err
	}

	ctx := context.Background()
	ds, err := newDockerServices()
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
func parseShowComposeFlags(args []string) (output string, passthrough []string, err error) {
	if findSubcmd(args) != "show-compose" {
		return "", nil, fmt.Errorf("show-compose: subcommand not found")
	}
	subcmdIdx := findSubcmdIndex(args)
	if subcmdIdx < 0 {
		return "", nil, fmt.Errorf("show-compose: subcommand not found")
	}

	passthrough = append(passthrough, args[:subcmdIdx+1]...)

	for i := subcmdIdx + 1; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "-o" || tok == "--output":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a value", tok)
			}
			output = args[i+1]
			i++
		case strings.HasPrefix(tok, "--output="):
			output = strings.TrimPrefix(tok, "--output=")
			if strings.TrimSpace(output) == "" {
				return "", nil, fmt.Errorf("--output requires a value")
			}
		case tok == "--":
			passthrough = append(passthrough, args[i:]...)
			return output, passthrough, nil
		case tok == "--build":
			// Keep --build so hasBuildFlag() can force prebuild parity with runFull.
			passthrough = append(passthrough, tok)
		case strings.HasPrefix(tok, "-"):
			return "", nil, fmt.Errorf("unknown show-compose flag: %s", tok)
		default:
			passthrough = append(passthrough, tok)
		}
	}

	return output, passthrough, nil
}
