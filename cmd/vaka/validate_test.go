package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"vaka.dev/vaka/pkg/policy"
)

func TestLoadAndValidateRecognizesInactiveProfileServices(t *testing.T) {
	dir := t.TempDir()
	chdirForTest(t, dir)
	writeFixtureFiles(t, dir, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  tool:
    network:
      egress:
        defaultAction: reject
`, `
services:
  app:
    image: alpine:3.20
  tool:
    image: alpine:3.20
    profiles: [tools]
`)
	_, project, err := loadAndValidateResolved("vaka.yaml", &composeResolution{
		Files: []string{filepath.Join(dir, "docker-compose.yaml")},
	})
	if err != nil {
		t.Fatalf("inactive managed service was treated as missing: %v", err)
	}
	if _, ok := project.DisabledServices["tool"]; !ok {
		t.Fatal("inactive profile service is not retained in DisabledServices")
	}
}

func TestDegradedEnforcementReasons(t *testing.T) {
	tests := []struct {
		name    string
		svc     composetypes.ServiceConfig
		runtime *policy.RuntimeConfig
		want    []string
	}{
		{name: "ordinary service"},
		{
			name: "privileged",
			svc:  composetypes.ServiceConfig{Privileged: true},
			want: []string{"is privileged and can bypass Vaka's runtime and egress policy"},
		},
		{
			name: "dangerous capabilities",
			svc:  composetypes.ServiceConfig{CapAdd: []string{"CAP_SYS_ADMIN", "net_admin", "sys_ptrace"}},
			want: []string{
				"retains SYS_ADMIN and can replace Vaka's runtime",
				"retains NET_ADMIN and can modify Vaka's nftables policy",
				"retains SYS_PTRACE and can interfere with Vaka's runtime processes",
			},
		},
		{
			name: "all capabilities with explicit policy drops",
			svc:  composetypes.ServiceConfig{CapAdd: []string{"ALL"}},
			runtime: &policy.RuntimeConfig{DropCaps: []string{
				"ALL",
			}},
		},
		{
			name:    "only one dangerous capability retained",
			svc:     composetypes.ServiceConfig{CapAdd: []string{"ALL"}, CapDrop: []string{"NET_ADMIN"}},
			runtime: &policy.RuntimeConfig{DropCaps: []string{"SYS_ADMIN", "SYS_PTRACE"}},
			want:    []string{"requests all Linux capabilities and can weaken Vaka's runtime boundary"},
		},
		{
			name: "daemon and pid namespace access",
			svc: composetypes.ServiceConfig{
				UseAPISocket: true,
				Pid:          "service:peer",
			},
			want: []string{
				"has Docker or container-runtime daemon access and can bypass the container security boundary",
				"shares a PID namespace and can interfere with Vaka's runtime processes",
			},
		},
		{
			name: "host pid namespace",
			svc:  composetypes.ServiceConfig{Pid: " HOST "},
			want: []string{"shares a PID namespace and can interfere with Vaka's runtime processes"},
		},
		{
			name: "docker socket bind",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{
				Type:   composetypes.VolumeTypeBind,
				Source: "/var/run/docker.sock",
				Target: "/var/run/docker.sock",
			}}},
			want: []string{"has Docker or container-runtime daemon access and can bypass the container security boundary"},
		},
		{
			name: "docker runtime directory bind",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{
				Type:   composetypes.VolumeTypeBind,
				Source: "/run",
				Target: "/host-run",
			}}},
			want: []string{"has Docker or container-runtime daemon access and can bypass the container security boundary"},
		},
		{
			name: "container runtime socket bind",
			svc: composetypes.ServiceConfig{Volumes: []composetypes.ServiceVolumeConfig{{
				Type:   composetypes.VolumeTypeBind,
				Source: "/custom/runtime/containerd.sock",
				Target: "/run/containerd/containerd.sock",
			}}},
			want: []string{"has Docker or container-runtime daemon access and can bypass the container security boundary"},
		},
		{
			name: "combined unconfined profiles",
			svc: composetypes.ServiceConfig{SecurityOpt: []string{
				"seccomp=unconfined",
				"apparmor:unconfined",
			}},
			want: []string{"sets seccomp unconfined and disables AppArmor confinement, which can let a root workload manipulate mount namespaces and overmount /vaka"},
		},
		{
			name: "specific powerful capabilities",
			svc:  composetypes.ServiceConfig{CapAdd: []string{"SYS_MODULE", "CAP_BPF", "CHOWN"}},
			want: []string{"retains powerful Linux capabilities (BPF, SYS_MODULE) that can weaken container isolation"},
		},
		{
			name:    "specific powerful capability dropped by policy",
			svc:     composetypes.ServiceConfig{CapAdd: []string{"SYS_MODULE"}},
			runtime: &policy.RuntimeConfig{DropCaps: []string{"SYS_MODULE"}},
		},
		{
			name: "docker init shim",
			svc: composetypes.ServiceConfig{Init: func() *bool {
				value := true
				return &value
			}()},
			want: []string{"uses init: true, leaving Docker's trusted init shim outside Vaka's capability-drop path"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := degradedEnforcementReasons(tc.svc, tc.runtime)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("degradedEnforcementReasons() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMountNamespaceSecurityReasons(t *testing.T) {
	tests := []struct {
		name    string
		options []string
		want    []string
	}{
		{name: "none"},
		{name: "docker defaults", options: []string{"seccomp=builtin", "apparmor=docker-default"}},
		{
			name:    "seccomp unconfined",
			options: []string{"SECCOMP: Unconfined"},
			want:    []string{"sets seccomp unconfined and removes syscall defenses against mount-namespace manipulation"},
		},
		{
			name:    "apparmor unconfined",
			options: []string{"apparmor=unconfined"},
			want:    []string{"sets AppArmor unconfined and removes mount restrictions protecting /vaka"},
		},
		{
			name:    "seccomp and selinux disabled",
			options: []string{"seccomp=unconfined", "label:disable"},
			want:    []string{"sets seccomp unconfined and disables SELinux confinement, which can let a root workload manipulate mount namespaces and overmount /vaka"},
		},
		{
			name:    "all confinement disabled",
			options: []string{"seccomp=unconfined", "apparmor=unconfined", "label=disable"},
			want:    []string{"sets seccomp unconfined and disables AppArmor and SELinux confinement, which can let a root workload manipulate mount namespaces and overmount /vaka"},
		},
		{
			name:    "custom profiles",
			options: []string{"seccomp=/profiles/service.json", "apparmor=service-profile", "no-new-privileges:true"},
			want: []string{
				"uses a custom seccomp profile whose mount-namespace restrictions Vaka cannot verify",
				"uses a custom AppArmor profile whose mount restrictions Vaka cannot verify",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mountNamespaceSecurityReasons(tc.options)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mountNamespaceSecurityReasons() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMountsContainerDaemonSocket(t *testing.T) {
	tests := []struct {
		name    string
		volumes []composetypes.ServiceVolumeConfig
		want    bool
	}{
		{
			name: "known socket",
			volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeBind, Source: "/run/crio/crio.sock", Target: "/runtime.sock",
			}},
			want: true,
		},
		{
			name: "custom socket location",
			volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeBind, Source: "/srv/runtime/podman.sock", Target: "/runtime.sock",
			}},
			want: true,
		},
		{
			name: "parent runtime directory",
			volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeBind, Source: "/run/containerd", Target: "/host-containerd",
			}},
			want: true,
		},
		{
			name: "named volume at conventional target",
			volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeVolume, Source: "runtime-data", Target: "/run/docker.sock",
			}},
		},
		{
			name: "unrelated unix socket",
			volumes: []composetypes.ServiceVolumeConfig{{
				Type: composetypes.VolumeTypeBind, Source: "/run/dbus/system_bus_socket", Target: "/bus.sock",
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mountsContainerDaemonSocket(tc.volumes); got != tc.want {
				t.Fatalf("mountsContainerDaemonSocket() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestWarnDegradedEnforcementReportsOnlyManagedServices(t *testing.T) {
	p := &policy.ServicePolicy{Services: map[string]*policy.ServiceConfig{
		"managed": {},
	}}
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"managed":   {CapAdd: []string{"NET_ADMIN"}},
		"unmanaged": {Privileged: true},
	}}

	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	warnDegradedEnforcement(p, project)
	_ = w.Close()
	os.Stderr = old
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read warning: %v", err)
	}

	got := string(out)
	if !strings.Contains(got, "Vaka warning: service managed retains NET_ADMIN") ||
		!strings.Contains(got, "Vaka enforcement is best-effort for this service.") {
		t.Fatalf("warning = %q", got)
	}
	if strings.Contains(got, "unmanaged") {
		t.Fatalf("warning unexpectedly includes unmanaged service: %q", got)
	}
}

func TestWarnDegradedEnforcementReportsReverseNamespaceSharing(t *testing.T) {
	p := &policy.ServicePolicy{Services: map[string]*policy.ServiceConfig{
		"managed": {},
	}}
	project := &composetypes.Project{Services: map[string]composetypes.ServiceConfig{
		"managed": {ContainerName: "managed-fixed-name"},
		"network-peer": {
			NetworkMode: "service:managed",
		},
		"pid-peer": {
			Pid: "service:managed",
		},
		"container-network-peer": {
			NetworkMode: "container:managed-fixed-name",
		},
		"container-pid-peer": {
			Pid: "container:managed-fixed-name",
		},
		"unrelated": {
			NetworkMode: "service:elsewhere",
		},
	}}

	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	warnDegradedEnforcement(p, project)
	_ = w.Close()
	os.Stderr = old
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read warning: %v", err)
	}

	got := string(out)
	if !strings.Contains(got, "unmanaged service network-peer joins managed service managed's network namespace") ||
		!strings.Contains(got, "unmanaged service pid-peer joins managed service managed's PID namespace") ||
		!strings.Contains(got, "unmanaged service container-network-peer joins managed service managed's network namespace") ||
		!strings.Contains(got, "unmanaged service container-pid-peer joins managed service managed's PID namespace") {
		t.Fatalf("warnings = %q", got)
	}
	if strings.Contains(got, "unrelated") {
		t.Fatalf("warning unexpectedly includes unrelated service: %q", got)
	}
}
