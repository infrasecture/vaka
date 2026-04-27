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

var knownVakaFlags = []string{
	"--vaka-file",
	"--vaka-init-present",
}

// ParseInvocation parses raw os.Args[1:] into a single invocation model used by
// all execution paths.
func ParseInvocation(argv []string) (*Invocation, error) {
	flags, composeArgs, err := extractVakaFlags(argv)
	if err != nil {
		return nil, err
	}

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
// Strict mode:
//   - `--vaka-*` flags are accepted only before the compose subcommand.
//   - `--vaka-file` requires `=` form: `--vaka-file=<path>`.
//   - unknown pre-subcommand `--vaka-*` flags are hard errors with suggestion.
//   - post-subcommand tokens are forwarded unchanged, except known misplaced
//     vaka flags which fail fast with a positioning hint.
func extractVakaFlags(argv []string) (map[string]string, []string, error) {
	flags := make(map[string]string)
	rest := make([]string, 0, len(argv))

	subcommand := ""
	seenSubcommand := false
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			rest = append(rest, argv[i:]...)
			break
		}

		if !seenSubcommand {
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

			if tok == "--vaka-file" {
				return nil, nil, fmt.Errorf("--vaka-file requires '=' form before the subcommand (use --vaka-file=<path>)")
			}
			if strings.HasPrefix(tok, "--vaka-file=") {
				val := strings.TrimSpace(strings.TrimPrefix(tok, "--vaka-file="))
				if val == "" {
					return nil, nil, fmt.Errorf("--vaka-file requires a non-empty value (use --vaka-file=<path>)")
				}
				flags["--vaka-file"] = val
				continue
			}
			if vakaFlagsBool[tok] {
				flags[tok] = "true"
				continue
			}
			if strings.HasPrefix(tok, "--vaka-") {
				return nil, nil, unknownVakaFlagError(tok)
			}

			if !strings.HasPrefix(tok, "-") {
				seenSubcommand = true
				subcommand = tok
				rest = append(rest, tok)
				continue
			}
			rest = append(rest, tok)
			continue
		}

		if isKnownVakaFlagToken(tok) {
			return nil, nil, fmt.Errorf("vaka flag %q must appear before subcommand %q", tok, subcommand)
		}
		rest = append(rest, tok)
	}
	return flags, rest, nil
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

func isKnownVakaFlagToken(tok string) bool {
	if tok == "--vaka-file" || strings.HasPrefix(tok, "--vaka-file=") {
		return true
	}
	return vakaFlagsBool[tok]
}

func unknownVakaFlagError(tok string) error {
	base := tok
	if idx := strings.Index(base, "="); idx >= 0 {
		base = base[:idx]
	}
	suggestion := nearestVakaFlag(base)
	if suggestion != "" {
		return fmt.Errorf("unknown vaka flag %q; did you mean %q?", tok, suggestion)
	}
	return fmt.Errorf("unknown vaka flag %q", tok)
}

func nearestVakaFlag(flag string) string {
	best := ""
	bestDist := -1
	for _, candidate := range knownVakaFlags {
		d := levenshteinDistance(flag, candidate)
		if bestDist == -1 || d < bestDist {
			bestDist = d
			best = candidate
		}
	}
	if bestDist <= 3 {
		return best
	}
	return ""
}

func levenshteinDistance(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			curr[j] = min3(del, ins, sub)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
