package main

import (
	"fmt"
	"strings"
)

// Invocation is the canonical parsed representation of one vaka CLI invocation.
// It preserves argv token ordering while separating vaka-only flags from compose
// argv and precomputing compose-aware metadata used across execution paths.
type Invocation struct {
	RawArgs     []string
	VakaFlags   map[string]string
	ComposeArgs []string

	Subcommand     string
	SubcommandIdx  int
	PreSubcommand  []string
	PostSubcommand []string

	ComposeGlobals []string
	DockerGlobals  []string
	GlobalFiles    []string

	ProjectDirectory string
	BuildRequested   bool

	lastFileTokenIdx int // index in ComposeArgs for the last pre-subcommand -f/--file value token
}

// composeGlobalFlagsWithValue is the set of docker compose global flags that
// consume a value token. This list intentionally excludes Docker top-level
// globals (for example --context/-c), which are rejected by vaka parser rules.
var composeGlobalFlagsWithValue = map[string]bool{
	"-f": true, "--file": true,
	"-p": true, "--project-name": true,
	"--profile":           true,
	"--env-file":          true,
	"--project-directory": true,
	"--parallel":          true,
	"--ansi":              true,
	"--progress":          true,
}

var dockerGlobalFlagsWithValue = map[string]bool{
	"--context":   true,
	"--host":      true,
	"-H":          true,
	"--config":    true,
	"-c":          true,
	"--tlscacert": true,
	"--tlscert":   true,
	"--tlskey":    true,
	"--log-level": true,
	"-l":          true,
}

var dockerGlobalBoolFlags = map[string]bool{
	"--debug":     true,
	"-D":          true,
	"--tls":       true,
	"--tlsverify": true,
}

// vakaFlagsTakingValue lists --vaka-* flags that consume the next token as their value.
var vakaFlagsTakingValue = map[string]bool{
	"--vaka-file": true,
}

// vakaFlagsBool lists --vaka-* boolean flags (no value token consumed).
var vakaFlagsBool = map[string]bool{
	"--vaka-init-present": true,
}

// ParseInvocation parses raw os.Args[1:] into a single invocation model used by
// all execution paths.
func ParseInvocation(argv []string) (*Invocation, error) {
	flags, composeArgs := extractVakaFlags(argv)

	inv := &Invocation{
		RawArgs:          append([]string{}, argv...),
		VakaFlags:        flags,
		ComposeArgs:      composeArgs,
		SubcommandIdx:    -1,
		lastFileTokenIdx: -1,
	}
	if err := inv.scanComposeArgs(); err != nil {
		return nil, err
	}
	inv.detectBuildRequested()
	return inv, nil
}

// extractVakaFlags splits raw os.Args[1:] into vaka-specific flags and
// compose-destined args.
//
// For now, compatibility with current behavior is preserved:
//   - vaka flags are accepted before the compose subcommand
//   - and between subcommand and first positional token
//   - and are ignored after `--` or after that first positional
func extractVakaFlags(argv []string) (map[string]string, []string) {
	flags := make(map[string]string)
	rest := make([]string, 0, len(argv))

	bareWords := 0
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		if bareWords >= 2 {
			rest = append(rest, argv[i:]...)
			break
		}
		if !strings.HasPrefix(tok, "-") {
			rest = append(rest, tok)
			bareWords++
			continue
		}
		if vakaFlagsTakingValue[tok] {
			if i+1 < len(argv) {
				flags[tok] = argv[i+1]
				i++
			}
			continue
		}
		if vakaFlagsBool[tok] {
			flags[tok] = "true"
			continue
		}
		if _, _, consumed, _, ok := parseValueTakingToken(argv, i, composeGlobalFlagsWithValue); ok {
			rest = append(rest, argv[i:i+consumed]...)
			i += consumed - 1
			continue
		}
		if _, _, consumed, _, ok := parseValueTakingToken(argv, i, dockerGlobalFlagsWithValue); ok {
			rest = append(rest, argv[i:i+consumed]...)
			i += consumed - 1
			continue
		}
		rest = append(rest, tok)
	}
	return flags, rest
}

func (inv *Invocation) scanComposeArgs() error {
	args := inv.ComposeArgs
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			inv.PreSubcommand = append(inv.PreSubcommand, args[:i]...)
			return nil
		}

		if matchedFlag, value, consumed, usedEquals, ok := parseValueTakingToken(args, i, dockerGlobalFlagsWithValue); ok {
			inv.DockerGlobals = append(inv.DockerGlobals, args[i:i+consumed]...)
			return unsupportedDockerGlobalError(matchedFlag, value, usedEquals)
		}
		if dockerGlobalBoolFlags[tok] {
			inv.DockerGlobals = append(inv.DockerGlobals, tok)
			return unsupportedDockerGlobalError(tok, "", false)
		}

		if matchedFlag, value, consumed, usedEquals, ok := parseValueTakingToken(args, i, composeGlobalFlagsWithValue); ok {
			inv.ComposeGlobals = append(inv.ComposeGlobals, args[i:i+consumed]...)
			if matchedFlag == "-f" || matchedFlag == "--file" {
				if value != "" {
					inv.GlobalFiles = append(inv.GlobalFiles, value)
					if usedEquals {
						inv.lastFileTokenIdx = i
					} else if consumed == 2 {
						inv.lastFileTokenIdx = i + 1
					}
				}
			}
			if matchedFlag == "--project-directory" {
				inv.ProjectDirectory = strings.TrimSpace(value)
			}
			i += consumed - 1
			continue
		}

		if strings.HasPrefix(tok, "--") && strings.Contains(tok, "=") {
			inv.ComposeGlobals = append(inv.ComposeGlobals, tok)
			continue
		}
		if strings.HasPrefix(tok, "-") {
			inv.ComposeGlobals = append(inv.ComposeGlobals, tok)
			continue
		}

		inv.SubcommandIdx = i
		inv.Subcommand = tok
		inv.PreSubcommand = append(inv.PreSubcommand, args[:i]...)
		if i+1 < len(args) {
			inv.PostSubcommand = append(inv.PostSubcommand, args[i+1:]...)
		}
		return nil
	}

	inv.PreSubcommand = append(inv.PreSubcommand, args...)
	return nil
}

func (inv *Invocation) detectBuildRequested() {
	if inv.Subcommand == "" {
		return
	}
	for _, tok := range inv.PostSubcommand {
		if tok == "--" {
			return
		}
		if tok == "--build" {
			inv.BuildRequested = true
			return
		}
	}
}

func (inv *Invocation) dockerComposeArgs() []string {
	out := make([]string, 0, len(inv.ComposeArgs)+1)
	out = append(out, "compose")
	out = append(out, inv.ComposeArgs...)
	return out
}

func parseValueTakingToken(args []string, idx int, flags map[string]bool) (flag string, value string, consumed int, usedEquals bool, ok bool) {
	if idx < 0 || idx >= len(args) {
		return "", "", 0, false, false
	}
	tok := args[idx]
	if flags[tok] {
		if idx+1 < len(args) {
			return tok, args[idx+1], 2, false, true
		}
		return tok, "", 1, false, true
	}
	for candidate := range flags {
		prefix := candidate + "="
		if strings.HasPrefix(tok, prefix) {
			return candidate, strings.TrimPrefix(tok, prefix), 1, true, true
		}
	}
	return "", "", 0, false, false
}

func unsupportedDockerGlobalError(flag, value string, usedEquals bool) error {
	switch flag {
	case "--context", "-c":
		if value != "" {
			return fmt.Errorf("docker top-level %s is not supported in vaka arguments (got %q); use `docker context use %s` or `DOCKER_CONTEXT=%s vaka ...`", flag, value, value, value)
		}
		return fmt.Errorf("docker top-level %s is not supported in vaka arguments; use `docker context use <name>` or `DOCKER_CONTEXT=<name> vaka ...`", flag)
	case "--host", "-H":
		if value != "" {
			return fmt.Errorf("docker top-level %s is not supported in vaka arguments (got %q); use `DOCKER_HOST=%s vaka ...`", flag, value, value)
		}
		return fmt.Errorf("docker top-level %s is not supported in vaka arguments; use `DOCKER_HOST=<daemon-url> vaka ...`", flag)
	default:
		if usedEquals && value != "" {
			return fmt.Errorf("docker top-level %s=%q is not supported in vaka arguments; configure Docker target via environment or docker config", flag, value)
		}
		if value != "" {
			return fmt.Errorf("docker top-level %s is not supported in vaka arguments (got %q); configure Docker target via environment or docker config", flag, value)
		}
		return fmt.Errorf("docker top-level %s is not supported in vaka arguments; configure Docker target via environment or docker config", flag)
	}
}
