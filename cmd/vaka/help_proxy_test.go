package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDiscoverComposeCommandHelpLines(t *testing.T) {
	old := dockerComposeHelpOutput
	dockerComposeHelpOutput = func() ([]byte, error) {
		return []byte(`Usage:  docker compose [OPTIONS] COMMAND

Commands:
  build       Build or rebuild services
  up          Create and start containers
  ps          List containers

Options:
  --ansi string   Control ANSI output`), nil
	}
	t.Cleanup(func() {
		dockerComposeHelpOutput = old
	})

	lines, err := discoverComposeCommandHelpLines()
	if err != nil {
		t.Fatalf("discoverComposeCommandHelpLines: %v", err)
	}
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "build") || !strings.Contains(got, "up") || !strings.Contains(got, "ps") {
		t.Fatalf("parsed command lines missing expected entries: %v", lines)
	}
}

func TestProxiedComposeCommandsHelpSectionFallback(t *testing.T) {
	old := dockerComposeHelpOutput
	dockerComposeHelpOutput = func() ([]byte, error) {
		return nil, errors.New("docker unavailable")
	}
	t.Cleanup(func() {
		dockerComposeHelpOutput = old
	})

	section := proxiedComposeCommandsHelpSection()
	if !strings.Contains(section, "unavailable") {
		t.Fatalf("expected fallback section, got %q", section)
	}
}

func TestConfigureRootHelpAppendsProxySection(t *testing.T) {
	old := dockerComposeHelpOutput
	dockerComposeHelpOutput = func() ([]byte, error) {
		return []byte(`Commands:
  up     Create and start containers
`), nil
	}
	t.Cleanup(func() {
		dockerComposeHelpOutput = old
	})

	root := &cobra.Command{
		Use:   "vaka",
		Short: "test",
	}
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run:   func(*cobra.Command, []string) {},
	})
	root.AddCommand(newShowComposeCmd())
	configureRootHelp(root)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Proxied docker compose commands") {
		t.Fatalf("help output missing proxied section:\n%s", got)
	}
	if !strings.Contains(got, "show-compose") {
		t.Fatalf("help output missing native show-compose command:\n%s", got)
	}
	if strings.Contains(got, "Additional vaka command") {
		t.Fatalf("help output should not use show-compose footer:\n%s", got)
	}
}

func TestShowComposeRegisteredForCobraHelp(t *testing.T) {
	root := &cobra.Command{
		Use:   "vaka",
		Short: "test",
	}
	root.AddCommand(newShowComposeCmd())

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"help", "show-compose"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Print the generated compose override YAML used by vaka injection.") {
		t.Fatalf("show-compose help missing description:\n%s", got)
	}
	if !strings.Contains(got, "vaka [compose-global-flags...] show-compose") {
		t.Fatalf("show-compose help missing custom usage:\n%s", got)
	}
}
