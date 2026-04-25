package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveComposeInputDefaultsComposeYaml(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	writeComposeFile(t, filepath.Join(dir, "compose.yaml"))
	writeComposeFile(t, filepath.Join(dir, "compose.override.yaml"))

	got, err := resolveComposeInput([]string{"up"})
	if err != nil {
		t.Fatalf("resolveComposeInput: %v", err)
	}

	want := []string{
		filepath.Join(dir, "compose.yaml"),
		filepath.Join(dir, "compose.override.yaml"),
	}
	assertArgv(t, want, got.Files)
}

func TestResolveComposeInputDefaultsDockerComposeFallback(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	writeComposeFile(t, filepath.Join(dir, "docker-compose.yaml"))
	writeComposeFile(t, filepath.Join(dir, "docker-compose.override.yml"))

	got, err := resolveComposeInput([]string{"up"})
	if err != nil {
		t.Fatalf("resolveComposeInput: %v", err)
	}

	want := []string{
		filepath.Join(dir, "docker-compose.yaml"),
		filepath.Join(dir, "docker-compose.override.yml"),
	}
	assertArgv(t, want, got.Files)
}

func TestResolveComposeInputTraversesParents(t *testing.T) {
	parent := t.TempDir()
	writeComposeFile(t, filepath.Join(parent, "compose.yml"))

	child := filepath.Join(parent, "nested", "deep")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	chdirForTest(t, child)

	got, err := resolveComposeInput([]string{"up"})
	if err != nil {
		t.Fatalf("resolveComposeInput: %v", err)
	}

	want := []string{filepath.Join(parent, "compose.yml")}
	assertArgv(t, want, got.Files)
}

func TestResolveComposeInputUsesComposeFileEnv(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	writeComposeFile(t, a)
	writeComposeFile(t, b)

	t.Setenv("COMPOSE_FILE", a+string(os.PathListSeparator)+b)

	got, err := resolveComposeInput([]string{"up"})
	if err != nil {
		t.Fatalf("resolveComposeInput: %v", err)
	}
	want := []string{a, b}
	assertArgv(t, want, got.Files)
}

func TestResolveComposeInputHonorsProjectDirectory(t *testing.T) {
	projectDir := t.TempDir()
	writeComposeFile(t, filepath.Join(projectDir, "compose.yaml"))

	otherDir := t.TempDir()
	chdirForTest(t, otherDir)

	got, err := resolveComposeInput([]string{"--project-directory", projectDir, "up"})
	if err != nil {
		t.Fatalf("resolveComposeInput: %v", err)
	}
	want := []string{filepath.Join(projectDir, "compose.yaml")}
	assertArgv(t, want, got.Files)
	if got.WorkingDir == "" {
		t.Fatalf("expected non-empty working dir for --project-directory")
	}
}

func TestResolveComposeInputExplicitFilesWinOverEnv(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	explicit := filepath.Join(dir, "explicit.yaml")
	viaEnv := filepath.Join(dir, "env.yaml")
	writeComposeFile(t, explicit)
	writeComposeFile(t, viaEnv)
	t.Setenv("COMPOSE_FILE", viaEnv)

	got, err := resolveComposeInput([]string{"-f", explicit, "up"})
	if err != nil {
		t.Fatalf("resolveComposeInput: %v", err)
	}
	want := []string{explicit}
	assertArgv(t, want, got.Files)
}

func TestProjectDirectoryFromArgs(t *testing.T) {
	got := projectDirectoryFromArgs([]string{"--project-directory", "/tmp/proj", "up"})
	if got != "/tmp/proj" {
		t.Fatalf("projectDirectoryFromArgs=%q, want /tmp/proj", got)
	}
	got = projectDirectoryFromArgs([]string{"--project-directory=/tmp/a", "--project-directory=/tmp/b", "up"})
	if got != "/tmp/b" {
		t.Fatalf("projectDirectoryFromArgs=%q, want /tmp/b", got)
	}
}

func writeComposeFile(t *testing.T, path string) {
	t.Helper()
	const yaml = "services:\n  app:\n    image: alpine:3.20\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write compose file %s: %v", path, err)
	}
}
