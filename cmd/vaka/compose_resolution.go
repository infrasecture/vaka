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
func resolveComposeInput(args []string) (*composeResolution, error) {
	explicitFiles := allFileFlags(args)
	workingDir := projectDirectoryFromArgs(args)
	if len(explicitFiles) > 0 {
		return &composeResolution{
			Files:      explicitFiles,
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

// projectDirectoryFromArgs returns --project-directory value from compose
// global flags (the last occurrence wins). Returns empty when unset.
func projectDirectoryFromArgs(args []string) string {
	return composeGlobalValue(args, "--project-directory", "")
}

func composeGlobalValue(args []string, longFlag, shortFlag string) string {
	var value string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break
		}

		if tok == longFlag || (shortFlag != "" && tok == shortFlag) {
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
			continue
		}
		if strings.HasPrefix(tok, longFlag+"=") {
			value = strings.TrimPrefix(tok, longFlag+"=")
			continue
		}
		if shortFlag != "" && strings.HasPrefix(tok, shortFlag+"=") {
			value = strings.TrimPrefix(tok, shortFlag+"=")
			continue
		}

		if composeGlobalFlagsWithValue[tok] {
			i++
			continue
		}
		if strings.HasPrefix(tok, "--") && strings.Contains(tok, "=") {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			continue
		}
		break
	}
	return strings.TrimSpace(value)
}
