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

func TestRunDoctorChecksPreservesOriginalErrorWhenPostFixStillFails(t *testing.T) {
	runCalls := 0
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "fixable-still-fails",
			required: true,
			run: func(context.Context) (string, error) {
				runCalls++
				if runCalls == 1 {
					return "", errors.New("initial probe failure")
				}
				return "", errors.New("post-fix probe failure")
			},
			fix: func(context.Context) (string, error) {
				return "fix applied", nil
			},
		},
	}, true)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	r := results[0]
	if r.ok {
		t.Fatal("expected failed result")
	}
	if r.errText != "initial probe failure" {
		t.Fatalf("errText = %q, want %q", r.errText, "initial probe failure")
	}
	if r.postFixErr != "post-fix probe failure" {
		t.Fatalf("postFixErr = %q, want %q", r.postFixErr, "post-fix probe failure")
	}
	if !r.fixApplied {
		t.Fatal("expected fixApplied=true")
	}
}

func TestRunDoctorChecksSkipsWhenPrerequisiteFails(t *testing.T) {
	imageRunCalled := 0
	results := runDoctorChecks(context.Background(), []doctorCheck{
		{
			name:     "docker daemon reachable",
			required: true,
			run: func(context.Context) (string, error) {
				return "", errors.New("daemon unavailable")
			},
		},
		{
			name:      "required vaka-init image present",
			required:  true,
			dependsOn: []string{"docker daemon reachable"},
			run: func(context.Context) (string, error) {
				imageRunCalled++
				return "should not run", nil
			},
			fix: func(context.Context) (string, error) {
				t.Fatal("fix should not run when prerequisite fails")
				return "", nil
			},
		},
	}, true)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if imageRunCalled != 0 {
		t.Fatalf("image check run called %d times, want 0", imageRunCalled)
	}
	r := results[1]
	if !r.skipped {
		t.Fatal("expected second check to be skipped")
	}
	if !strings.Contains(r.skipText, "prerequisite") {
		t.Fatalf("skipText = %q, want contains %q", r.skipText, "prerequisite")
	}
	if r.fixAttempted {
		t.Fatal("fixAttempted=true, want false for skipped check")
	}
}

func TestResolveDoctorFixTimeoutDefaultsToGlobalFixTimeout(t *testing.T) {
	got := resolveDoctorFixTimeout(doctorCheck{
		timeout:    doctorProbeTimeout,
		fixTimeout: 0,
	})
	if got != doctorDefaultFixTimeout {
		t.Fatalf("resolveDoctorFixTimeout = %s, want %s", got, doctorDefaultFixTimeout)
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
	if f.ensureErr == nil {
		f.imageExists = true
	}
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
	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

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

func TestDoctorCheckRequiredVakaInitImagePresent(t *testing.T) {
	origNewDoctorDockerServices := newDoctorDockerServices
	defer func() { newDoctorDockerServices = origNewDoctorDockerServices }()
	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

	fake := &fakeDoctorDockerServices{imageExists: true}
	newDoctorDockerServices = func(args []string) (DockerServices, error) {
		if len(args) != 0 {
			t.Fatalf("newDoctorDockerServices args = %v, want empty", args)
		}
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	gotDetail, err := check.run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedRef := vakaInitBaseImage + ":" + version
	if gotDetail != expectedRef {
		t.Fatalf("detail = %q, want %q", gotDetail, expectedRef)
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
	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

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

func TestDoctorRequiredVakaInitImageDevBuildNonFixable(t *testing.T) {
	origNewDoctorDockerServices := newDoctorDockerServices
	defer func() { newDoctorDockerServices = origNewDoctorDockerServices }()
	origVersion := version
	version = "dev"
	defer func() { version = origVersion }()

	ctorCount := 0
	newDoctorDockerServices = func(args []string) (DockerServices, error) {
		ctorCount++
		return &fakeDoctorDockerServices{}, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	if check.fix != nil {
		t.Fatal("dev build check should be non-fixable (fix must be nil)")
	}
	_, err := check.run(context.Background())
	if err == nil {
		t.Fatal("expected dev non-fixable error, got nil")
	}
	if !strings.Contains(err.Error(), "not auto-fixable") {
		t.Fatalf("error = %q, want contains %q", err.Error(), "not auto-fixable")
	}
	if ctorCount != 0 {
		t.Fatalf("docker services constructor called %d times, want 0", ctorCount)
	}

	results := runDoctorChecks(context.Background(), []doctorCheck{check}, true)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].fixAttempted {
		t.Fatal("fixAttempted=true, want false for non-fixable dev check")
	}
}

func TestDoctorRequiredVakaInitImageFixReusesDockerServicesCache(t *testing.T) {
	origNewDoctorDockerServices := newDoctorDockerServices
	defer func() { newDoctorDockerServices = origNewDoctorDockerServices }()
	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

	fake := &fakeDoctorDockerServices{imageExists: false}
	ctorCount := 0
	newDoctorDockerServices = func(args []string) (DockerServices, error) {
		if len(args) != 0 {
			t.Fatalf("newDoctorDockerServices args = %v, want empty", args)
		}
		ctorCount++
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	results := runDoctorChecks(context.Background(), []doctorCheck{check}, true)
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
	if ctorCount != 1 {
		t.Fatalf("newDoctorDockerServices called %d times, want 1", ctorCount)
	}
	if fake.ensureCalled != 1 {
		t.Fatalf("EnsureImage called %d times, want 1", fake.ensureCalled)
	}
	if fake.imageExistsCalled != 2 {
		t.Fatalf("ImageExists called %d times, want 2 (probe + post-fix re-probe)", fake.imageExistsCalled)
	}
}
