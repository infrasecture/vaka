package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestDiscoverComposeVerbs(t *testing.T) {
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

	verbs, err := discoverComposeVerbs()
	if err != nil {
		t.Fatalf("discoverComposeVerbs: %v", err)
	}
	for _, want := range []string{"build", "up", "ps"} {
		if !verbs[want] {
			t.Fatalf("parsed verbs missing %q: %v", want, verbs)
		}
	}
	if verbs["--ansi"] || verbs["string"] {
		t.Fatalf("parsed verbs must not include option tokens: %v", verbs)
	}
}

func TestDiscoverComposeVerbsUnavailable(t *testing.T) {
	old := dockerComposeHelpOutput
	dockerComposeHelpOutput = func() ([]byte, error) {
		return nil, errors.New("docker unavailable")
	}
	t.Cleanup(func() {
		dockerComposeHelpOutput = old
	})

	if _, err := discoverComposeVerbs(); err == nil {
		t.Fatal("expected error when docker compose --help is unavailable")
	}
}

func TestRootHelpListsShorthandsAndCompose(t *testing.T) {
	root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml"})

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"Vaka Commands:",
		"Compose Commands:",
		"compose ",
		"show-compose",
		"validate",
		"doctor",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("root help missing %q:\n%s", want, got)
		}
	}
	for _, shorthand := range composeShorthands {
		if !strings.Contains(got, "vaka compose "+shorthand) {
			t.Fatalf("root help missing shorthand %q:\n%s", shorthand, got)
		}
	}
}

// TestRootHelpUsageShowsGrammarOnce guards against cobra's default two-line
// usage for runnable roots ("vaka [flags]" + "vaka [command]"): root help must
// show exactly the vaka grammar line.
func TestRootHelpUsageShowsGrammarOnce(t *testing.T) {
	root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml"})

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root.Execute: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "Usage:\n  vaka [--vaka-file=<path>] [--vaka-init-present] <command>\n") {
		t.Fatalf("root help missing grammar usage line:\n%s", got)
	}
	for _, stale := range []string{"vaka [flags]", "\n  vaka [command]"} {
		if strings.Contains(got, stale) {
			t.Fatalf("root help contains stale usage line %q:\n%s", stale, got)
		}
	}
}

func TestShowComposeRegisteredForCobraHelp(t *testing.T) {
	root := newRootCmd(&RootInvocation{VakaFile: "vaka.yaml"})

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
	if !strings.Contains(got, "--vaka-file") {
		t.Fatalf("show-compose help missing root-flag placement note:\n%s", got)
	}
}
