package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

func TestCheckComposeCompatibility(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{in: "v2.35.0"},
		{in: "2.35.1"},
		{in: "5.3.1"},
		{in: "2.34.0", wantErr: true},
		{in: "1.29.0", wantErr: true},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			err := checkComposeCompatibility(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCheckDockerCompatibility(t *testing.T) {
	tests := []struct {
		name    string
		engine  string
		api     string
		wantErr bool
	}{
		{name: "minimum", engine: "28.0.0", api: "1.48"},
		{name: "current", engine: "29.6.2", api: "1.55"},
		{name: "old engine", engine: "27.5.1", api: "1.48", wantErr: true},
		{name: "old API", engine: "28.0.0", api: "1.47", wantErr: true},
		{name: "prerelease", engine: "28.0.0-rc.1", api: "1.48", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDockerCompatibility(tc.engine, tc.api)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkDockerCompatibility(%q, %q) error = %v, wantErr=%v", tc.engine, tc.api, err, tc.wantErr)
			}
		})
	}
}

func TestCheckDockerClientCompatibility(t *testing.T) {
	if err := checkDockerClientCompatibility("1.48"); err != nil {
		t.Fatalf("minimum client API rejected: %v", err)
	}
	err := checkDockerClientCompatibility("1.47")
	if err == nil || !strings.Contains(err.Error(), "Docker client API") {
		t.Fatalf("old client API error = %v", err)
	}
}

func TestDoctorDockerCompatibilityRejectsPinnedOldClientAPI(t *testing.T) {
	originalProbe := doctorDockerProbe
	t.Cleanup(func() { doctorDockerProbe = originalProbe })
	doctorDockerProbe = func(_ context.Context, args []string) (string, string, error) {
		if !strings.Contains(strings.Join(args, " "), ".Client.APIVersion") {
			t.Fatalf("docker version probe does not request client API: %v", args)
		}
		return "28.0.0 1.48 1.47", "", nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "docker engine compatible")
	_, err := check.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Docker client API") {
		t.Fatalf("pinned old client API error = %v", err)
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

func (f *fakeDoctorDockerServices) CheckRuntimeCompatibility(context.Context) error { return nil }

func (f *fakeDoctorDockerServices) ResolveRuntimeImage(_ context.Context, ref, _ string, repair bool) (ResolvedImage, error) {
	if repair {
		f.ensureCalled++
		f.lastEnsureRef = ref
		if f.ensureErr != nil {
			return ResolvedImage{}, f.ensureErr
		}
		f.imageExists = true
		return ResolvedImage{ID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
	}
	f.imageExistsCalled++
	f.lastExistsRef = ref
	if f.imageExistsErr != nil {
		return ResolvedImage{}, f.imageExistsErr
	}
	if !f.imageExists {
		return ResolvedImage{}, fmt.Errorf("runtime image %s is missing", ref)
	}
	return ResolvedImage{ID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, nil
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
	newDoctorDockerServices = func(inv *ComposeInvocation, _ PullPolicy) (DockerServices, error) {
		if inv != nil {
			t.Fatalf("newDoctorDockerServices invocation = %#v, want nil", inv)
		}
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	_, err := check.run(context.Background())
	if err == nil {
		t.Fatal("expected missing-image error, got nil")
	}
	expectedRef := vakaInitImageReference()
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
	newDoctorDockerServices = func(inv *ComposeInvocation, _ PullPolicy) (DockerServices, error) {
		if inv != nil {
			t.Fatalf("newDoctorDockerServices invocation = %#v, want nil", inv)
		}
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	gotDetail, err := check.run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedRef := vakaInitImageReference()
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
	newDoctorDockerServices = func(inv *ComposeInvocation, _ PullPolicy) (DockerServices, error) {
		if inv != nil {
			t.Fatalf("newDoctorDockerServices invocation = %#v, want nil", inv)
		}
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	gotDetail, err := check.fix(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedRef := vakaInitImageReference()
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

func TestDoctorDevBuildUsesFixableRuntimeBundleImage(t *testing.T) {
	origNewDoctorDockerServices := newDoctorDockerServices
	defer func() { newDoctorDockerServices = origNewDoctorDockerServices }()
	origVersion := version
	version = "dev"
	defer func() { version = origVersion }()

	fake := &fakeDoctorDockerServices{imageExists: false}
	newDoctorDockerServices = func(inv *ComposeInvocation, _ PullPolicy) (DockerServices, error) {
		return fake, nil
	}

	check := mustDoctorCheckByName(t, defaultDoctorChecks(), "required vaka-init image present")
	if check.fix == nil {
		t.Fatal("dev build runtime image check should be fixable")
	}
	_, err := check.run(context.Background())
	if err == nil {
		t.Fatal("expected missing-image error, got nil")
	}
	if !strings.Contains(err.Error(), vakaInitImageReference()) {
		t.Fatalf("error = %q, want runtime image %q", err.Error(), vakaInitImageReference())
	}
	if strings.Contains(vakaInitImageReference(), version) {
		t.Fatalf("runtime image %q must not depend on CLI version %q", vakaInitImageReference(), version)
	}

	results := runDoctorChecks(context.Background(), []doctorCheck{check}, true)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if !results[0].fixAttempted || !results[0].fixApplied {
		t.Fatalf("fixAttempted=%v fixApplied=%v, want successful repair", results[0].fixAttempted, results[0].fixApplied)
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
	newDoctorDockerServices = func(inv *ComposeInvocation, _ PullPolicy) (DockerServices, error) {
		if inv != nil {
			t.Fatalf("newDoctorDockerServices invocation = %#v, want nil", inv)
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
