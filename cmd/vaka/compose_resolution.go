package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	composecli "github.com/compose-spec/compose-go/v2/cli"
)

// composeResolution is the resolved compose input set for one vaka invocation.
type composeResolution struct {
	Files               []string
	WorkingDir          string
	ProjectName         string
	Profiles            []string
	EnvFiles            []string
	Environment         map[string]string
	EnvironmentResolved bool
	ImplicitFiles       bool
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
		Files:               append([]string{}, opts.ConfigPaths...),
		WorkingDir:          workingDir,
		ProjectName:         inv.ProjectName,
		Profiles:            append([]string{}, inv.Profiles...),
		EnvFiles:            append([]string{}, inv.EnvFiles...),
		Environment:         cloneComposeEnvironment(opts.Environment),
		EnvironmentResolved: true,
		ImplicitFiles:       true,
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
	if input.EnvironmentResolved {
		opts = append(opts, composecli.WithEnv(composeEnvironmentEntries(input.Environment)))
		opts = append(opts,
			composecli.WithDefaultProfiles(input.Profiles...),
			composecli.WithName(input.ProjectName),
		)
		return composecli.NewProjectOptions(input.Files, opts...)
	}

	// Match Compose's two-stage dotenv loading. The launcher/PWD dotenv is
	// loaded first so it can select COMPOSE_FILE. Once files are known, the
	// project-directory dotenv is loaded without replacing values already
	// supplied by the OS or launcher. Profile and project-name resolution happen
	// only after both stages.
	opts = append(opts, composecli.WithOsEnv)
	if _, present := os.LookupEnv("PWD"); !present {
		pwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		opts = append(opts, composecli.WithEnv([]string{"PWD=" + pwd}))
	}
	if len(input.EnvFiles) > 0 {
		opts = append(opts, composecli.WithEnvFiles(input.EnvFiles...))
	} else {
		opts = append(opts, composecli.WithEnvFiles())
	}
	opts = append(opts, composecli.WithDotEnv)
	if autoDiscover && len(input.Files) == 0 {
		opts = append(opts,
			composecli.WithConfigFileEnv,
			composecli.WithDefaultConfigPath,
		)
	}
	if len(input.EnvFiles) > 0 {
		opts = append(opts, composecli.WithEnvFiles(input.EnvFiles...))
	} else {
		opts = append(opts, composecli.WithEnvFiles())
	}
	opts = append(opts,
		composecli.WithDotEnv,
		composecli.WithDefaultProfiles(input.Profiles...),
		composecli.WithName(input.ProjectName),
	)
	return composecli.NewProjectOptions(input.Files, opts...)
}

func cloneComposeEnvironment(environment map[string]string) map[string]string {
	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}
	return cloned
}

func composeEnvironmentEntries(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]string, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, key+"="+environment[key])
	}
	return entries
}

// resolvedComposeDotEnv serializes only values which did not originate in the
// real process environment. Passing these through Compose's --env-file keeps
// them in Compose's project environment instead of promoting arbitrary dotenv
// keys (for example DOCKER_HOST) into the child process environment.
func resolvedComposeDotEnv(environment map[string]string) string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		if _, present := os.LookupEnv(key); !present {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		out.WriteString(key)
		out.WriteString("='")
		out.WriteString(strings.ReplaceAll(environment[key], "'", `\'`))
		out.WriteString("'\n")
	}
	return out.String()
}
