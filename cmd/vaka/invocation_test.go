package main

import (
	"strings"
	"testing"
)

func TestInjectStdinOverride(t *testing.T) {
	t.Run("last -f gets -f - appended after it", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"-f", "a.yaml", "-f", "b.yaml", "up", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		got := injectStdinOverride(inv, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "b.yaml", "-f", "-", "up", "--build"}
		assertArgv(t, want, got)
	})

	t.Run("--file=value single-token form", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"--file=a.yaml", "up"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		got := injectStdinOverride(inv, nil)
		want := []string{"compose", "--file=a.yaml", "-f", "-", "up"}
		assertArgv(t, want, got)
	})

	t.Run("-f before -- is found; -f after -- is ignored", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"-f", "a.yaml", "run", "--", "-f", "trick"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		got := injectStdinOverride(inv, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "-", "run", "--", "-f", "trick"}
		assertArgv(t, want, got)
	})

	t.Run("no -f: inject discovered defaults then -f -", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"up", "--build"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		defaults := []string{"docker-compose.yaml", "docker-compose.override.yaml"}
		got := injectStdinOverride(inv, defaults)
		want := []string{
			"compose",
			"-f", "docker-compose.yaml",
			"-f", "docker-compose.override.yaml",
			"-f", "-",
			"up", "--build",
		}
		assertArgv(t, want, got)
	})
}

func TestParseInvocationVakaFlagExtraction(t *testing.T) {
	t.Run("extracts --vaka-file and preserves compose args", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"--vaka-file", "vaka.yaml", "-f", "a.yaml", "up", "--build"})
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

	t.Run("--vaka-file after run service is not extracted", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"run", "gateway", "mytool", "--vaka-file", "/app/cfg.yaml"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if len(inv.VakaFlags) != 0 {
			t.Fatalf("expected no vaka flags extracted, got %v", inv.VakaFlags)
		}
		assertArgv(t, []string{"run", "gateway", "mytool", "--vaka-file", "/app/cfg.yaml"}, inv.ComposeArgs)
	})

	t.Run("--vaka-init-present between subcommand and first positional is extracted", func(t *testing.T) {
		inv, err := ParseInvocation([]string{"run", "--vaka-init-present", "gateway", "mytool"})
		if err != nil {
			t.Fatalf("ParseInvocation: %v", err)
		}
		if inv.VakaFlags["--vaka-init-present"] != "true" {
			t.Fatalf("expected --vaka-init-present=true, got %v", inv.VakaFlags)
		}
		assertArgv(t, []string{"run", "gateway", "mytool"}, inv.ComposeArgs)
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
