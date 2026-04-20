// cmd/vaka/inject_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInjectStdinOverride(t *testing.T) {
	t.Run("last -f gets -f - appended after it", func(t *testing.T) {
		args := []string{"compose", "-f", "a.yaml", "-f", "b.yaml", "up", "--build"}
		got := injectStdinOverride(args, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "b.yaml", "-f", "-", "up", "--build"}
		assertArgv(t, want, got)
	})

	t.Run("--file=value single-token form", func(t *testing.T) {
		args := []string{"compose", "--file=a.yaml", "up"}
		got := injectStdinOverride(args, nil)
		want := []string{"compose", "--file=a.yaml", "-f", "-", "up"}
		assertArgv(t, want, got)
	})

	t.Run("-f before -- is found; -f after -- is ignored", func(t *testing.T) {
		args := []string{"compose", "-f", "a.yaml", "run", "--", "-f", "trick"}
		got := injectStdinOverride(args, nil)
		want := []string{"compose", "-f", "a.yaml", "-f", "-", "run", "--", "-f", "trick"}
		assertArgv(t, want, got)
	})

	t.Run("no -f: inject discovered defaults then -f -", func(t *testing.T) {
		defaults := []string{"docker-compose.yaml", "docker-compose.override.yaml"}
		args := []string{"compose", "up", "--build"}
		got := injectStdinOverride(args, defaults)
		want := []string{
			"compose",
			"-f", "docker-compose.yaml",
			"-f", "docker-compose.override.yaml",
			"-f", "-",
			"up", "--build",
		}
		assertArgv(t, want, got)
	})

	// NOTE: The "no -f and no defaults" case is not tested here.
	// runInjection returns an error before calling injectStdinOverride when
	// no -f flags are present and discoverComposeFiles returns nothing.
}

func TestExtractVakaFlags(t *testing.T) {
	t.Run("extracts --vaka-file and leaves compose global flags in rest", func(t *testing.T) {
		raw := []string{"--vaka-file", "vaka.yaml", "-f", "a.yaml", "up", "--build"}
		flags, rest := extractVakaFlags(raw)
		if flags["--vaka-file"] != "vaka.yaml" {
			t.Fatalf("expected vaka-file=vaka.yaml, got %v", flags)
		}
		want := []string{"-f", "a.yaml", "up", "--build"}
		assertArgv(t, want, rest)
	})

	t.Run("no vaka flags: argv unchanged", func(t *testing.T) {
		raw := []string{"up", "--build"}
		flags, rest := extractVakaFlags(raw)
		if len(flags) != 0 {
			t.Fatalf("expected no flags, got %v", flags)
		}
		assertArgv(t, raw, rest)
	})
}

func TestDiscoverComposeFiles(t *testing.T) {
	dir := t.TempDir()

	t.Run("yaml primary + override.yml", func(t *testing.T) {
		primary := filepath.Join(dir, "docker-compose.yaml")
		override := filepath.Join(dir, "docker-compose.override.yml")
		os.WriteFile(primary, []byte("version: '3'"), 0644)
		os.WriteFile(override, []byte("version: '3'"), 0644)

		got := discoverComposeFiles(dir)
		want := []string{primary, override}
		assertArgv(t, want, got)

		os.Remove(primary)
		os.Remove(override)
	})

	t.Run("neither exists: empty", func(t *testing.T) {
		got := discoverComposeFiles(dir)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})
}

func TestAllFileFlags(t *testing.T) {
	t.Run("multiple -f forms collected in order", func(t *testing.T) {
		args := []string{"-f", "a.yaml", "--file", "b.yaml", "--file=c.yaml", "up"}
		got := allFileFlags(args)
		want := []string{"a.yaml", "b.yaml", "c.yaml"}
		assertArgv(t, want, got)
	})

	t.Run("stops at --", func(t *testing.T) {
		args := []string{"-f", "a.yaml", "run", "--", "-f", "trick.yaml"}
		got := allFileFlags(args)
		want := []string{"a.yaml"}
		assertArgv(t, want, got)
	})

	t.Run("stops at subcommand: -f after run is not a compose file", func(t *testing.T) {
		// -f output.txt is an arg to the command being run, not a compose file.
		args := []string{"run", "--rm", "svc", "myapp", "-f", "output.txt"}
		got := allFileFlags(args)
		if len(got) != 0 {
			t.Fatalf("expected empty (stopped at subcommand), got %v", got)
		}
	})

	t.Run("unknown value-taking global flag does not swallow subcommand", func(t *testing.T) {
		// --ansi is a value-taking flag; its value must not be mistaken for subcommand.
		args := []string{"--ansi", "always", "-f", "a.yaml", "up"}
		got := allFileFlags(args)
		want := []string{"a.yaml"}
		assertArgv(t, want, got)
	})

	t.Run("no -f flags: empty result", func(t *testing.T) {
		args := []string{"up", "--build"}
		got := allFileFlags(args)
		if len(got) != 0 {
			t.Fatalf("expected empty, got %v", got)
		}
	})
}

func TestHasBuildFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"up --build", []string{"up", "--build"}, true},
		{"up -d (no build)", []string{"up", "-d"}, false},
		{"run --build svc", []string{"run", "--build", "svc"}, true},
		{"-f a.yml up --build", []string{"-f", "a.yml", "up", "--build"}, true},
		{"global flag with value, then up --build", []string{"--ansi", "always", "up", "--build"}, true},
		{"--build before subcommand is ignored (treated as compose global flag)", []string{"--build", "up"}, false},
		{"--build after -- is ignored (inner command arg)", []string{"run", "svc", "mycmd", "--", "--build"}, false},
		// Accepted false positive: --build after the service name on `run` is
		// really an arg to the inner command, but disambiguating requires
		// compose-specific positional semantics. Extra prebuild is a perf
		// cost only; stale entrypoints are a correctness bug.
		{"--build without -- (false positive, safe)", []string{"run", "svc", "mycmd", "--build"}, true},
		{"empty args", []string{}, false},
		{"no subcommand", []string{"-f", "a.yml"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hasBuildFlag(tc.args)
			if got != tc.want {
				t.Errorf("hasBuildFlag(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestFindSubcmd(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"up", "--build"}, "up"},
		{[]string{"-f", "a.yaml", "up", "--build"}, "up"},
		{[]string{"--file", "a.yaml", "-f", "b.yaml", "run", "--rm", "svc"}, "run"},
		{[]string{"--profile", "myprofile", "run", "--rm", "svc"}, "run"},
		{[]string{"--file=a.yaml", "exec", "svc", "bash"}, "exec"},
		{[]string{"-p", "myproject", "ps"}, "ps"},
		// --ansi and --progress take a value; their value must not be mistaken for subcommand.
		{[]string{"--ansi", "always", "up", "--build"}, "up"},
		{[]string{"--progress", "plain", "-f", "a.yaml", "run"}, "run"},
		{[]string{}, ""},
	}
	for _, tc := range tests {
		got := findSubcmd(tc.args)
		if got != tc.want {
			t.Errorf("findSubcmd(%v) = %q, want %q", tc.args, got, tc.want)
		}
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
