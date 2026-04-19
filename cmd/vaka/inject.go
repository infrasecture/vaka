// cmd/vaka/inject.go
package main

import (
	"os"
	"path/filepath"
	"strings"
)

// vakaFlagsTakingValue lists --vaka-* flags that consume the next token as their value.
var vakaFlagsTakingValue = map[string]bool{
	"--vaka-file": true,
}

// vakaFlagsBool lists --vaka-* boolean flags (no value token consumed).
var vakaFlagsBool = map[string]bool{
	"--vaka-init-present": true,
}

// extractVakaFlags splits raw os.Args[1:] into vaka-specific flags (returned as
// a map of flag→value) and the remaining compose-destined args.
// Only flags in vakaFlagsTakingValue or vakaFlagsBool are recognised; unknown
// --vaka-* flags are left in rest so docker compose can reject them with a
// clear error.
func extractVakaFlags(argv []string) (flags map[string]string, rest []string) {
	flags = make(map[string]string)
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if vakaFlagsTakingValue[arg] {
			if i+1 < len(argv) {
				flags[arg] = argv[i+1]
				i++ // consume value token
			}
			continue
		}
		if vakaFlagsBool[arg] {
			flags[arg] = "true"
			continue
		}
		rest = append(rest, arg)
	}
	return flags, rest
}

// discoverComposeFiles returns the default compose files that Docker Compose
// would load from dir when no explicit -f flags are given, in the order they
// would be merged (primary first, then override).
func discoverComposeFiles(dir string) []string {
	var found []string

	primaries := []string{"docker-compose.yaml", "docker-compose.yml"}
	for _, name := range primaries {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
			break
		}
	}

	overrides := []string{"docker-compose.override.yaml", "docker-compose.override.yml"}
	for _, name := range overrides {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
			break
		}
	}

	return found
}

// injectStdinOverride takes dockerArgs (already prefixed with "compose") and
// inserts "-f -" as the last -f argument so the vaka override YAML (piped on
// stdin) wins over all other compose files.
//
// defaults is the list of compose files to inject when the user supplied no
// explicit -f flags (output of discoverComposeFiles). Pass nil only when -f
// flags are already present in dockerArgs. Callers must NOT pass nil defaults
// when there are also no -f flags in dockerArgs — that case must be caught by
// the caller and returned as an error before calling injectStdinOverride.
func injectStdinOverride(dockerArgs []string, defaults []string) []string {
	// Find the position of the value token belonging to the last -f/--file
	// before any -- end-of-options marker.
	lastFileValueIdx := -1
	for i := 1; i < len(dockerArgs); i++ {
		tok := dockerArgs[i]
		if tok == "--" {
			break
		}
		if tok == "-f" || tok == "--file" {
			if i+1 < len(dockerArgs) {
				lastFileValueIdx = i + 1
				i++ // skip value token
			}
		} else if strings.HasPrefix(tok, "--file=") {
			lastFileValueIdx = i
		}
	}

	out := make([]string, 0, len(dockerArgs)+len(defaults)*2+2)

	if lastFileValueIdx >= 0 {
		// Insert "-f", "-" immediately after the last file value token.
		out = append(out, dockerArgs[:lastFileValueIdx+1]...)
		out = append(out, "-f", "-")
		out = append(out, dockerArgs[lastFileValueIdx+1:]...)
	} else {
		// No explicit -f: insert discovered defaults then "-f", "-" at index 1
		// (right after "compose", before any subcommand or other flags).
		out = append(out, dockerArgs[0]) // "compose"
		for _, f := range defaults {
			out = append(out, "-f", f)
		}
		out = append(out, "-f", "-")
		out = append(out, dockerArgs[1:]...)
	}

	return out
}

// composeGlobalFlagsWithValue is the set of docker compose global flags that
// consume the next token as their value. Both allFileFlags and findSubcmd use
// this to skip value tokens when scanning for the subcommand boundary.
//
// Keep in sync with: docker compose --help (global options section).
// Flags known to take a value as of Docker Compose v2:
//
//	-f/--file, -p/--project-name, --profile, --env-file,
//	--project-directory, --parallel, --context/-c, --ansi, --progress
var composeGlobalFlagsWithValue = map[string]bool{
	"-f": true, "--file": true,
	"-p": true, "--project-name": true,
	"--profile":           true,
	"--env-file":          true,
	"--project-directory": true,
	"--parallel":          true,
	"--context":           true,
	"-c":                  true,
	"--ansi":              true,
	"--progress":          true,
}

// allFileFlags returns all -f / --file values from args that appear before
// the subcommand boundary, in order. Scanning stops at -- or at the first
// bare-word token (the subcommand); compose global flags only appear before
// the subcommand, so any -f after it belongs to the subcommand or downstream
// command (e.g. the command run by "docker compose run").
func allFileFlags(args []string) []string {
	var files []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		if tok == "-f" || tok == "--file" {
			if i+1 < len(args) {
				files = append(files, args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(tok, "--file=") {
			files = append(files, strings.TrimPrefix(tok, "--file="))
			continue
		}
		// Other global flags that take a value: skip their value token.
		if composeGlobalFlagsWithValue[tok] {
			i++
			continue
		}
		// --flag=value: no separate value token.
		if strings.HasPrefix(tok, "--") && strings.Contains(tok, "=") {
			continue
		}
		// Boolean flag.
		if strings.HasPrefix(tok, "-") {
			continue
		}
		// First bare-word token is the subcommand: stop here.
		break
	}
	return files
}

// findSubcmd returns the first non-flag, non-value token from args (the compose
// subcommand). Returns "" if no subcommand is found.
func findSubcmd(args []string) string {
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		if composeGlobalFlagsWithValue[tok] {
			i++ // skip value token
			continue
		}
		if strings.HasPrefix(tok, "--") && strings.Contains(tok, "=") {
			continue // --flag=value: no separate value token
		}
		if strings.HasPrefix(tok, "-") {
			continue // boolean flag
		}
		return tok
	}
	return ""
}
