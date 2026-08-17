package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// RootInvocation is the parsed root layer of one vaka CLI invocation: the
// vaka-owned flags plus the remaining argv destined for command dispatch.
type RootInvocation struct {
	VakaFile        string
	VakaInitPresent bool
	PullPolicy      PullPolicy
	Rest            []string
}

// ComposeInvocation is the canonical parsed representation of one compose-bound
// argv slice (vaka root flags already stripped). It preserves token ordering
// and precomputes compose-aware metadata used across execution paths.
type ComposeInvocation struct {
	Args []string

	Subcommand     string
	SubcommandIdx  int
	PreSubcommand  []string
	PostSubcommand []string

	ComposeGlobals []string
	DockerGlobals  []string
	GlobalFiles    []string

	ProjectDirectory    string
	ProjectName         string
	Profiles            []string
	EnvFiles            []string
	BuildRequested      bool
	ResolvedProjectName string
	GlobalHelp          bool
	GlobalVersion       bool
	Compatibility       bool
	AllResources        bool

	lastFileTokenIdx int // index in Args for the last pre-subcommand -f/--file value token
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

var composeGlobalBoolFlags = map[string]bool{
	"--all-resources": true,
	"--compatibility": true,
	"--dry-run":       true,
	"-h":              true,
	"--help":          true,
	"-v":              true,
	"--version":       true,
	// These remain accepted by Compose for compatibility, even though they are
	// hidden from normal help output.
	"--no-ansi": true,
	"--verbose": true,
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
	"--vaka-pull": true,
}

// vakaFlagsBool lists --vaka-* boolean flags (no value token consumed).
var vakaFlagsBool = map[string]bool{
	"--vaka-init-present": true,
}

// parseRootArgs parses raw os.Args[1:] into the vaka root layer: strict
// --vaka-* flags plus the remaining argv for command dispatch.
func parseRootArgs(argv []string) (*RootInvocation, error) {
	flags, rest, err := extractVakaFlags(argv)
	if err != nil {
		return nil, err
	}
	pullPolicy, err := ParsePullPolicy(flags["--vaka-pull"])
	if err != nil {
		return nil, err
	}
	root := &RootInvocation{
		VakaFile:        flags["--vaka-file"],
		VakaInitPresent: flags["--vaka-init-present"] == "true",
		PullPolicy:      pullPolicy,
		Rest:            rest,
	}
	if root.VakaFile == "" {
		root.VakaFile = "vaka.yaml"
	}
	return root, nil
}

// ParseComposeInvocation parses a compose-bound argv slice (vaka root flags
// already stripped) into the invocation model used by all compose execution
// paths.
func ParseComposeInvocation(argv []string) (*ComposeInvocation, error) {
	inv := &ComposeInvocation{
		Args:             append([]string{}, argv...),
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
// command-destined args.
//
// Strict mode:
//   - `--vaka-*` flags are accepted only before the command.
//   - value-taking `--vaka-*` flags require `=` form: `--flag=<value>`.
//   - unknown pre-command `--vaka-*` flags are hard errors with suggestion.
//   - any other pre-command flag is a hard error (`-h`/`--help` excepted):
//     compose global flags belong after the `compose` command.
//   - post-command tokens are forwarded unchanged, except known misplaced
//     vaka flags which fail fast with a positioning hint.
func extractVakaFlags(argv []string) (map[string]string, []string, error) {
	flags := make(map[string]string)
	rest := make([]string, 0, len(argv))

	command := ""
	seenCommand := false
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if tok == "--" {
			rest = append(rest, argv[i:]...)
			break
		}

		if !seenCommand {
			if flag, value, err := parseVakaValueFlag(tok); err != nil {
				return nil, nil, err
			} else if flag != "" {
				flags[flag] = value
				continue
			}
			if vakaFlagsBool[tok] {
				flags[tok] = "true"
				continue
			}
			if strings.HasPrefix(tok, "--vaka-") {
				return nil, nil, unknownVakaFlagError(tok)
			}
			if tok == "-h" || tok == "--help" {
				rest = append(rest, tok)
				continue
			}
			if strings.HasPrefix(tok, "-") {
				return nil, nil, rootLeadingFlagError(argv[i:])
			}

			seenCommand = true
			command = tok
			rest = append(rest, tok)
			continue
		}

		if isKnownVakaFlagToken(tok) {
			return nil, nil, fmt.Errorf("vaka flag %q must appear before subcommand %q", tok, command)
		}
		rest = append(rest, tok)
	}
	return flags, rest, nil
}

// rootLeadingFlagError explains a non-vaka flag encountered before the command.
// Docker top-level globals keep their targeted guidance; compose globals point
// at the `vaka compose` form; anything else gets the generic placement rule.
func rootLeadingFlagError(tail []string) error {
	tok := tail[0]
	if matched, value, _, usedEquals, ok := parseValueTakingToken(tail, 0, dockerGlobalFlagsWithValue); ok {
		return unsupportedDockerGlobalError(matched, value, usedEquals)
	}
	if dockerGlobalBoolFlags[tok] {
		return unsupportedDockerGlobalError(tok, "", false)
	}
	if matched, _, _, _, ok := parseValueTakingToken(tail, 0, composeGlobalFlagsWithValue); ok {
		return fmt.Errorf("compose global flag %q must follow the compose command: try `vaka compose %s`", matched, strings.Join(tail, " "))
	}
	return fmt.Errorf("unknown flag %q before command; only --vaka-* flags may precede the command, and compose global flags belong after it: `vaka compose %s`", tok, strings.Join(tail, " "))
}

func (inv *ComposeInvocation) scanComposeArgs() error {
	args := inv.Args
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
			if value == "" {
				return fmt.Errorf("compose global option %s requires a non-empty value", matchedFlag)
			}
			if err := validateComposeGlobalValue(matchedFlag, value); err != nil {
				return err
			}
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
			if matchedFlag == "-p" || matchedFlag == "--project-name" {
				inv.ProjectName = strings.TrimSpace(value)
			}
			if matchedFlag == "--profile" {
				inv.Profiles = append(inv.Profiles, value)
			}
			if matchedFlag == "--env-file" {
				inv.EnvFiles = append(inv.EnvFiles, value)
			}
			i += consumed - 1
			continue
		}

		if name, enabled, ok, err := parseComposeGlobalBoolToken(tok); ok {
			if err != nil {
				return err
			}
			inv.ComposeGlobals = append(inv.ComposeGlobals, tok)
			switch name {
			case "-h", "--help":
				inv.GlobalHelp = enabled
			case "-v", "--version":
				inv.GlobalVersion = enabled
			case "--compatibility":
				inv.Compatibility = enabled
			case "--all-resources":
				inv.AllResources = enabled
			}
			continue
		}
		if strings.HasPrefix(tok, "-") {
			return fmt.Errorf("unknown compose global option %q before command; use raw `docker compose` for an option Vaka has not reviewed", tok)
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

func validateComposeGlobalValue(flag, value string) error {
	switch flag {
	case "--ansi":
		if value != "auto" && value != "always" && value != "never" {
			return fmt.Errorf("compose global option --ansi has invalid value %q", value)
		}
	case "--progress":
		switch value {
		case "auto", "tty", "plain", "json", "quiet", "none":
		default:
			return fmt.Errorf("compose global option --progress has invalid value %q", value)
		}
	case "--parallel":
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("compose global option --parallel requires an integer, got %q", value)
		}
	}
	return nil
}

func parseComposeGlobalBoolToken(tok string) (name string, enabled bool, ok bool, err error) {
	if composeGlobalBoolFlags[tok] {
		return tok, true, true, nil
	}
	if len(tok) > 3 && tok[0] == '-' && tok[1] != '-' && tok[2] == '=' && composeGlobalBoolFlags[tok[:2]] {
		enabled, err = composeBoolValue(tok[:2], tok[3:])
		return tok[:2], enabled, true, err
	}
	if !strings.HasPrefix(tok, "--") {
		return "", false, false, nil
	}
	name, value, hasValue := strings.Cut(tok, "=")
	if !hasValue || !composeGlobalBoolFlags[name] {
		return "", false, false, nil
	}
	enabled, err = composeBoolValue(name, value)
	if err != nil {
		return name, false, true, err
	}
	return name, enabled, true, nil
}

func (inv *ComposeInvocation) detectBuildRequested() {
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

func (inv *ComposeInvocation) dockerComposeArgs() []string {
	out := make([]string, 0, len(inv.Args)+1)
	out = append(out, "compose")
	out = append(out, inv.Args...)
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
	for candidate := range flags {
		if len(candidate) != 2 || candidate[0] != '-' || candidate[1] == '-' || !strings.HasPrefix(tok, candidate) || len(tok) <= len(candidate) {
			continue
		}
		value := strings.TrimPrefix(tok, candidate)
		usedEquals := strings.HasPrefix(value, "=")
		value = strings.TrimPrefix(value, "=")
		return candidate, value, 1, usedEquals, true
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
	if flagNameFromEqualsForm(tok, vakaFlagsTakingValue) != "" {
		return true
	}
	if vakaFlagsTakingValue[tok] {
		return true
	}
	return vakaFlagsBool[tok]
}

func parseVakaValueFlag(tok string) (flag string, value string, err error) {
	if vakaFlagsTakingValue[tok] {
		return "", "", fmt.Errorf("%s requires '=' form before the subcommand (use %s=<value>)", tok, tok)
	}
	flag = flagNameFromEqualsForm(tok, vakaFlagsTakingValue)
	if flag == "" {
		return "", "", nil
	}
	value = strings.TrimSpace(strings.TrimPrefix(tok, flag+"="))
	if value == "" {
		return "", "", fmt.Errorf("%s requires a non-empty value (use %s=<value>)", flag, flag)
	}
	return flag, value, nil
}

func flagNameFromEqualsForm(tok string, flags map[string]bool) string {
	for _, candidate := range sortedFlagKeys(flags) {
		if strings.HasPrefix(tok, candidate+"=") {
			return candidate
		}
	}
	return ""
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
	for _, candidate := range allKnownVakaFlags() {
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

func allKnownVakaFlags() []string {
	out := make([]string, 0, len(vakaFlagsTakingValue)+len(vakaFlagsBool))
	for _, flag := range sortedFlagKeys(vakaFlagsTakingValue) {
		out = append(out, flag)
	}
	for _, flag := range sortedFlagKeys(vakaFlagsBool) {
		out = append(out, flag)
	}
	return out
}

func sortedFlagKeys(flags map[string]bool) []string {
	out := make([]string, 0, len(flags))
	for flag := range flags {
		out = append(out, flag)
	}
	sort.Strings(out)
	return out
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
