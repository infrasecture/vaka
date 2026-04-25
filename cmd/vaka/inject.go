// cmd/vaka/inject.go
package main

import (
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
//
// Recognition scope is bounded so vaka never steals tokens that belong to the
// inner command on `run`/`exec`:
//   - Before the compose subcommand: vaka flags are recognised (e.g.
//     `vaka --vaka-file x up`).
//   - Between the subcommand and its first positional: also recognised
//     (e.g. `vaka up --vaka-init-present`, `vaka run --vaka-init-present svc`).
//   - After the first positional (the service name for run/exec, or the
//     first service for up/down), or after --: tokens pass through
//     untouched. `vaka run gateway mytool --vaka-file cfg.yaml` leaves
//     `--vaka-file cfg.yaml` as args to mytool.
//
// Unknown --vaka-* flags pass through so docker compose can reject them with
// a clear error.
func extractVakaFlags(argv []string) (flags map[string]string, rest []string) {
	flags = make(map[string]string)
	bareWords := 0
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			rest = append(rest, argv[i:]...)
			return flags, rest
		}
		if bareWords >= 2 {
			// Past subcommand and its first positional. Further tokens belong
			// to the subcommand or the inner command — pass through untouched.
			rest = append(rest, argv[i:]...)
			return flags, rest
		}
		if !strings.HasPrefix(tok, "-") {
			// Bare-word: subcommand (first) or positional (second).
			rest = append(rest, tok)
			bareWords++
			continue
		}
		if vakaFlagsTakingValue[tok] {
			if i+1 < len(argv) {
				flags[tok] = argv[i+1]
				i++ // consume value token
			}
			continue
		}
		if vakaFlagsBool[tok] {
			flags[tok] = "true"
			continue
		}
		if composeGlobalFlagsWithValue[tok] {
			rest = append(rest, tok)
			if i+1 < len(argv) {
				rest = append(rest, argv[i+1])
				i++ // consume value token
			}
			continue
		}
		rest = append(rest, tok)
	}
	return flags, rest
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

// globalFlags returns the docker compose global flags (tokens appearing before
// the first bare-word subcommand) from args, preserving their original order.
// Used to prefix an auxiliary compose invocation (e.g. pre-build) so that user
// overrides like -f, --project-name, --profile, etc. are honoured.
func globalFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}
		if composeGlobalFlagsWithValue[tok] {
			if i+1 < len(args) {
				out = append(out, tok, args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(tok, "--") && strings.Contains(tok, "=") {
			out = append(out, tok)
			continue
		}
		if strings.HasPrefix(tok, "-") {
			out = append(out, tok)
			continue
		}
		// Subcommand boundary.
		break
	}
	return out
}

// findSubcmdIndex returns the index of the first non-flag, non-value token
// from args (the compose subcommand). Returns -1 if no subcommand is found.
func findSubcmdIndex(args []string) int {
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
		return i
	}
	return -1
}

// findSubcmd returns the first non-flag, non-value token from args (the compose
// subcommand). Returns "" if no subcommand is found.
func findSubcmd(args []string) string {
	idx := findSubcmdIndex(args)
	if idx < 0 {
		return ""
	}
	return args[idx]
}

// hasBuildFlag reports whether --build appears among the subcommand's own
// flags. It scans tokens after the compose subcommand boundary and stops at --
// (anything after -- is the inner command's args for run/exec, not compose
// flags). --build is a boolean flag on up/run/create; when set, prebuild must
// not short-circuit on "image exists locally".
func hasBuildFlag(args []string) bool {
	seenSubcmd := false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			return false
		}
		if !seenSubcmd {
			if composeGlobalFlagsWithValue[tok] {
				i++ // skip value token
				continue
			}
			if strings.HasPrefix(tok, "-") {
				continue
			}
			seenSubcmd = true
			continue
		}
		if tok == "--build" {
			return true
		}
	}
	return false
}
