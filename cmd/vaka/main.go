// cmd/vaka/main.go
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

var version = "dev"

func main() {
	root, err := parseRootArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "vaka:", err)
		os.Exit(1)
	}

	cmd := newRootCmd(root)
	cmd.SetArgs(root.Rest)
	if err := cmd.Execute(); err != nil {
		os.Exit(exitCode(err))
	}
}

// exitCode extracts the process exit code from an *exec.ExitError so that
// vaka propagates docker's exit code faithfully.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
