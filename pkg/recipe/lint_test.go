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

func TestRecipeComposeFilesMatchesComposeGo(t *testing.T) {
	// Precedence/family matrix pinned to compose-go's exported ordering:
	// base = first of [compose.yaml, compose.yml, docker-compose.yml,
	// docker-compose.yaml]; override = first of [compose.override.yml,
	// .yaml, docker-compose.override.yml, .yaml], chosen INDEPENDENTLY of the
	// base family.
	tests := []struct {
		name    string
		present []string
		want    []string
	}{
		{"single base", []string{"compose.yaml"}, []string{"compose.yaml"}},
		{"yaml beats yml for compose", []string{"compose.yaml", "compose.yml"}, []string{"compose.yaml"}},
		{"yml beats yaml for docker-compose", []string{"docker-compose.yaml", "docker-compose.yml"}, []string{"docker-compose.yml"}},
		{"compose beats docker-compose", []string{"compose.yml", "docker-compose.yml"}, []string{"compose.yml"}},
		{"base + its override", []string{"compose.yaml", "compose.override.yaml"}, []string{"compose.yaml", "compose.override.yaml"}},
		{"override yml beats yaml", []string{"compose.yaml", "compose.override.yml", "compose.override.yaml"}, []string{"compose.yaml", "compose.override.yml"}},
		// Override is family-INDEPENDENT: a compose.yaml base picks up a
		// docker-compose.override.yml when no compose.override.* exists — the
		// case the old family-matched logic silently ignored.
		{"cross-family override", []string{"compose.yaml", "docker-compose.override.yml"}, []string{"compose.yaml", "docker-compose.override.yml"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.present {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("services: {}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := recipeComposeFiles(dir)
			if err != nil {
				t.Fatalf("recipeComposeFiles: %v", err)
			}
			var gotNames []string
			for _, g := range got {
				gotNames = append(gotNames, filepath.Base(g))
			}
			if strings.Join(gotNames, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v, want %v", gotNames, tc.want)
			}
		})
	}

	// Unusual entry types must match compose-go's os.Stat semantics.
	t.Run("dangling high-priority symlink is skipped (Stat follows)", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink("does-not-exist", filepath.Join(dir, "compose.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := recipeComposeFiles(dir)
		if err != nil {
			t.Fatalf("recipeComposeFiles: %v", err)
		}
		if len(got) != 1 || filepath.Base(got[0]) != "compose.yml" {
			t.Fatalf("got %v, want [compose.yml] (dangling compose.yaml skipped)", got)
		}
	})

	t.Run("high-priority directory is selected (Stat accepts dirs)", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "compose.yaml"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := recipeComposeFiles(dir)
		if err != nil {
			t.Fatalf("recipeComposeFiles: %v", err)
		}
		// compose-go would select the directory then fail to load it; we
		// match that selection rather than silently picking compose.yml.
		if len(got) != 1 || filepath.Base(got[0]) != "compose.yaml" {
			t.Fatalf("got %v, want [compose.yaml] (directory selected like compose-go)", got)
		}
	})
}

func TestLintDirMergesComposeOverride(t *testing.T) {
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
	// The override — docker's (and vaka's) default customization mechanism —
	// makes the service privileged. The lint must merge it, like vaka up
	// would, or an override could smuggle dangerous config past the lint.
	if err := os.WriteFile(filepath.Join(dir, "compose.override.yaml"),
		[]byte("services:\n  app:\n    privileged: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sum, err := LintDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("LintDir: %v", err)
	}
	found := false
	for _, f := range sum.RiskFlags {
		if f == "app:"+FlagPrivileged {
			found = true
		}
	}
	if !found {
		t.Fatalf("override's privileged: true was not flagged; risk flags = %v", sum.RiskFlags)
	}
}

func TestReadRecipePolicyRefusesEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(secret, []byte("apiVersion: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// vaka.yaml is a symlink escaping the recipe dir.
	if err := os.Symlink(secret, filepath.Join(dir, "vaka.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecipePolicy(dir); err == nil {
		t.Fatal("readRecipePolicy followed a symlink escaping the recipe dir")
	}
}

func TestValidateStagedFailsClosed(t *testing.T) {
	good := `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`
	compose := "services:\n  app:\n    image: alpine:3.20\n"

	t.Run("well-formed passes", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, good)
		if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte("apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: demo\nversion: 1.0.0\ndescription: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir); err != nil {
			t.Fatalf("ValidateStaged: %v", err)
		}
	})

	t.Run("missing recipe.yaml is refused", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, good) // no recipe.yaml
		if err := ValidateStaged(context.Background(), dir); err == nil ||
			!strings.Contains(err.Error(), "missing recipe.yaml") {
			t.Fatalf("err = %v, want missing recipe.yaml", err)
		}
	})

	t.Run("invalid policy is refused", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, "apiVersion: agent.vaka/v1alpha1\n") // no kind
		if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte("apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: demo\nversion: 1.0.0\ndescription: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir); err == nil ||
			!strings.Contains(err.Error(), "policy is invalid") {
			t.Fatalf("err = %v, want invalid policy", err)
		}
	})
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
