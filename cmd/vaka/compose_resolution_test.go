package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestComposeResolutionCarriesProjectProfilesAndEnvFiles(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	composeFile := filepath.Join(dir, "compose.yaml")
	envFile := filepath.Join(dir, "runtime.env")
	if err := os.WriteFile(composeFile, []byte(`
services:
  app:
    image: ${APP_IMAGE}
  tool:
    image: alpine:3.20
    profiles: [tools]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, []byte("APP_IMAGE=busybox:1.36\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := ParseComposeInvocation([]string{
		"--project-name", "selected",
		"--profile", "tools",
		"--env-file", envFile,
		"up",
	})
	if err != nil {
		t.Fatal(err)
	}
	input, err := resolveComposeInput(inv)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := newComposeProjectOptions(input, false)
	if err != nil {
		t.Fatal(err)
	}
	project, err := opts.LoadProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "selected" {
		t.Fatalf("project name = %q, want selected", project.Name)
	}
	if project.Services["app"].Image != "busybox:1.36" {
		t.Fatalf("app image = %q, want env-file interpolation", project.Services["app"].Image)
	}
	if _, ok := project.Services["tool"]; !ok {
		t.Fatal("profile-selected service tool is missing")
	}
}

func TestComposeResolutionUsesComposeProfilesEnvironment(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	composeFile := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte(`
services:
  app:
    image: alpine:3.20
  tool:
    image: alpine:3.20
    profiles: [tools]
  debug:
    image: alpine:3.20
    profiles: [debug]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOSE_PROFILES", "tools")

	inv, err := ParseComposeInvocation([]string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := resolveComposeInput(inv)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := newComposeProjectOptions(input, false)
	if err != nil {
		t.Fatal(err)
	}
	project, err := opts.LoadProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := project.Services["tool"]; !ok {
		t.Fatal("COMPOSE_PROFILES did not enable tool")
	}
	if _, ok := project.DisabledServices["debug"]; !ok {
		t.Fatal("unselected debug profile was not retained as a declared service")
	}

	inv, err = ParseComposeInvocation([]string{"--profile", "debug", "up"})
	if err != nil {
		t.Fatal(err)
	}
	input, err = resolveComposeInput(inv)
	if err != nil {
		t.Fatal(err)
	}
	opts, err = newComposeProjectOptions(input, false)
	if err != nil {
		t.Fatal(err)
	}
	project, err = opts.LoadProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := project.Services["debug"]; !ok {
		t.Fatal("explicit --profile did not enable debug")
	}
	if _, ok := project.DisabledServices["tool"]; !ok {
		t.Fatal("COMPOSE_PROFILES incorrectly augmented explicit --profile")
	}
}

func TestResolveComposeInputDefaultsComposeYaml(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)

	writeComposeFile(t, filepath.Join(dir, "compose.yaml"))
	writeComposeFile(t, filepath.Join(dir, "compose.override.yaml"))

	inv, err := ParseComposeInvocation([]string{"up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	got, err := resolveComposeInput(inv)
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

	inv, err := ParseComposeInvocation([]string{"up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	got, err := resolveComposeInput(inv)
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

	inv, err := ParseComposeInvocation([]string{"up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	got, err := resolveComposeInput(inv)
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

	inv, err := ParseComposeInvocation([]string{"up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	got, err := resolveComposeInput(inv)
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

	inv, err := ParseComposeInvocation([]string{"--project-directory", projectDir, "up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	got, err := resolveComposeInput(inv)
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

	inv, err := ParseComposeInvocation([]string{"-f", explicit, "up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	got, err := resolveComposeInput(inv)
	if err != nil {
		t.Fatalf("resolveComposeInput: %v", err)
	}
	want := []string{explicit}
	assertArgv(t, want, got.Files)
}

func TestParseComposeInvocationProjectDirectory(t *testing.T) {
	inv, err := ParseComposeInvocation([]string{"--project-directory", "/tmp/proj", "up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	if inv.ProjectDirectory != "/tmp/proj" {
		t.Fatalf("ProjectDirectory=%q, want /tmp/proj", inv.ProjectDirectory)
	}

	inv, err = ParseComposeInvocation([]string{"--project-directory=/tmp/a", "--project-directory=/tmp/b", "up"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	if inv.ProjectDirectory != "/tmp/b" {
		t.Fatalf("ProjectDirectory=%q, want /tmp/b", inv.ProjectDirectory)
	}
}

func writeComposeFile(t *testing.T, path string) {
	t.Helper()
	const yaml = "services:\n  app:\n    image: alpine:3.20\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write compose file %s: %v", path, err)
	}
}
