package main

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"vaka.dev/vaka/pkg/compose"
)

func TestParseExecOptionsAndBoundary(t *testing.T) {
	parsed, err := parseExec([]string{
		"-it", "-e", "A=1", "--env=B=2", "--index", "2", "-w/tmp",
		"--user", "1001:1002", "app", "sh", "-c", "printf '%s' --privileged",
	})
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	if parsed.service != "app" || parsed.index != 2 || parsed.user != "1001:1002" {
		t.Fatalf("parsed target = %+v", parsed)
	}
	wantCommand := []string{"sh", "-c", "printf '%s' --privileged"}
	if !reflect.DeepEqual(parsed.command, wantCommand) {
		t.Fatalf("command = %v, want %v", parsed.command, wantCommand)
	}
	wantPrefix := []string{"-it", "-e", "A=1", "--env=B=2", "--index", "2", "-w/tmp"}
	if !reflect.DeepEqual(parsed.prefix, wantPrefix) {
		t.Fatalf("prefix = %v, want %v", parsed.prefix, wantPrefix)
	}
}

func TestParseExecCompactEqualsUser(t *testing.T) {
	parsed, err := parseExec([]string{"-u=1000", "app", "id"})
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	if parsed.user != "1000" {
		t.Fatalf("user = %q, want 1000", parsed.user)
	}
}

func TestParseExecBooleanAliasesUseLastValue(t *testing.T) {
	parsed, err := parseExec([]string{
		"--privileged", "--privileged=false",
		"--interactive", "--interactive=0",
		"--no-TTY", "--no-TTY=false",
		"--detach=false", "-d",
		"app", "id",
	})
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	if parsed.privileged || parsed.interactive || parsed.noTTY || !parsed.detach {
		t.Fatalf("last-value state = %+v", parsed)
	}

	for _, args := range [][]string{
		{"--tty", "--no-TTY=false", "app", "id"},
		{"--interactive=garbage", "app", "id"},
		{"-d=garbage", "app", "id"},
	} {
		if _, err := parseExec(args); err == nil {
			t.Errorf("parseExec(%v) unexpectedly succeeded", args)
		}
	}
}

func TestInspectExecTargetMatchesComposeReplicaSelection(t *testing.T) {
	containers := []containertypes.Summary{
		execContainer("oneoff", "1", true),
		execContainer("replica-2", "2", false),
	}
	containers[0].Labels[compose.ManagedLabel] = "false"
	containers[0].Labels[compose.RuntimeVersionLabel] = "v0.0.1"
	serviceImageID := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for i := range containers {
		containers[i].Labels[compose.ServiceImageLabel] = serviceImageID
	}
	inspectFn := func(id string) (containertypes.InspectResponse, error) {
		var labels map[string]string
		for _, ctr := range containers {
			if ctr.ID == id {
				labels = ctr.Labels
				break
			}
		}
		return containertypes.InspectResponse{
			ContainerJSONBase: &containertypes.ContainerJSONBase{
				ID: id, Image: serviceImageID,
				HostConfig: &containertypes.HostConfig{Mounts: []mount.Mount{{
					Type: mount.TypeImage, Source: testRuntimeImageID, Target: protectedRuntimePath, ReadOnly: true,
					ImageOptions: &mount.ImageOptions{Subpath: "opt/vaka"},
				}}},
			},
			Config: &containertypes.Config{Labels: labels},
			Mounts: []containertypes.MountPoint{
				{Type: mount.TypeImage, Destination: protectedRuntimePath, RW: false},
			},
		}, nil
	}
	ds := &dockerServices{
		c: &fakeDockerClient{inspectResults: map[string]dockerimage.InspectResponse{
			testRuntimeImageID: {ID: testRuntimeImageID},
		}},
		legacy: &fakeLegacyRuntimeClient{listFn: func(containertypes.ListOptions) ([]containertypes.Summary, error) {
			return containers, nil
		}, inspectFn: inspectFn},
		targetDesc: "test-context",
	}
	got, err := ds.InspectExecTarget(context.Background(), "demo", "app", 0)
	if err != nil {
		t.Fatalf("InspectExecTarget default: %v", err)
	}
	if !got.Managed || !got.RuntimeMounted || got.RuntimeVersion != runtimeBundleVersion {
		t.Fatalf("default target = %+v", got)
	}

	got, err = ds.InspectExecTarget(context.Background(), "demo", "app", 2)
	if err != nil {
		t.Fatalf("InspectExecTarget index 2: %v", err)
	}
	if !got.Managed || !got.RuntimeMounted {
		t.Fatalf("indexed target = %+v", got)
	}
}

func TestInspectExecTargetExcludesOneoffs(t *testing.T) {
	ds := &dockerServices{
		legacy: &fakeLegacyRuntimeClient{listFn: func(containertypes.ListOptions) ([]containertypes.Summary, error) {
			return []containertypes.Summary{execContainer("oneoff", "1", true)}, nil
		}},
		targetDesc: "test-context",
	}
	if _, err := ds.InspectExecTarget(context.Background(), "demo", "app", 0); err == nil || !strings.Contains(err.Error(), "no running container") {
		t.Fatalf("one-off-only selection error = %v", err)
	}
	if _, err := ds.InspectExecTarget(context.Background(), "demo", "app", 1); err == nil || !strings.Contains(err.Error(), "no running container at index 1") {
		t.Fatalf("indexed one-off-only selection error = %v", err)
	}
}

func execContainer(id, number string, oneoff bool) containertypes.Summary {
	return containertypes.Summary{
		ID: id,
		Labels: map[string]string{
			composeProjectLabel:         "demo",
			composeServiceLabel:         "app",
			composeConfigHashLabel:      "hash",
			composeContainerNumberLabel: number,
			composeOneoffLabel:          strings.ToLower(strconv.FormatBool(oneoff)),
			compose.ManagedLabel:        "true",
			compose.RuntimeVersionLabel: runtimeBundleVersion,
			compose.RuntimeImageLabel:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		Mounts: []containertypes.MountPoint{{Type: mount.Type("image"), Destination: "/opt/vaka", RW: false}},
	}
}

func validManagedInspectResponse() containertypes.InspectResponse {
	serviceImageID := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	return containertypes.InspectResponse{
		ContainerJSONBase: &containertypes.ContainerJSONBase{
			ID: "container", Image: serviceImageID,
			HostConfig: &containertypes.HostConfig{Mounts: []mount.Mount{{
				Type: mount.TypeImage, Source: testRuntimeImageID, Target: protectedRuntimePath, ReadOnly: true,
				ImageOptions: &mount.ImageOptions{Subpath: "opt/vaka"},
			}}},
		},
		Mounts: []containertypes.MountPoint{
			{Type: mount.TypeImage, Destination: protectedRuntimePath, RW: false},
		},
	}
}

func TestVerifyManagedContainerMountsFailsClosed(t *testing.T) {
	serviceImageID := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	otherRuntimeID := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	tests := []struct {
		name   string
		mutate func(*containertypes.InspectResponse)
		images map[string]dockerimage.InspectResponse
		want   string
	}{
		{name: "valid", images: map[string]dockerimage.InspectResponse{testRuntimeImageID: {ID: testRuntimeImageID}}},
		{name: "nested mount", mutate: func(i *containertypes.InspectResponse) {
			i.Mounts = append(i.Mounts, containertypes.MountPoint{Type: mount.TypeVolume, Destination: protectedRuntimePath + "/sbin", RW: true})
		}, images: map[string]dockerimage.InspectResponse{testRuntimeImageID: {ID: testRuntimeImageID}}, want: "overlaps protected runtime"},
		{name: "wrong subpath", mutate: func(i *containertypes.InspectResponse) {
			i.HostConfig.Mounts[0].ImageOptions.Subpath = "opt/other"
		}, images: map[string]dockerimage.InspectResponse{testRuntimeImageID: {ID: testRuntimeImageID}}, want: "unexpected subpath"},
		{name: "wrong runtime source", images: map[string]dockerimage.InspectResponse{testRuntimeImageID: {ID: otherRuntimeID}}, want: "resolves to"},
		{name: "unexpected policy mount", mutate: func(i *containertypes.InspectResponse) {
			i.Mounts = append(i.Mounts, containertypes.MountPoint{Type: mount.TypeBind, Destination: protectedPolicyPath, RW: false})
		}, images: map[string]dockerimage.InspectResponse{testRuntimeImageID: {ID: testRuntimeImageID}}, want: "reserved policy path"},
		{name: "wrong service image", mutate: func(i *containertypes.InspectResponse) {
			i.Image = testRuntimeImageID
		}, images: map[string]dockerimage.InspectResponse{testRuntimeImageID: {ID: testRuntimeImageID}}, want: "service image"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inspect := validManagedInspectResponse()
			if tc.mutate != nil {
				tc.mutate(&inspect)
			}
			ds := &dockerServices{c: &fakeDockerClient{inspectResults: tc.images}, targetDesc: "test"}
			err := ds.verifyManagedContainerMounts(context.Background(), inspect, testRuntimeImageID, serviceImageID)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLegacyManagedSignature(t *testing.T) {
	tests := []containertypes.InspectResponse{
		{
			Config: &containertypes.Config{User: "0:0", Entrypoint: []string{vakaInitPath, "--"}},
			Mounts: []containertypes.MountPoint{{Type: mount.TypeVolume, Destination: protectedRuntimePath, RW: true}},
		},
		{
			Config: &containertypes.Config{
				User:       "",
				Entrypoint: []string{vakaInitPath},
				Labels:     map[string]string{vakaInitLabel: "present"},
			},
		},
		{
			Config: &containertypes.Config{
				User:       "root:staff",
				Entrypoint: []string{vakaInitPath},
				Labels:     map[string]string{vakaInitLabel: "present"},
			},
		},
	}
	for i, inspect := range tests {
		if !legacyManagedSignature(inspect) {
			t.Errorf("pre-v0.2 Vaka container signature %d was not detected", i)
		}
	}
	inspect := tests[0]
	inspect.Config.Entrypoint = []string{"/usr/local/bin/app"}
	if legacyManagedSignature(inspect) {
		t.Fatal("ordinary root container was classified as legacy-managed")
	}
	inspect = tests[1]
	inspect.Config.User = "1000:1000"
	if legacyManagedSignature(inspect) {
		t.Fatal("non-root container was classified as legacy-managed")
	}
}

func TestParseExecFailsClosedOnAmbiguousOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--future-option", "value", "app", "id"},
		{"--index", "zero", "app", "id"},
		{"app"},
		{},
	} {
		if _, err := parseExec(args); err == nil {
			t.Errorf("parseExec(%v) unexpectedly succeeded", args)
		}
	}
}

func TestSecureExecInvocationUsesRootOnlyForTrampoline(t *testing.T) {
	inv, err := ParseComposeInvocation([]string{"-f", "compose.yml", "exec", "-T", "--user", "1001", "app", "id", "-u"})
	if err != nil {
		t.Fatalf("ParseComposeInvocation: %v", err)
	}
	parsed, err := parseExec(inv.PostSubcommand)
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	secured, err := secureExecInvocation(inv, parsed)
	if err != nil {
		t.Fatalf("secureExecInvocation: %v", err)
	}
	want := []string{
		"-f", "compose.yml", "exec", "-T", "--user=0:0", "app",
		"/opt/vaka/sbin/vaka-init", "exec", "--user", "1001", "--", "id", "-u",
	}
	if !reflect.DeepEqual(secured.Args, want) {
		t.Fatalf("secured args = %v, want %v", secured.Args, want)
	}
}

func TestSecureDockerExecUsesExactContainerID(t *testing.T) {
	parsed, err := parseExec([]string{"-T", "-e", "A=1", "--index", "2", "--user=1001", "app", "id"})
	if err != nil {
		t.Fatalf("parseExec: %v", err)
	}
	got, err := secureDockerExecArgs("exact-container", parsed)
	if err != nil {
		t.Fatalf("secureDockerExecArgs: %v", err)
	}
	want := []string{"exec", "-e", "A=1", "-i", "--user=0:0", "exact-container", vakaInitPath, "exec", "--user", "1001", "--", "id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("docker args = %v, want %v", got, want)
	}
}

func TestSecureDockerExecTTYFollowsComposeOutputDetection(t *testing.T) {
	parsed, err := parseExec([]string{"app", "id"})
	if err != nil {
		t.Fatal(err)
	}
	original := execOutputIsTerminalFn
	t.Cleanup(func() { execOutputIsTerminalFn = original })

	execOutputIsTerminalFn = func() bool { return false }
	withoutTTY, err := secureDockerExecArgs("exact-container", parsed)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(withoutTTY, "-t") {
		t.Fatalf("non-terminal exec requested a TTY: %v", withoutTTY)
	}

	execOutputIsTerminalFn = func() bool { return true }
	withTTY, err := secureDockerExecArgs("exact-container", parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(withTTY, "-t") {
		t.Fatalf("terminal exec omitted its TTY: %v", withTTY)
	}

	noTTY, err := parseExec([]string{"-T", "app", "id"})
	if err != nil {
		t.Fatal(err)
	}
	explicitlyDisabled, err := secureDockerExecArgs("exact-container", noTTY)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(explicitlyDisabled, "-t") {
		t.Fatalf("exec -T requested a TTY: %v", explicitlyDisabled)
	}
}

func TestSecureDockerExecSynthesizesFinalBooleanState(t *testing.T) {
	parsed, err := parseExec([]string{"--interactive=false", "--tty=false", "--detach=false", "app", "id"})
	if err != nil {
		t.Fatal(err)
	}
	original := execOutputIsTerminalFn
	execOutputIsTerminalFn = func() bool { return true }
	t.Cleanup(func() { execOutputIsTerminalFn = original })

	args, err := secureDockerExecArgs("exact-container", parsed)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "-i") || slices.Contains(args, "-t") || slices.Contains(args, "-d") {
		t.Fatalf("disabled exec booleans leaked into docker exec: %v", args)
	}
}

func TestSecureDockerExecRejectsPolicyEnvironmentOverride(t *testing.T) {
	parsed, err := parseExec([]string{"-e", "AGENT_VAKA_POLICY=bad", "app", "id"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secureDockerExecArgs("exact-container", parsed); err == nil || !strings.Contains(err.Error(), "reserved Vaka environment") {
		t.Fatalf("reserved environment error = %v", err)
	}
}

func TestSecureExecRejectsPrivilegedForManagedTarget(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
`)

	calls, err := runRootCapturingExec(t, []string{"exec", "--privileged", "app", "id"})
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("privileged exec error = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("privileged exec must not reach Compose, got %+v", calls)
	}
}

func TestSecureExecRejectsPolicyManagedUnlabeledContainer(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
`)
	setDockerServicesFactoryForTest(t, &fakeBuilderDockerServices{execTargets: map[string]execTarget{
		"app:0": {ContainerID: "plain-compose", Managed: false},
	}})
	var composeCalls int
	setExecDockerComposeForTest(t, func(*ComposeInvocation, string, []string) error {
		composeCalls++
		return nil
	})
	inv, _ := ParseComposeInvocation([]string{"exec", "app", "id"})
	err := runSecureExec("vaka.yaml", inv)
	if err == nil || !strings.Contains(err.Error(), "was not created by Vaka") || !strings.Contains(err.Error(), "raw `docker compose exec`") {
		t.Fatalf("unlabeled policy container error = %v", err)
	}
	if composeCalls != 0 {
		t.Fatalf("raw Compose calls = %d, want zero", composeCalls)
	}
}
