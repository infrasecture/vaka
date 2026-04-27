package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

type fakeBuilderDockerServices struct {
	imageExists map[string]bool
	runtimes    map[string]ResolvedRuntime
	ensureRefs  []string
}

func (f *fakeBuilderDockerServices) EnsureImage(_ context.Context, ref string) error {
	f.ensureRefs = append(f.ensureRefs, ref)
	return nil
}

func (f *fakeBuilderDockerServices) ImageExists(_ context.Context, ref string) (bool, error) {
	return f.imageExists[ref], nil
}

func (f *fakeBuilderDockerServices) ResolveRuntime(_ context.Context, svcName string, svc composetypes.ServiceConfig) (ResolvedRuntime, error) {
	if rt, ok := f.runtimes[svcName]; ok {
		return rt, nil
	}
	return ResolvedRuntime{
		Entrypoint: svc.Entrypoint,
		Command:    svc.Command,
	}, nil
}

func writeFixtureFiles(t *testing.T, dir, policyYAML, composeYAML string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "vaka.yaml"), []byte(strings.TrimSpace(policyYAML)+"\n"), 0o644); err != nil {
		t.Fatalf("write vaka.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yaml"), []byte(strings.TrimSpace(composeYAML)+"\n"), 0o644); err != nil {
		t.Fatalf("write docker-compose.yaml: %v", err)
	}
}

func chdirForTest(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(old)
	})
}

func setDockerServicesFactoryForTest(t *testing.T, ds DockerServices, captures ...*[][]string) {
	t.Helper()
	old := newDockerServices
	newDockerServices = func(inv *Invocation) (DockerServices, error) {
		if len(captures) > 0 && captures[0] != nil {
			*captures[0] = append(*captures[0], append([]string{}, inv.ComposeArgs...))
		}
		return ds, nil
	}
	t.Cleanup(func() {
		newDockerServices = old
	})
}

func setExecDockerComposeForTest(t *testing.T, fn func(inv *Invocation, overrideYAML string, extraEnv []string) error) {
	t.Helper()
	old := execDockerComposeFn
	execDockerComposeFn = fn
	t.Cleanup(func() {
		execDockerComposeFn = old
	})
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	runErr := fn()

	_ = w.Close()
	os.Stdout = old

	out, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatalf("read captured stdout: %v", readErr)
	}
	return string(out), runErr
}
