package main

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
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

func TestInspectExecTargetMatchesComposeReplicaSelection(t *testing.T) {
	containers := []containertypes.Summary{
		execContainer("oneoff", "1", true),
		execContainer("replica-2", "2", false),
		execContainer("replica-1", "1", false),
	}
	containers[0].Labels[compose.RuntimeVersionLabel] = "v0.0.1"
	ds := &dockerServices{
		legacy: &fakeLegacyRuntimeClient{listFn: func(containertypes.ListOptions) ([]containertypes.Summary, error) {
			return containers, nil
		}},
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
