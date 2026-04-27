package main

import (
	"fmt"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"
)

// composeResolution is the resolved compose input set for one vaka invocation.
type composeResolution struct {
	Files      []string
	WorkingDir string
}

// resolveComposeInput resolves the compose files vaka must use for policy
// validation and override generation.
//
// Resolution order matches Compose expectations:
//  1. explicit -f/--file flags
//  2. COMPOSE_FILE (from env / .env via compose-go)
//  3. default compose file discovery with parent traversal
func resolveComposeInput(inv *Invocation) (*composeResolution, error) {
	explicitFiles := inv.GlobalFiles
	workingDir := inv.ProjectDirectory
	if len(explicitFiles) > 0 {
		return &composeResolution{
			Files:      append([]string{}, explicitFiles...),
			WorkingDir: workingDir,
		}, nil
	}

	opts, err := newComposeProjectOptions(nil, workingDir, true)
	if err != nil {
		return nil, fmt.Errorf("compose project options: %w", err)
	}
	if len(opts.ConfigPaths) == 0 {
		suffix := ""
		if wd := strings.TrimSpace(workingDir); wd != "" {
			suffix = fmt.Sprintf(" from %q and parent directories", wd)
		}
		return nil, fmt.Errorf("no compose configuration file found (checked COMPOSE_FILE and default compose.yaml/docker-compose.yaml names%s)", suffix)
	}

	return &composeResolution{
		Files:      append([]string{}, opts.ConfigPaths...),
		WorkingDir: workingDir,
	}, nil
}

// newComposeProjectOptions builds compose-go project options for validation and
// project loading. When autoDiscover is true and composeFiles is empty, compose
// defaults are enabled (COMPOSE_FILE + default file search).
func newComposeProjectOptions(composeFiles []string, workingDir string, autoDiscover bool) (*composecli.ProjectOptions, error) {
	opts := []composecli.ProjectOptionsFn{}
	if strings.TrimSpace(workingDir) != "" {
		opts = append(opts, composecli.WithWorkingDirectory(workingDir))
	}
	opts = append(opts,
		composecli.WithOsEnv,
		composecli.WithEnvFiles(),
		composecli.WithDotEnv,
	)
	if autoDiscover && len(composeFiles) == 0 {
		opts = append(opts,
			composecli.WithConfigFileEnv,
			composecli.WithDefaultConfigPath,
		)
	}
	return composecli.NewProjectOptions(composeFiles, opts...)
}
