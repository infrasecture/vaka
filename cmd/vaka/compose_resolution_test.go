package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

func TestEffectiveComposeEnvFiles(t *testing.T) {
	t.Run("ambient comma-separated default", func(t *testing.T) {
		t.Setenv(composeEnvFilesVariable, "base.env,, local.env,")
		got := effectiveComposeEnvFiles(nil)
		assertArgv(t, []string{"base.env", " local.env"}, got)
	})

	t.Run("explicit flags replace ambient default", func(t *testing.T) {
		t.Setenv(composeEnvFilesVariable, "ambient.env")
		explicit := []string{"base.env", "local.env"}
		got := effectiveComposeEnvFiles(explicit)
		assertArgv(t, explicit, got)
		got[0] = "changed.env"
		if explicit[0] != "base.env" {
			t.Fatal("effective env files alias the explicit invocation slice")
		}
	})

	t.Run("empty ambient value retains default dotenv behavior", func(t *testing.T) {
		t.Setenv(composeEnvFilesVariable, ",,")
		if got := effectiveComposeEnvFiles(nil); len(got) != 0 {
			t.Fatalf("effective env files = %v, want none", got)
		}
	})
}

func TestComposeResolutionUsesAmbientEnvFilesForProfilesAndInterpolation(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	composeFile := filepath.Join(dir, "compose.yaml")
	baseEnv := filepath.Join(dir, "base.env")
	overrideEnv := filepath.Join(dir, "override.env")
	if err := os.WriteFile(composeFile, []byte(`
services:
  base:
    image: ${APP_IMAGE}
  debug:
    image: alpine:3.20
    profiles: [debug]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baseEnv, []byte("APP_IMAGE=busybox:1.36\nCOMPOSE_PROFILES=debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overrideEnv, []byte("APP_IMAGE=alpine:3.20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"APP_IMAGE", "COMPOSE_FILE", "COMPOSE_PROFILES"} {
		unsetEnvironmentForTest(t, key)
	}
	t.Setenv(composeEnvFilesVariable, baseEnv+","+overrideEnv)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "implicit compose file", args: []string{"up"}},
		{name: "explicit compose file", args: []string{"-f", composeFile, "up"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := ParseComposeInvocation(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			input, err := resolveComposeInput(inv)
			if err != nil {
				t.Fatal(err)
			}
			assertArgv(t, []string{baseEnv, overrideEnv}, input.EnvFiles)
			opts, err := newComposeProjectOptions(input, false)
			if err != nil {
				t.Fatal(err)
			}
			project, err := opts.LoadProject(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := project.Services["debug"]; !ok {
				t.Fatal("COMPOSE_ENV_FILES did not activate the debug profile")
			}
			if got := project.Services["base"].Image; got != "alpine:3.20" {
				t.Fatalf("base image = %q, want later ambient env file value", got)
			}
		})
	}
}

func TestExplicitComposeEnvFileReplacesAmbientDefault(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	composeFile := filepath.Join(dir, "compose.yaml")
	ambientEnv := filepath.Join(dir, "ambient.env")
	explicitEnv := filepath.Join(dir, "explicit.env")
	if err := os.WriteFile(composeFile, []byte(`
services:
  ambient:
    image: alpine:3.20
    profiles: [ambient]
  explicit:
    image: alpine:3.20
    profiles: [explicit]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ambientEnv, []byte("COMPOSE_PROFILES=ambient\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(explicitEnv, []byte("COMPOSE_PROFILES=explicit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsetEnvironmentForTest(t, "COMPOSE_PROFILES")
	t.Setenv(composeEnvFilesVariable, ambientEnv)

	inv, err := ParseComposeInvocation([]string{"--env-file", explicitEnv, "-f", composeFile, "up"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := resolveComposeInput(inv)
	if err != nil {
		t.Fatal(err)
	}
	assertArgv(t, []string{explicitEnv}, input.EnvFiles)
	opts, err := newComposeProjectOptions(input, false)
	if err != nil {
		t.Fatal(err)
	}
	project, err := opts.LoadProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := project.Services["explicit"]; !ok {
		t.Fatal("explicit --env-file profile is not active")
	}
	if _, ok := project.Services["ambient"]; ok {
		t.Fatal("ambient COMPOSE_ENV_FILES augmented explicit --env-file")
	}
}

func TestComposeEnvFileDefaultAndDisableBehavior(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	composeFile := filepath.Join(dir, "compose.yaml")
	emptyEnv := filepath.Join(dir, "empty.env")
	if err := os.WriteFile(composeFile, []byte(`
services:
  base:
    image: alpine:3.20
  dotenv:
    image: alpine:3.20
    profiles: [dotenv]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("COMPOSE_PROFILES=dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emptyEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unsetEnvironmentForTest(t, "COMPOSE_PROFILES")

	for _, tc := range []struct {
		name        string
		envFiles    string
		disable     string
		wantProfile bool
	}{
		{name: "ordinary dotenv fallback", wantProfile: true},
		{name: "ambient file replaces dotenv fallback", envFiles: emptyEnv},
		{name: "default dotenv disabled", disable: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(composeEnvFilesVariable, tc.envFiles)
			if tc.disable == "" {
				unsetEnvironmentForTest(t, "COMPOSE_DISABLE_ENV_FILE")
			} else {
				t.Setenv("COMPOSE_DISABLE_ENV_FILE", tc.disable)
			}
			inv, err := ParseComposeInvocation([]string{"-f", composeFile, "up"})
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
			_, active := project.Services["dotenv"]
			if active != tc.wantProfile {
				t.Fatalf("dotenv profile active = %v, want %v", active, tc.wantProfile)
			}
		})
	}
}

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

func TestComposeResolutionPreservesLauncherAndProjectDotEnv(t *testing.T) {
	root := t.TempDir()
	launcher := filepath.Join(root, "launcher")
	projectDir := filepath.Join(root, "project")
	for _, dir := range []string{launcher, projectDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	chdirForTest(t, launcher)
	for _, key := range []string{
		"COMPOSE_FILE", "COMPOSE_PROFILES", "COMPOSE_PROJECT_NAME",
		"APP_IMAGE", "SHARED_VALUE", "PROJECT_ONLY",
	} {
		unsetEnvironmentForTest(t, key)
	}

	if err := os.WriteFile(filepath.Join(launcher, ".env"), []byte(strings.TrimSpace(`
COMPOSE_FILE=../project/compose.yaml
COMPOSE_PROFILES=debug
COMPOSE_PROJECT_NAME=launcher-project
APP_IMAGE=busybox:1.36
SHARED_VALUE=launcher
`)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".env"), []byte(strings.TrimSpace(`
SHARED_VALUE=project
PROJECT_ONLY=from-project
`)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "compose.yaml"), []byte(`
services:
  base:
    image: ${APP_IMAGE}
    environment:
      SHARED_VALUE: ${SHARED_VALUE}
      PROJECT_ONLY: ${PROJECT_ONLY}
      COMPOSE_FILE_VALUE: ${COMPOSE_FILE}
  debug:
    image: alpine:3.20
    profiles: [debug]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	inv, err := ParseComposeInvocation([]string{"up"})
	if err != nil {
		t.Fatal(err)
	}
	input, err := resolveComposeInput(inv)
	if err != nil {
		t.Fatal(err)
	}
	if !input.ImplicitFiles || !input.EnvironmentResolved {
		t.Fatalf("implicit resolution metadata = %+v", input)
	}
	assertArgv(t, []string{filepath.Join(projectDir, "compose.yaml")}, input.Files)
	if input.Environment["SHARED_VALUE"] != "launcher" {
		t.Fatalf("launcher dotenv did not retain precedence: %q", input.Environment["SHARED_VALUE"])
	}
	if input.Environment["PROJECT_ONLY"] != "from-project" {
		t.Fatalf("project dotenv value = %q", input.Environment["PROJECT_ONLY"])
	}

	opts, err := newComposeProjectOptions(input, false)
	if err != nil {
		t.Fatal(err)
	}
	project, err := opts.LoadProject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "launcher-project" {
		t.Fatalf("project name = %q", project.Name)
	}
	if _, ok := project.Services["debug"]; !ok {
		t.Fatal("launcher COMPOSE_PROFILES did not enable debug")
	}
	base := project.Services["base"]
	if base.Image != "busybox:1.36" {
		t.Fatalf("base image = %q", base.Image)
	}
	for key, want := range map[string]string{
		"SHARED_VALUE":       "launcher",
		"PROJECT_ONLY":       "from-project",
		"COMPOSE_FILE_VALUE": "../project/compose.yaml",
	} {
		value, ok := base.Environment[key]
		if !ok || value == nil || *value != want {
			t.Errorf("base environment %s = %v, want %q", key, value, want)
		}
	}
}

func TestComposeResolutionExplicitProfileAndOSStillWin(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("COMPOSE_PROFILES=dotenv\nVALUE=dotenv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`
services:
  explicit:
    image: ${VALUE}
    profiles: [explicit]
  dotenv:
    image: alpine:3.20
    profiles: [dotenv]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VALUE", "from-os")
	unsetEnvironmentForTest(t, "COMPOSE_PROFILES")

	inv, err := ParseComposeInvocation([]string{"--profile", "explicit", "up"})
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
	if project.Services["explicit"].Image != "from-os" {
		t.Fatalf("OS interpolation lost precedence: %q", project.Services["explicit"].Image)
	}
	if _, ok := project.Services["dotenv"]; ok {
		t.Fatal("dotenv profile augmented explicit --profile")
	}
}

func TestResolvedComposeDotEnvRoundTripsWithoutOSPromotion(t *testing.T) {
	unsetEnvironmentForTest(t, "VAKA_TEST_EMPTY")
	unsetEnvironmentForTest(t, "VAKA_TEST_COMPLEX")
	t.Setenv("VAKA_TEST_OS", "from-os")

	raw := resolvedComposeDotEnv(map[string]string{
		"VAKA_TEST_EMPTY":   "",
		"VAKA_TEST_COMPLEX": "line one\nline 'two' $VALUE \\ tail",
		"VAKA_TEST_OS":      "must-not-be-written",
	})
	if strings.Contains(raw, "VAKA_TEST_OS") {
		t.Fatalf("real OS value was serialized: %q", raw)
	}
	parsed, err := dotenv.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if parsed["VAKA_TEST_EMPTY"] != "" || parsed["VAKA_TEST_COMPLEX"] != "line one\nline 'two' $VALUE \\ tail" {
		t.Fatalf("dotenv round trip = %#v", parsed)
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

func unsetEnvironmentForTest(t *testing.T, key string) {
	t.Helper()
	value, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
