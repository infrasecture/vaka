// cmd/vaka/show_compose.go
package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"
)

// newShowComposeCmd prints the generated compose override YAML that vaka would
// inject for `vaka compose up`. Compose inputs are command-local flags here
// (not compose globals): `vaka show-compose -f compose.yml --build`.
func newShowComposeCmd(root *RootInvocation) *cobra.Command {
	var (
		files            []string
		projectDirectory string
		projectName      string
		profiles         []string
		envFiles         []string
		build            bool
		output           string
	)
	cmd := &cobra.Command{
		Use:   "show-compose [-f compose.yml ...] [--build] [-o override.yaml]",
		Short: "Print the generated compose override YAML used by vaka injection",
		Long: `Print the generated compose override YAML used by vaka injection.

Pass --vaka-file before the command:
  vaka --vaka-file=prod.yaml show-compose

Internal per-service policy payload values are never printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			argv := make([]string, 0, 2*(len(files)+len(profiles)+len(envFiles))+8)
			for _, f := range files {
				argv = append(argv, "-f", f)
			}
			if projectDirectory != "" {
				argv = append(argv, "--project-directory", projectDirectory)
			}
			if projectName != "" {
				argv = append(argv, "--project-name", projectName)
			}
			for _, p := range profiles {
				argv = append(argv, "--profile", p)
			}
			for _, e := range envFiles {
				argv = append(argv, "--env-file", e)
			}
			argv = append(argv, "show-compose")
			if build {
				argv = append(argv, "--build")
			}
			inv, err := ParseComposeInvocation(argv)
			if err != nil {
				return err
			}
			return runShowCompose(root.VakaFile, inv, root.VakaInitPresent, root.PullPolicy, output)
		},
	}
	cmd.Flags().StringArrayVarP(&files, "file", "f", nil, "Compose configuration files")
	cmd.Flags().StringVar(&projectDirectory, "project-directory", "", "Alternate working directory for compose file discovery")
	cmd.Flags().StringVarP(&projectName, "project-name", "p", "", "Project name")
	cmd.Flags().StringArrayVar(&profiles, "profile", nil, "Compose profiles to enable")
	cmd.Flags().StringArrayVar(&envFiles, "env-file", nil, "Alternate environment files")
	cmd.Flags().BoolVar(&build, "build", false, "Pre-build eligible services before resolving image runtime metadata")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Write override YAML to a file instead of stdout")
	return cmd
}

// runShowCompose builds the same compose override as runFull and prints it to
// stdout, or writes it to output when non-empty. inv is the synthetic compose
// invocation assembled from show-compose flags so the shared builder receives
// the same input shape as `vaka compose up`.
func runShowCompose(vakaFile string, inv *ComposeInvocation, vakaInitPresent bool, pullPolicy PullPolicy, output string) error {
	ctx := context.Background()
	ds, err := newDockerServices(inv, pullPolicy)
	if err != nil {
		return err
	}
	if err := ds.CheckRuntimeCompatibility(ctx); err != nil {
		return err
	}

	overrideYAML, _, err := buildInjectionOverride(ctx, ds, vakaFile, inv, vakaInitPresent)
	if err != nil {
		return err
	}

	if output == "" {
		_, err := os.Stdout.WriteString(overrideYAML)
		return err
	}
	return os.WriteFile(output, []byte(overrideYAML), 0o644)
}
