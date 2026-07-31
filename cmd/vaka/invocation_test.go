package main

import (
	"strings"
	"testing"
)

func TestInjectFDOverride(t *testing.T) {
	t.Run("last -f gets -f /dev/fd/3 appended after it", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{"-f", "a.yaml", "-f", "b.yaml", "up", "--build"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		got := injectFDOverride(inv, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "b.yaml", "-f", composeOverridePath, "up", "--build"}
		assertArgv(t, want, got)
	})

	t.Run("--file=value single-token form", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{"--file=a.yaml", "up"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		got := injectFDOverride(inv, nil)
		want := []string{"compose", "--file=a.yaml", "-f", composeOverridePath, "up"}
		assertArgv(t, want, got)
	})

	t.Run("-f before -- is found; -f after -- is ignored", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{"-f", "a.yaml", "run", "--", "-f", "trick"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		got := injectFDOverride(inv, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", composeOverridePath, "run", "--", "-f", "trick"}
		assertArgv(t, want, got)
	})

	t.Run("no -f: inject discovered defaults then -f /dev/fd/3", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{"up", "--build"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
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

func TestParseComposeInvocationMetadata(t *testing.T) {
	t.Run("collects file globals in order before subcommand", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{"-f", "a.yaml", "--file", "b.yaml", "--file=c.yaml", "up"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		assertArgv(t, []string{"a.yaml", "b.yaml", "c.yaml"}, inv.GlobalFiles)
	})

	t.Run("subcommand detection skips value-taking compose globals", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{"--ansi", "always", "-f", "a.yaml", "run", "--rm", "svc"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		if inv.Subcommand != "run" {
			t.Fatalf("Subcommand=%q, want run", inv.Subcommand)
		}
	})

	t.Run("captures project-affecting compose globals", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{
			"--project-name", "demo",
			"--profile=tools",
			"--profile", "debug",
			"--env-file", "base.env",
			"--env-file=local.env",
			"up",
		})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		if inv.ProjectName != "demo" {
			t.Fatalf("ProjectName = %q, want demo", inv.ProjectName)
		}
		assertArgv(t, []string{"tools", "debug"}, inv.Profiles)
		assertArgv(t, []string{"base.env", "local.env"}, inv.EnvFiles)
	})

	t.Run("build detection follows subcommand flags until --", func(t *testing.T) {
		inv, err := ParseComposeInvocation([]string{"run", "svc", "mycmd", "--build"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		if !inv.BuildRequested {
			t.Fatalf("BuildRequested=false, want true")
		}

		inv, err = ParseComposeInvocation([]string{"run", "svc", "mycmd", "--", "--build"})
		if err != nil {
			t.Fatalf("ParseComposeInvocation: %v", err)
		}
		if inv.BuildRequested {
			t.Fatalf("BuildRequested=true, want false")
		}
	})
}

func TestParseComposeInvocationRejectsDockerGlobals(t *testing.T) {
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
			_, err := ParseComposeInvocation(tc.args)
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
