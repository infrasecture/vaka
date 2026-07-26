package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/config/configfile"
	clitypes "github.com/docker/cli/cli/config/types"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

func TestParsePullPolicy(t *testing.T) {
	cases := map[string]PullPolicy{
		"":               PullMissingPinned, // default
		"missing-pinned": PullMissingPinned,
		"missing":        PullMissing,
		"never":          PullNever,
		"always":         PullAlways,
	}
	for in, want := range cases {
		got, err := ParsePullPolicy(in)
		if err != nil {
			t.Errorf("ParsePullPolicy(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Errorf("ParsePullPolicy(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParsePullPolicy("sometimes"); err == nil {
		t.Error("ParsePullPolicy(\"sometimes\") expected an error")
	}
}

func TestIsDigestPinned(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	pinned := []string{
		"nginx@sha256:" + hex64,
		"nginx:1.2@sha256:" + hex64,
		"ghcr.io/x/y:tag@sha256:" + hex64,
	}
	unpinned := []string{
		"nginx", "nginx:latest", "nginx@sha256:tooshort",
		"nginx@sha256:" + strings.Repeat("g", 64), // non-hex
	}
	for _, r := range pinned {
		if !isDigestPinned(r) {
			t.Errorf("isDigestPinned(%q) = false, want true", r)
		}
	}
	for _, r := range unpinned {
		if isDigestPinned(r) {
			t.Errorf("isDigestPinned(%q) = true, want false", r)
		}
	}
}

// pullFake is a stateful dockerClient: ImagePull can make a previously-absent
// image present, so pull-then-inspect flows are exercisable.
type pullFake struct {
	present  bool
	pullOK   bool // pull succeeds and makes the image present
	pullErr  error
	pulls    int
	inspects int
}

func (f *pullFake) ImageInspect(context.Context, string, ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	f.inspects++
	if !f.present {
		return dockerimage.InspectResponse{}, errdefs.NotFound(errors.New("not found"))
	}
	return imageConfig([]string{"/app"}, nil, ""), nil
}

func (f *pullFake) ImagePull(context.Context, string, dockerimage.PullOptions) (io.ReadCloser, error) {
	f.pulls++
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	if f.pullOK {
		f.present = true
	}
	return io.NopCloser(strings.NewReader("{}\n")), nil
}

func TestResolveRuntimePullPolicy(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	pinned := "app@sha256:" + hex64
	unpinned := "app:latest"

	cases := []struct {
		name      string
		policy    PullPolicy
		ref       string
		present   bool
		pullOK    bool
		pullErr   error
		wantPulls int
		wantErr   bool
	}{
		{"never: missing → error, no pull", PullNever, pinned, false, true, nil, 0, true},
		{"missing-pinned: missing+pinned → pull", PullMissingPinned, pinned, false, true, nil, 1, false},
		{"missing-pinned: missing+unpinned → no pull, error", PullMissingPinned, unpinned, false, true, nil, 0, true},
		{"missing: missing+unpinned → pull", PullMissing, unpinned, false, true, nil, 1, false},
		{"missing-pinned: present → no pull", PullMissingPinned, pinned, true, true, nil, 0, false},
		{"always: present → pulls anyway", PullAlways, pinned, true, true, nil, 1, false},
		{"missing-pinned: pull fails → error", PullMissingPinned, pinned, false, false, errors.New("net down"), 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &pullFake{present: tc.present, pullOK: tc.pullOK, pullErr: tc.pullErr}
			ds := &dockerServices{c: fake, targetDesc: "test", pullPolicy: tc.policy}
			// Empty entrypoint/user forces image inspection.
			_, err := ds.ResolveRuntime(context.Background(), "app", composetypes.ServiceConfig{Image: tc.ref})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if fake.pulls != tc.wantPulls {
				t.Errorf("pulls = %d, want %d", fake.pulls, tc.wantPulls)
			}
		})
	}
}

func TestPullOptionsAuth(t *testing.T) {
	hex := strings.Repeat("a", 64)
	cfg := configfile.New("")
	cfg.AuthConfigs["myreg.example.com"] = clitypes.AuthConfig{Username: "u", Password: "p"}
	ds := &dockerServices{cfg: cfg, targetDesc: "test"}

	if ds.pullOptions("myreg.example.com/app@sha256:"+hex).RegistryAuth == "" {
		t.Error("configured registry: expected non-empty RegistryAuth")
	}
	if ds.pullOptions("other.example.com/app:latest").RegistryAuth != "" {
		t.Error("unconfigured registry: expected empty RegistryAuth (anonymous)")
	}
	nilCfg := &dockerServices{targetDesc: "test"}
	if nilCfg.pullOptions("x@sha256:"+hex).RegistryAuth != "" {
		t.Error("nil cfg: expected empty RegistryAuth")
	}
}
