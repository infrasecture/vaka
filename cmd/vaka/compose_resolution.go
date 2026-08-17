package main

import (
	"context"
	"fmt"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"
)

// composeResolution is the resolved compose input set for one vaka invocation.
type composeResolution struct {
	Files       []string
	WorkingDir  string
	ProjectName string
	Profiles    []string
	EnvFiles    []string
}

func resolveComposeProjectName(ctx context.Context, inv *ComposeInvocation) (string, error) {
	input, err := resolveComposeInput(inv)
	if err != nil {
		return "", err
	}
	opts, err := newComposeProjectOptions(input, false)
	if err != nil {
		return "", fmt.Errorf("compose project options: %w", err)
	}
	project, err := opts.LoadProject(ctx)
	if err != nil {
		return "", fmt.Errorf("load compose project: %w", err)
	}
	return project.Name, nil
}

// resolveComposeInput resolves the compose files vaka must use for policy
// validation and override generation.
//
// Resolution order matches Compose expectations:
//  1. explicit -f/--file flags
//  2. COMPOSE_FILE (from env / .env via compose-go)
//  3. default compose file discovery with parent traversal
func resolveComposeInput(inv *ComposeInvocation) (*composeResolution, error) {
	explicitFiles := inv.GlobalFiles
	workingDir := inv.ProjectDirectory
	if len(explicitFiles) > 0 {
		return &composeResolution{
			Files:       append([]string{}, explicitFiles...),
			WorkingDir:  workingDir,
			ProjectName: inv.ProjectName,
			Profiles:    append([]string{}, inv.Profiles...),
			EnvFiles:    append([]string{}, inv.EnvFiles...),
		}, nil
	}

	input := &composeResolution{
		WorkingDir:  workingDir,
		ProjectName: inv.ProjectName,
		Profiles:    append([]string{}, inv.Profiles...),
		EnvFiles:    append([]string{}, inv.EnvFiles...),
	}
	opts, err := newComposeProjectOptions(input, true)
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
		Files:       append([]string{}, opts.ConfigPaths...),
		WorkingDir:  workingDir,
		ProjectName: inv.ProjectName,
		Profiles:    append([]string{}, inv.Profiles...),
		EnvFiles:    append([]string{}, inv.EnvFiles...),
	}, nil
}

// newComposeProjectOptions builds compose-go project options for validation and
// project loading. When autoDiscover is true and input.Files is empty, Compose
// defaults are enabled (COMPOSE_FILE + default file search).
func newComposeProjectOptions(input *composeResolution, autoDiscover bool) (*composecli.ProjectOptions, error) {
	if input == nil {
		input = &composeResolution{}
	}
	opts := []composecli.ProjectOptionsFn{}
	if strings.TrimSpace(input.WorkingDir) != "" {
		opts = append(opts, composecli.WithWorkingDirectory(input.WorkingDir))
	}
	if strings.TrimSpace(input.ProjectName) != "" {
		opts = append(opts, composecli.WithName(input.ProjectName))
	}
	opts = append(opts, composecli.WithOsEnv)
	if len(input.EnvFiles) > 0 {
		opts = append(opts, composecli.WithEnvFiles(input.EnvFiles...))
	} else {
		opts = append(opts, composecli.WithEnvFiles())
	}
	opts = append(opts, composecli.WithDotEnv)
	// Match Compose's profile precedence: explicit --profile values win; when
	// absent, COMPOSE_PROFILES is read after OS, env-file, and .env loading.
	opts = append(opts, composecli.WithDefaultProfiles(input.Profiles...))
	if autoDiscover && len(input.Files) == 0 {
		opts = append(opts,
			composecli.WithConfigFileEnv,
			composecli.WithDefaultConfigPath,
		)
	}
	return composecli.NewProjectOptions(input.Files, opts...)
}
