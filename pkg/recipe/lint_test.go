package recipe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeRecipeDir(t *testing.T, composeYAML, vakaYAML string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vaka.yaml"), []byte(vakaYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLintDirClean(t *testing.T) {
	dir := writeRecipeDir(t, `
services:
  app:
    image: alpine:3.20
    volumes:
      - ./conf.yaml:/app/conf.yaml:ro
  gateway:
    image: alpine:3.20
`, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
  gateway:
    network:
      egress:
        defaultAction: reject
`)

	sum, err := LintDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("LintDir: %v", err)
	}
	if len(sum.RiskFlags) != 0 {
		t.Fatalf("clean recipe has flags: %v", sum.RiskFlags)
	}
	want := map[string]string{"app": "reject", "gateway": "reject"}
	if !reflect.DeepEqual(sum.DefaultActions, want) {
		t.Fatalf("defaultActions = %v, want %v", sum.DefaultActions, want)
	}
}

func TestLintDirIgnoresComposeFileEnv(t *testing.T) {
	// The recipe under lint is clean.
	dir := writeRecipeDir(t, `
services:
  app:
    image: alpine:3.20
`, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`)

	// A decoy project elsewhere is dangerous. If lint honored COMPOSE_FILE it
	// would analyze the decoy and report its risk flags.
	decoy := filepath.Join(t.TempDir(), "decoy-compose.yaml")
	if err := os.WriteFile(decoy, []byte("services:\n  evil:\n    image: x\n    privileged: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOSE_FILE", decoy)

	sum, err := LintDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("LintDir: %v", err)
	}
	if _, ok := sum.DefaultActions["evil"]; ok {
		t.Fatal("lint was redirected to the decoy project via COMPOSE_FILE")
	}
	if _, ok := sum.DefaultActions["app"]; !ok {
		t.Fatalf("lint did not analyze the recipe's own service: %v", sum.DefaultActions)
	}
	if len(sum.RiskFlags) != 0 {
		t.Fatalf("clean recipe reported flags (decoy leakage?): %v", sum.RiskFlags)
	}
}

func TestLintDirFlagsEverything(t *testing.T) {
	dir := writeRecipeDir(t, `
services:
  risky:
    image: alpine:3.20
    privileged: true
    cap_add: [SYS_ADMIN]
    network_mode: host
    pid: host
    ipc: host
    labels:
      agent.vaka.init: present
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /:/host
  unpoliced:
    image: alpine:3.20
`, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  risky:
    network:
      egress:
        defaultAction: accept
`)

	sum, err := LintDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("LintDir: %v", err)
	}
	want := []string{
		"risky:" + FlagBroadBindMount,
		"risky:" + FlagCapAddBroad,
		"risky:" + FlagDisablesVakaInit,
		"risky:" + FlagDockerSocketMount,
		"risky:" + FlagEgressDefaultAcc,
		"risky:" + FlagHostIPC,
		"risky:" + FlagHostNetwork,
		"risky:" + FlagHostPID,
		"risky:" + FlagPrivileged,
		"unpoliced:" + FlagNoPolicyForSvc,
	}
	if !reflect.DeepEqual(sum.RiskFlags, want) {
		t.Fatalf("flags:\ngot  %v\nwant %v", sum.RiskFlags, want)
	}
	if sum.DefaultActions["risky"] != "accept" || sum.DefaultActions["unpoliced"] != "none" {
		t.Fatalf("defaultActions = %v", sum.DefaultActions)
	}
}

func TestLintDirAgainstLiveStyleRecipe(t *testing.T) {
	// The extractor fixture (goodRecipe) is not policy-complete; use a
	// codex-shaped pair: two services, one talking to the other only.
	dir := writeRecipeDir(t, `
services:
  agent:
    image: alpine:3.20
    volumes:
      - ${WORKSPACE_DIR:-./}:/workspace
  gateway:
    image: alpine:3.20
    expose: ["4000"]
`, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  agent:
    network:
      egress:
        defaultAction: reject
        accept:
          - dns: {}
          - proto: tcp
            to: [gateway]
            ports: [4000]
  gateway:
    network:
      egress:
        defaultAction: reject
        accept:
          - dns: {}
`)
	sum, err := LintDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("LintDir: %v", err)
	}
	if len(sum.RiskFlags) != 0 {
		t.Fatalf("flags = %v, want none (the relative workspace mount must not count as broad)", sum.RiskFlags)
	}
}

func TestCompareWithIndex(t *testing.T) {
	local := &LocalPolicySummary{
		DefaultActions: map[string]string{"app": "reject"},
		RiskFlags:      []string{"app:privileged"},
	}

	t.Run("absent index block is fine", func(t *testing.T) {
		if w := CompareWithIndex(local, nil, nil, "demo@1.0.0"); w != nil {
			t.Fatalf("warnings = %v", w)
		}
	})

	t.Run("matching block is silent", func(t *testing.T) {
		w := CompareWithIndex(local,
			map[string]string{"app": "reject"}, []string{"app:privileged"}, "demo@1.0.0")
		if len(w) != 0 {
			t.Fatalf("warnings = %v", w)
		}
	})

	t.Run("differing flags warn loudly but never fail", func(t *testing.T) {
		w := CompareWithIndex(local,
			map[string]string{"app": "reject"}, []string{}, "demo@1.0.0")
		if len(w) != 1 || !strings.Contains(w[0], "stale/inaccurate") || !strings.Contains(w[0], "risk-flag list") {
			t.Fatalf("warnings = %v", w)
		}
	})

	t.Run("differing actions warn", func(t *testing.T) {
		w := CompareWithIndex(local,
			map[string]string{"app": "accept"}, []string{"app:privileged"}, "demo@1.0.0")
		if len(w) != 1 || !strings.Contains(w[0], "default-action table") {
			t.Fatalf("warnings = %v", w)
		}
	})
}
