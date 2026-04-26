package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

func TestParseComposeMajorVersion(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "v2.27.0", want: 2},
		{in: "2.23.3", want: 2},
		{in: "1.29.0", want: 1},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseComposeMajorVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRunDoctorChecksTimeout(t *testing.T) {
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "times out",
			required: true,
			timeout:  25 * time.Millisecond,
			run: func(ctx context.Context) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		},
	}, false)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].ok {
		t.Fatalf("expected failed result")
	}
	if !strings.Contains(results[0].errText, "timed out after") {
		t.Fatalf("unexpected err text: %q", results[0].errText)
	}
}

func TestRunDoctorChecksInformationalResult(t *testing.T) {
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "info check",
			required: false,
			run: func(context.Context) (string, error) {
				return "", errors.New("probe unavailable")
			},
		},
	}, false)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].required {
		t.Fatalf("expected informational (required=false) result")
	}
	if results[0].ok {
		t.Fatalf("expected failed informational probe")
	}
}

func TestRunDoctorChecksFixesThenPasses(t *testing.T) {
	fixed := false
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "fixable",
			required: true,
			run: func(context.Context) (string, error) {
				if !fixed {
					return "", errors.New("broken")
				}
				return "ok now", nil
			},
			fix: func(context.Context) (string, error) {
				fixed = true
				return "applied fix", nil
			},
		},
	}, true)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	r := results[0]
	if !r.ok {
		t.Fatalf("expected check to pass after fix, got fail: %s", r.errText)
	}
	if !r.fixAttempted || !r.fixApplied {
		t.Fatalf("expected fixAttempted=true and fixApplied=true, got attempted=%v applied=%v", r.fixAttempted, r.fixApplied)
	}
	if r.fixErrText != "" {
		t.Fatalf("unexpected fixErrText: %q", r.fixErrText)
	}
	if r.detail != "ok now" {
		t.Fatalf("detail = %q, want %q", r.detail, "ok now")
	}
	if r.fixDetail != "applied fix" {
		t.Fatalf("fixDetail = %q, want %q", r.fixDetail, "applied fix")
	}
}

func TestRunDoctorChecksFixAttemptFails(t *testing.T) {
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "fixable-fails",
			required: true,
			run: func(context.Context) (string, error) {
				return "", errors.New("broken")
			},
			fix: func(context.Context) (string, error) {
				return "", errors.New("cannot auto-fix")
			},
		},
	}, true)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	r := results[0]
	if r.ok {
		t.Fatalf("expected failed result")
	}
	if !r.fixAttempted {
		t.Fatalf("expected fixAttempted=true")
	}
	if r.fixApplied {
		t.Fatalf("expected fixApplied=false")
	}
	if !strings.Contains(r.fixErrText, "cannot auto-fix") {
		t.Fatalf("fixErrText = %q, want contains %q", r.fixErrText, "cannot auto-fix")
	}
}

type fakeDoctorDockerServices struct {
	imageExists       bool
	imageExistsErr    error
	ensureErr         error
	imageExistsCalled int
	ensureCalled      int
	lastExistsRef     string
	lastEnsureRef     string
}

func (f *fakeDoctorDockerServices) EnsureImage(_ context.Context, ref string) error {
	f.ensureCalled++
	f.lastEnsureRef = ref
	return f.ensureErr
}

func (f *fakeDoctorDockerServices) ImageExists(_ context.Context, ref string) (bool, error) {
	f.imageExistsCalled++
	f.lastExistsRef = ref
	return f.imageExists, f.imageExistsErr
}

func (f *fakeDoctorDockerServices) ResolveRuntime(_ context.Context, _ string, _ composetypes.ServiceConfig) (ResolvedRuntime, error) {
	return ResolvedRuntime{}, errors.New("not implemented")
}

func mustDoctorCheckByName(t *testing.T, checks []doctorCheck, name string) doctorCheck {
	t.Helper()
	for _, c := range checks {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("doctor check %q not found", name)
	return doctorCheck{}
}

func TestDoctorCheckRequiredVakaInitImageMissing(t *testing.T) {
	origNewDoctorDockerServices := newDoctorDockerServices
	defer func() { newDoctorDockerServices = origNewDoctorDockerServices }()

	fake := &fakeDoctorDockerServices{imageExists: false}
	newDoctorDockerServices = func(args []string) (DockerServices, error) {
		if len(args) != 0 {
			t.Fatalf("newDoctorDockerServices args = %v, want empty", args)
		}
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	_, err := check.run(context.Background())
	if err == nil {
		t.Fatal("expected missing-image error, got nil")
	}
	expectedRef := vakaInitBaseImage + ":" + version
	if !strings.Contains(err.Error(), expectedRef) {
		t.Fatalf("error %q does not contain image ref %q", err.Error(), expectedRef)
	}
	if fake.imageExistsCalled != 1 {
		t.Fatalf("ImageExists called %d times, want 1", fake.imageExistsCalled)
	}
	if fake.ensureCalled != 0 {
		t.Fatalf("EnsureImage called %d times, want 0", fake.ensureCalled)
	}
}

func TestDoctorFixPullsRequiredVakaInitImage(t *testing.T) {
	origNewDoctorDockerServices := newDoctorDockerServices
	defer func() { newDoctorDockerServices = origNewDoctorDockerServices }()

	fake := &fakeDoctorDockerServices{}
	newDoctorDockerServices = func(args []string) (DockerServices, error) {
		if len(args) != 0 {
			t.Fatalf("newDoctorDockerServices args = %v, want empty", args)
		}
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	gotDetail, err := check.fix(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedRef := vakaInitBaseImage + ":" + version
	wantFixDetail := "pulled " + expectedRef
	if gotDetail != wantFixDetail {
		t.Fatalf("detail = %q, want %q", gotDetail, wantFixDetail)
	}
	if fake.ensureCalled != 1 {
		t.Fatalf("EnsureImage called %d times, want 1", fake.ensureCalled)
	}
	if fake.lastEnsureRef != expectedRef {
		t.Fatalf("EnsureImage ref = %q, want %q", fake.lastEnsureRef, expectedRef)
	}
	if fake.imageExistsCalled != 0 {
		t.Fatalf("ImageExists called %d times, want 0", fake.imageExistsCalled)
	}
}
