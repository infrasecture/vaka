// cmd/vaka/compose_load_test.go
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	composecli "github.com/compose-spec/compose-go/v2/cli"
)

// TestComposeGoBuildOnlyImageNotPopulated pins compose-go's loader behavior
// for services that declare only `build:` without an explicit `image:` key.
//
// compose-go v2 does NOT auto-populate ServiceConfig.Image during project
// load; it stays empty. Because ResolveRuntime in cmd/vaka/images.go
// returns an error when Image is empty and no compose-declared entrypoint
// exists, build-only services without an explicit entrypoint: key fail with
// a clear message. This test ensures the loader assumption matches reality;
// if a future compose-go release starts synthesizing the image name (as the
// CLI does at build time: <project>-<service>:latest), this test will
// break and the limitation can be lifted.
//
// User-facing workaround: add `image:` or `entrypoint:` to the compose
// service. Documented in README under "Build-only services".
func TestComposeGoBuildOnlyImageNotPopulated(t *testing.T) {
	dir := t.TempDir()
	composeFile := filepath.Join(dir, "docker-compose.yaml")
	yaml := `
name: testproj
services:
  app:
    build: .
`
	if err := os.WriteFile(composeFile, []byte(yaml), 0644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	opts, err := composecli.NewProjectOptions([]string{composeFile},
		composecli.WithWorkingDirectory(dir),
		composecli.WithOsEnv,
		composecli.WithDotEnv,
		composecli.WithName("testproj"),
	)
	if err != nil {
		t.Fatalf("project options: %v", err)
	}
	project, err := opts.LoadProject(context.Background())
	if err != nil {
		t.Fatalf("load project: %v", err)
	}

	app, ok := project.Services["app"]
	if !ok {
		t.Fatal("service \"app\" not found in loaded project")
	}
	if app.Build == nil {
		t.Fatal("expected Build to be populated for build-only service")
	}
	if app.Image != "" {
		t.Fatalf("compose-go populated Image=%q for a build-only service; the "+
			"ResolveRuntime build-only limitation documented in README can "+
			"now be lifted", app.Image)
	}
}
