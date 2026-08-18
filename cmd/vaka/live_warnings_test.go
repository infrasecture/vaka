package main

import (
	"strings"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"vaka.dev/vaka/internal/runtimebundle"
	"vaka.dev/vaka/pkg/compose"
	"vaka.dev/vaka/pkg/policy"
)

func liveWarningInspect(t *testing.T, host *containertypes.HostConfig, drops []string, defaultAction string) containertypes.InspectResponse {
	t.Helper()
	if host == nil {
		host = &containertypes.HostConfig{}
	}
	p := &policy.ServicePolicy{
		APIVersion: "agent.vaka/v1alpha1",
		Kind:       "ServicePolicy",
		Services: map[string]*policy.ServiceConfig{
			"app": {
				Network: &policy.NetworkConfig{Egress: &policy.EgressPolicy{DefaultAction: defaultAction}},
			},
		},
	}
	payload, revision, err := buildServicePolicyPayload(p, "app", "0:0", nil, drops)
	if err != nil {
		t.Fatal(err)
	}
	return containertypes.InspectResponse{
		ContainerJSONBase: &containertypes.ContainerJSONBase{ID: "container", HostConfig: host},
		Config: &containertypes.Config{
			Env: []string{
				runtimebundle.PolicyEnvironment + "=" + payload,
				runtimebundle.PolicyRevisionEnvironment + "=" + revision,
			},
			Labels: map[string]string{compose.PolicyRevisionLabel: revision},
		},
	}
}

func TestDeriveLiveEnforcementWarningsUsesInjectedDropContract(t *testing.T) {
	safe := liveWarningInspect(t, &containertypes.HostConfig{
		CapAdd: []string{"NET_ADMIN", "SETUID", "SETGID", "SETPCAP"},
	}, []string{"NET_ADMIN", "SETUID", "SETGID", "SETPCAP"}, "reject")
	if got := deriveLiveEnforcementWarnings(safe); len(got.reasons) != 0 || got.defaultActionAccept {
		t.Fatalf("safe Vaka capability delta warned: %+v", got)
	}
	prefixedDrop := liveWarningInspect(t, &containertypes.HostConfig{
		CapAdd: []string{"NET_ADMIN"},
	}, []string{"CAP_NET_ADMIN"}, "reject")
	if got := deriveLiveEnforcementWarnings(prefixedDrop); len(got.reasons) != 0 {
		t.Fatalf("valid prefixed capability drop warned: %+v", got)
	}

	retained := liveWarningInspect(t, &containertypes.HostConfig{
		CapAdd: []string{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE"},
	}, nil, "reject")
	got := strings.Join(deriveLiveEnforcementWarnings(retained).reasons, "\n")
	for _, capability := range []string{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE"} {
		if !strings.Contains(got, "retains "+capability) {
			t.Errorf("live warnings %q omit %s", got, capability)
		}
	}
}

func TestDeriveLiveEnforcementWarningsReadsHostConfigAndMounts(t *testing.T) {
	initEnabled := true
	inspect := liveWarningInspect(t, &containertypes.HostConfig{
		Init:        &initEnabled,
		PidMode:     "host",
		Privileged:  true,
		SecurityOpt: []string{"seccomp=unconfined"},
	}, nil, "accept")
	inspect.AppArmorProfile = "unconfined"
	inspect.Mounts = []containertypes.MountPoint{{
		Type:        mount.TypeBind,
		Source:      "/var/run/docker.sock",
		Destination: "/var/run/docker.sock",
		RW:          true,
	}}

	warnings := deriveLiveEnforcementWarnings(inspect)
	got := strings.Join(warnings.reasons, "\n")
	for _, want := range []string{
		"is privileged",
		"seccomp unconfined",
		"container-runtime daemon access",
		"shares a PID namespace",
		"uses init: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("live warnings %q omit %q", got, want)
		}
	}
	if !warnings.defaultActionAccept {
		t.Fatal("live defaultAction: accept was not retained")
	}
}

func TestDeriveLiveEnforcementWarningsFailsSafeOnUnreadablePolicy(t *testing.T) {
	inspect := containertypes.InspectResponse{
		ContainerJSONBase: &containertypes.ContainerJSONBase{HostConfig: &containertypes.HostConfig{
			CapAdd: []string{"NET_ADMIN"},
		}},
		Config: &containertypes.Config{Env: []string{runtimebundle.PolicyEnvironment + "=not-base64"}},
	}
	warnings := strings.Join(deriveLiveEnforcementWarnings(inspect).reasons, "\n")
	if !strings.Contains(warnings, "drop contract cannot be verified") {
		t.Fatalf("missing unverifiable-contract warning: %q", warnings)
	}
	if strings.Contains(warnings, "retains NET_ADMIN") {
		t.Fatalf("temporary capability was misreported without a readable contract: %q", warnings)
	}
}

func TestContainerEnvironmentValueMatchesVakaInitFirstEntry(t *testing.T) {
	value, ok := containerEnvironmentValue([]string{"A=old", "B=value", "A=new=with-equals"}, "A")
	if !ok || value != "old" {
		t.Fatalf("value = %q, %t", value, ok)
	}
}

func TestReverseNamespaceWarningReasonsResolveLiveIDsAndNames(t *testing.T) {
	managedID := strings.Repeat("a", 64)
	containers := []liveNamespaceContainer{
		{id: managedID, service: "app", names: []string{"demo-app-1"}, managed: true},
		{id: strings.Repeat("b", 64), service: "network-peer", networkMode: "container:" + managedID[:16]},
		{id: strings.Repeat("c", 64), service: "pid-peer", pidMode: "container:demo-app-1"},
		{id: strings.Repeat("d", 64), service: "managed-peer", managed: true, networkMode: "container:" + managedID},
		{id: strings.Repeat("e", 64), service: "unrelated", networkMode: "container:outside-project"},
	}

	reasons := strings.Join(reverseNamespaceWarningReasons(containers, map[string]bool{managedID: true})[managedID], "\n")
	for _, want := range []string{
		"shares its network namespace with unmanaged live container bbbbbbbbbbbb for service network-peer",
		"shares its PID namespace with unmanaged live container cccccccccccc for service pid-peer",
	} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reverse namespace reasons %q omit %q", reasons, want)
		}
	}
	if strings.Contains(reasons, "managed-peer") || strings.Contains(reasons, "unrelated") {
		t.Fatalf("reverse namespace reasons include an inapplicable peer: %q", reasons)
	}
}

func TestResolveLiveContainerReferenceRejectsAmbiguousPrefix(t *testing.T) {
	containers := []liveNamespaceContainer{
		{id: "abcdef111111", managed: true},
		{id: "abcdef222222", managed: true},
	}
	if _, ok := resolveLiveContainerReference("abcdef", containers); ok {
		t.Fatal("ambiguous live container ID prefix resolved")
	}
	if got, ok := resolveLiveContainerReference("abcdef111111", containers); !ok || got.id != "abcdef111111" {
		t.Fatalf("exact live container ID = %+v, %t", got, ok)
	}
}
