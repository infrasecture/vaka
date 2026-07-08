package main

import (
	"strings"
	"testing"
)

func TestParseRootArgsVakaFlagExtraction(t *testing.T) {
	t.Run("extracts --vaka-file=<path> and preserves remaining args", func(t *testing.T) {
		root, err := parseRootArgs([]string{"--vaka-file=prod.yaml", "-f", "a.yaml", "up", "--build"})
		if err != nil {
			t.Fatalf("parseRootArgs: %v", err)
		}
		if root.VakaFile != "prod.yaml" {
			t.Fatalf("VakaFile=%q, want prod.yaml", root.VakaFile)
		}
		assertArgv(t, []string{"-f", "a.yaml", "up", "--build"}, root.Rest)
	})

	t.Run("no vaka flags: args unchanged and defaults applied", func(t *testing.T) {
		root, err := parseRootArgs([]string{"up", "--build"})
		if err != nil {
			t.Fatalf("parseRootArgs: %v", err)
		}
		if root.VakaFile != "vaka.yaml" {
			t.Fatalf("VakaFile=%q, want default vaka.yaml", root.VakaFile)
		}
		if root.VakaInitPresent {
			t.Fatal("VakaInitPresent=true, want false")
		}
		assertArgv(t, []string{"up", "--build"}, root.Rest)
	})

	t.Run("unknown --vaka-* after subcommand is forwarded verbatim", func(t *testing.T) {
		root, err := parseRootArgs([]string{"run", "gateway", "mytool", "--vaka-anything"})
		if err != nil {
			t.Fatalf("parseRootArgs: %v", err)
		}
		assertArgv(t, []string{"run", "gateway", "mytool", "--vaka-anything"}, root.Rest)
	})

	t.Run("known vaka flag after subcommand hard-errors", func(t *testing.T) {
		_, err := parseRootArgs([]string{"up", "--vaka-file=x.yml"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "must appear before subcommand") {
			t.Fatalf("error %q does not contain positioning hint", err.Error())
		}
	})

	t.Run("vaka flag after -- is not extracted", func(t *testing.T) {
		root, err := parseRootArgs([]string{"--", "--vaka-file", "x"})
		if err != nil {
			t.Fatalf("parseRootArgs: %v", err)
		}
		if root.VakaFile != "vaka.yaml" {
			t.Fatalf("VakaFile=%q, want default vaka.yaml", root.VakaFile)
		}
		assertArgv(t, []string{"--", "--vaka-file", "x"}, root.Rest)
	})
}

func TestParseRootArgsVakaStrictRules(t *testing.T) {
	t.Run("space form for --vaka-file is rejected", func(t *testing.T) {
		_, err := parseRootArgs([]string{"--vaka-file", "x.yml", "up"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "requires '=' form") {
			t.Fatalf("error %q does not contain '=' form guidance", err.Error())
		}
	})

	t.Run("unknown vaka flag before subcommand suggests known flag", func(t *testing.T) {
		_, err := parseRootArgs([]string{"--vaka-flie=x.yml", "up"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "did you mean \"--vaka-file\"") {
			t.Fatalf("error %q missing suggestion", err.Error())
		}
	})

	t.Run("combined strict vaka flags before subcommand works", func(t *testing.T) {
		root, err := parseRootArgs([]string{"--vaka-file=x.yml", "--vaka-init-present", "up"})
		if err != nil {
			t.Fatalf("parseRootArgs: %v", err)
		}
		if root.VakaFile != "x.yml" {
			t.Fatalf("VakaFile=%q, want x.yml", root.VakaFile)
		}
		if !root.VakaInitPresent {
			t.Fatal("VakaInitPresent=false, want true")
		}
	})
}
