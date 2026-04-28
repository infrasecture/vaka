package main

import (
	"strings"
	"testing"
)

func TestInjectFDOverride(t *testing.T) {
	t.Run("last -f gets -f /dev/fd/3 appended after it", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"-f", "a.yaml", "-f", "b.yaml", "up", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		got := injectFDOverride(inv, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "b.yaml", "-f", composeOverridePath, "up", "--build"}
		assertArgv(t, want, got)
	})

	t.Run("--file=value single-token form", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"--file=a.yaml", "up"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		got := injectFDOverride(inv, nil)
		want := []string{"compose", "--file=a.yaml", "-f", composeOverridePath, "up"}
		assertArgv(t, want, got)
	})

	t.Run("-f before -- is found; -f after -- is ignored", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"-f", "a.yaml", "run", "--", "-f", "trick"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		got := injectFDOverride(inv, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", composeOverridePath, "run", "--", "-f", "trick"}
		assertArgv(t, want, got)
	})

	t.Run("no -f: inject discovered defaults then -f /dev/fd/3", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"up", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		defaults := []string{"docker-compose.yaml", "docker-compose.override.yaml"}
		got := injectFDOverride(inv, defaults)
		want := []string{
			"compose",
			"-f", "docker-compose.yaml",
			"-f", "docker-compose.override.yaml",
			"-f", composeOverridePath,
			"up", "--build",
		}
		assertArgv(t, want, got)
	})
}

func TestParseInvocationVakaFlagExtraction(t *testing.T) {
	t.Run("extracts --vaka-file=<path> and preserves compose args", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"--vaka-file=vaka.yaml", "-f", "a.yaml", "up", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if inv.VakaFlags["--vaka-file"] != "vaka.yaml" {
			t.Fatalf("expected vaka-file=vaka.yaml, got %v", inv.VakaFlags)
		}
		want := []string{"-f", "a.yaml", "up", "--build"}
		assertArgv(t, want, inv.ComposeArgs)
	})

	t.Run("no vaka flags: args unchanged", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"up", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		assertArgv(t, []string{"up", "--build"}, inv.ComposeArgs)
	})

	t.Run("unknown --vaka-* after subcommand is forwarded verbatim", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"run", "gateway", "mytool", "--vaka-anything"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if len(inv.VakaFlags) != 0 {
			t.Fatalf("expected no vaka flags extracted, got %v", inv.VakaFlags)
		}
		assertArgv(t, []string{"run", "gateway", "mytool", "--vaka-anything"}, inv.ComposeArgs)
	})

	t.Run("known vaka flag after subcommand hard-errors", func(t *testing.T) {
		_, err := ParseInvocation([]string{"up", "--vaka-file=x.yml"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "must appear before subcommand") {
			t.Fatalf("error %q does not contain positioning hint", err.Error())
		}
	})

	t.Run("vaka flag after -- is not extracted", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"--", "--vaka-file", "x"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if len(inv.VakaFlags) != 0 {
			t.Fatalf("expected no vaka flags extracted, got %v", inv.VakaFlags)
		}
		assertArgv(t, []string{"--", "--vaka-file", "x"}, inv.ComposeArgs)
	})
}

func TestParseInvocationVakaStrictRules(t *testing.T) {
	t.Run("space form for --vaka-file is rejected", func(t *testing.T) {
		_, err := ParseInvocation([]string{"--vaka-file", "x.yml", "up"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "requires '=' form") {
			t.Fatalf("error %q does not contain '=' form guidance", err.Error())
		}
	})

	t.Run("unknown vaka flag before subcommand suggests known flag", func(t *testing.T) {
		_, err := ParseInvocation([]string{"--vaka-flie=x.yml", "up"})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "did you mean \"--vaka-file\"") {
			t.Fatalf("error %q missing suggestion", err.Error())
		}
	})

	t.Run("combined strict vaka flags before subcommand works", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"--vaka-file=x.yml", "--vaka-init-present", "up"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if inv.VakaFlags["--vaka-file"] != "x.yml" {
			t.Fatalf("vaka-file=%q, want x.yml", inv.VakaFlags["--vaka-file"])
		}
		if inv.VakaFlags["--vaka-init-present"] != "true" {
			t.Fatalf("--vaka-init-present=%q, want true", inv.VakaFlags["--vaka-init-present"])
		}
	})
}

func TestParseInvocationMetadata(t *testing.T) {
	t.Run("collects file globals in order before subcommand", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"-f", "a.yaml", "--file", "b.yaml", "--file=c.yaml", "up"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		assertArgv(t, []string{"a.yaml", "b.yaml", "c.yaml"}, inv.GlobalFiles)
	})

	t.Run("subcommand detection skips value-taking compose globals", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"--ansi", "always", "-f", "a.yaml", "run", "--rm", "svc"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if inv.Subcommand != "run" {
			t.Fatalf("Subcommand=%q, want run", inv.Subcommand)
		}
	})

	t.Run("build detection follows subcommand flags until --", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"run", "svc", "mycmd", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if !inv.BuildRequested {
			t.Fatalf("BuildRequested=false, want true")
		}

		inv, err = ParseInvocation([]string{"run", "svc", "mycmd", "--", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if inv.BuildRequested {
			t.Fatalf("BuildRequested=true, want false")
		}
	})
}

func TestParseInvocationRejectsDockerGlobals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "--context rejected",
			args: []string{"--context", "rootless", "up"},
			want: "docker context use rootless",
		},
		{
			name: "--host rejected",
			args: []string{"--host", "ssh://user@remote", "up"},
			want: "DOCKER_HOST=ssh://user@remote",
		},
		{
			name: "--debug rejected",
			args: []string{"--debug", "up"},
			want: "docker top-level --debug is not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseInvocation(tc.args)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func assertArgv(t *testing.T, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("length mismatch\nwant %v\n got  %v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("index %d: want %q got %q\nfull want: %v\nfull got:  %v", i, want[i], got[i], want, got)
		}
	}
}
