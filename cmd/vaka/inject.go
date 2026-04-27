package main

const (
	composeOverrideFD   = 3
	composeOverridePath = "/dev/fd/3"
)

// injectFDOverride takes parsed invocation data and inserts a compose file
// reference backed by an inherited file descriptor (`-f /dev/fd/3`) as the
// last pre-subcommand compose file, so the vaka override YAML wins over all
// other compose files without consuming stdin.
//
// defaults is the list of compose files to inject when the user supplied no
// explicit -f flags (output of resolveComposeInput). Pass nil only when
// inv.GlobalFiles is non-empty.
func injectFDOverride(inv *Invocation, defaults []string) []string {
	dockerArgs := inv.dockerComposeArgs()

	// We insert after the last explicit compose file token before subcommand.
	// inv.lastFileTokenIdx is indexed in inv.ComposeArgs, so add one for the
	// leading "compose" token in dockerArgs.
	if inv.lastFileTokenIdx >= 0 {
		insertAfter := inv.lastFileTokenIdx + 1
		out := make([]string, 0, len(dockerArgs)+2)
		out = append(out, dockerArgs[:insertAfter+1]...)
		out = append(out, "-f", composeOverridePath)
		out = append(out, dockerArgs[insertAfter+1:]...)
		return out
	}

	// No explicit -f: insert discovered defaults then "-f /dev/fd/3" right after
	// "compose", before subcommand or other flags.
	out := make([]string, 0, len(dockerArgs)+len(defaults)*2+2)
	out = append(out, dockerArgs[0]) // "compose"
	for _, f := range defaults {
		out = append(out, "-f", f)
	}
	out = append(out, "-f", composeOverridePath)
	out = append(out, dockerArgs[1:]...)
	return out
}
