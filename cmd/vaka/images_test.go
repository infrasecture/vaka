package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

type fakeCall struct {
	globals []string
	args    []string
}

type fakeOutput struct {
	stdout string
	stderr string
	err    error
}

type fakeDockerCLI struct {
	outputByKey  map[string]fakeOutput
	execErrByKey map[string]error
	outputCalls  []fakeCall
	execCalls    []fakeCall
}

func (f *fakeDockerCLI) output(_ context.Context, globals []string, args []string) ([]byte, []byte, error) {
	f.outputCalls = append(f.outputCalls, fakeCall{
		globals: append([]string{}, globals...),
		args:    append([]string{}, args...),
	})
	o, ok := f.outputByKey[key(args)]
	if !ok {
		return nil, nil, errors.New("unexpected docker output invocation")
	}
	return []byte(o.stdout), []byte(o.stderr), o.err
}

func (f *fakeDockerCLI) exec(_ context.Context, globals []string, args []string, _, _ io.Writer) error {
	f.execCalls = append(f.execCalls, fakeCall{
		globals: append([]string{}, globals...),
		args:    append([]string{}, args...),
	})
	if err, ok := f.execErrByKey[key(args)]; ok {
		return err
	}
	return nil
}

func key(args []string) string { return strings.Join(args, "\x00") }

func TestNewDockerServicesUsesContextFlag(t *testing.T) {
	ds, err := NewDockerServices([]string{"--context", "desktop-linux", "up"})
	if err != nil {
		t.Fatalf("NewDockerServices: %v", err)
	}
	impl := ds.(*dockerServices)
	want := []string{"--context", "desktop-linux"}
	assertArgv(t, want, impl.dockerGlobalArgs)
}

func TestImageExistsPresent(t *testing.T) {
	f := &fakeDockerCLI{
		outputByKey: map[string]fakeOutput{
			key([]string{"image", "inspect", "nginx:latest"}): {},
		},
	}
	ds := &dockerServices{
		targetDesc: "test-context",
		outputFn:   f.output,
	}
	ok, err := ds.ImageExists(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatalf("ImageExists: %v", err)
	}
	if !ok {
		t.Fatalf("ImageExists=false, want true")
	}
}

func TestImageExistsAbsent(t *testing.T) {
	f := &fakeDockerCLI{
		outputByKey: map[string]fakeOutput{
			key([]string{"image", "inspect", "nginx:latest"}): {
				stderr: "Error response from daemon: No such image: nginx:latest",
				err:    errors.New("exit status 1"),
			},
		},
	}
	ds := &dockerServices{
		targetDesc: "test-context",
		outputFn:   f.output,
	}
	ok, err := ds.ImageExists(context.Background(), "nginx:latest")
	if err != nil {
		t.Fatalf("ImageExists unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("ImageExists=true, want false")
	}
}

func TestEnsureImagePullWhenMissing(t *testing.T) {
	f := &fakeDockerCLI{
		outputByKey: map[string]fakeOutput{
			key([]string{"image", "inspect", "emsi/vaka-init:v0.1.0"}): {
				stderr: "No such image",
				err:    errors.New("exit status 1"),
			},
		},
		execErrByKey: map[string]error{},
	}
	ds := &dockerServices{
		targetDesc: "test-context",
		outputFn:   f.output,
		execFn: func(ctx context.Context, globals []string, args []string, stdout, stderr io.Writer) error {
			return f.exec(ctx, globals, args, stdout, stderr)
		},
	}
	if err := ds.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0"); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	if len(f.execCalls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(f.execCalls))
	}
	want := []string{"pull", "emsi/vaka-init:v0.1.0"}
	assertArgv(t, want, f.execCalls[0].args)
}

func TestResolveRuntimeMatrix(t *testing.T) {
	imgEP := []string{"/docker-entrypoint.sh"}
	imgCmd := []string{"nginx", "-g", "daemon off;"}
	imgUser := "1001:1002"
	cfgJSON := `{"Entrypoint":["/docker-entrypoint.sh"],"Cmd":["nginx","-g","daemon off;"],"User":"1001:1002"}`

	tests := []struct {
		name          string
		composeEP     []string
		composeCmd    []string
		composeUser   string
		wantEP        []string
		wantCmd       []string
		wantImageUser string
		wantInspect   bool
	}{
		{
			name:          "entrypoint and user set no inspect",
			composeEP:     []string{"/app"},
			composeCmd:    []string{"--flag"},
			composeUser:   "app",
			wantEP:        []string{"/app"},
			wantCmd:       []string{"--flag"},
			wantImageUser: "",
			wantInspect:   false,
		},
		{
			name:          "command only with compose user set needs image entrypoint",
			composeEP:     nil,
			composeCmd:    []string{"worker"},
			composeUser:   "1000",
			wantEP:        imgEP,
			wantCmd:       []string{"worker"},
			wantImageUser: "",
			wantInspect:   true,
		},
		{
			name:          "neither entrypoint nor user set",
			composeEP:     nil,
			composeCmd:    nil,
			composeUser:   "",
			wantEP:        imgEP,
			wantCmd:       imgCmd,
			wantImageUser: imgUser,
			wantInspect:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDockerCLI{
				outputByKey: map[string]fakeOutput{
					key([]string{"image", "inspect", "--format", "{{json .Config}}", "nginx:latest"}): {
						stdout: cfgJSON,
					},
				},
			}
			ds := &dockerServices{
				targetDesc: "test-context",
				outputFn:   f.output,
			}
			svc := composetypes.ServiceConfig{
				Image:      "nginx:latest",
				Entrypoint: tc.composeEP,
				Command:    tc.composeCmd,
				User:       tc.composeUser,
			}
			got, err := ds.ResolveRuntime(context.Background(), "web", svc)
			if err != nil {
				t.Fatalf("ResolveRuntime: %v", err)
			}
			if !strEq(got.Entrypoint, tc.wantEP) {
				t.Errorf("entrypoint=%v want=%v", got.Entrypoint, tc.wantEP)
			}
			if !strEq(got.Command, tc.wantCmd) {
				t.Errorf("command=%v want=%v", got.Command, tc.wantCmd)
			}
			if got.ImageUser != tc.wantImageUser {
				t.Errorf("image user=%q want=%q", got.ImageUser, tc.wantImageUser)
			}
			if tc.wantInspect && len(f.outputCalls) == 0 {
				t.Fatalf("expected inspect call")
			}
			if !tc.wantInspect && len(f.outputCalls) != 0 {
				t.Fatalf("unexpected inspect call(s): %d", len(f.outputCalls))
			}
		})
	}
}

func TestResolveRuntimeImageNotFound(t *testing.T) {
	f := &fakeDockerCLI{
		outputByKey: map[string]fakeOutput{
			key([]string{"image", "inspect", "--format", "{{json .Config}}", "myapp:latest"}): {
				stderr: "No such image: myapp:latest",
				err:    errors.New("exit status 1"),
			},
		},
	}
	ds := &dockerServices{
		targetDesc: "context \"dev\"",
		outputFn:   f.output,
	}
	_, err := ds.ResolveRuntime(context.Background(), "myapp", composetypes.ServiceConfig{Image: "myapp:latest"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not available locally") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRuntimeNoImageNeedsFallback(t *testing.T) {
	ds := &dockerServices{targetDesc: "test-context"}
	_, err := ds.ResolveRuntime(context.Background(), "svc", composetypes.ServiceConfig{Command: []string{"worker"}})
	if err == nil {
		t.Fatalf("expected fallback error")
	}
}

func strEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
