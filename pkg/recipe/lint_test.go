package recipe

import (
	"context"
	"fmt"
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
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
    volumes:
      - ./conf.yaml:/app/conf.yaml:ro
  gateway:
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
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
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
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
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
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
	goodManifest := "apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: demo\nversion: 1.0.0\ndescription: x\n"
	demo := ExpectedIdentity{Name: "demo", Version: "1.0.0"}

	t.Run("well-formed passes", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, good)
		if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte(goodManifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir, demo); err != nil {
			t.Fatalf("ValidateStaged: %v", err)
		}
	})

	t.Run("missing recipe.yaml is refused", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, good) // no recipe.yaml
		if err := ValidateStaged(context.Background(), dir, demo); err == nil ||
			!strings.Contains(err.Error(), "missing recipe.yaml") {
			t.Fatalf("err = %v, want missing recipe.yaml", err)
		}
	})

	t.Run("recipe.yaml that is a directory is refused", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, good)
		if err := os.Mkdir(filepath.Join(dir, "recipe.yaml"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir, demo); err == nil {
			t.Fatal("a directory named recipe.yaml passed validation")
		}
	})

	t.Run("garbage recipe.yaml is refused", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, good)
		if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte("just: some: bytes: ::\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir, demo); err == nil {
			t.Fatal("garbage recipe.yaml passed validation")
		}
	})

	t.Run("manifest schema violation is refused", func(t *testing.T) {
		// Reserved field present, wrong kind.
		dir := writeRecipeDir(t, compose, good)
		bad := "apiVersion: recipes.vaka/v1alpha1\nkind: Nope\nname: demo\nversion: 1.0.0\ndescription: x\nprovides: [a]\n"
		if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir, demo); err == nil ||
			!strings.Contains(err.Error(), "recipe.yaml is invalid") {
			t.Fatalf("err = %v, want manifest schema failure", err)
		}
	})

	t.Run("identity mismatch is refused", func(t *testing.T) {
		// The manifest is well-formed but names a different version than the
		// index resolved — the tarball does not match the entry.
		dir := writeRecipeDir(t, compose, good)
		if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"),
			[]byte("apiVersion: recipes.vaka/v1alpha1\nkind: Recipe\nname: demo\nversion: 2.0.0\ndescription: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir, demo); err == nil ||
			!strings.Contains(err.Error(), "identity mismatch") {
			t.Fatalf("err = %v, want identity mismatch", err)
		}
	})

	t.Run("invalid policy is refused", func(t *testing.T) {
		dir := writeRecipeDir(t, compose, "apiVersion: agent.vaka/v1alpha1\n") // no kind
		if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte(goodManifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateStaged(context.Background(), dir, demo); err == nil ||
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
		"risky:" + FlagUnpinnedImage,
		"unpoliced:" + FlagNoPolicyForSvc,
		"unpoliced:" + FlagUnpinnedImage,
	}
	if !reflect.DeepEqual(sum.RiskFlags, want) {
		t.Fatalf("flags:\ngot  %v\nwant %v", sum.RiskFlags, want)
	}
	if sum.DefaultActions["risky"] != "accept" || sum.DefaultActions["unpoliced"] != "none" {
		t.Fatalf("defaultActions = %v", sum.DefaultActions)
	}
}

func TestLintDirUnpinnedImage(t *testing.T) {
	dir := writeRecipeDir(t, `
services:
  mutable:
    image: nginx:latest
  taggged:
    image: nginx:1.27
  pinned:
    image: nginx:1.27@sha256:0000000000000000000000000000000000000000000000000000000000000000
  builtlocal:
    build: .
`, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  mutable:
    network: {egress: {defaultAction: reject}}
  taggged:
    network: {egress: {defaultAction: reject}}
  pinned:
    network: {egress: {defaultAction: reject}}
  builtlocal:
    network: {egress: {defaultAction: reject}}
`)
	sum, err := LintDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("LintDir: %v", err)
	}
	has := func(f string) bool {
		for _, g := range sum.RiskFlags {
			if g == f {
				return true
			}
		}
		return false
	}
	if !has("mutable:" + FlagUnpinnedImage) {
		t.Error("implicit-latest image not flagged")
	}
	if !has("taggged:" + FlagUnpinnedImage) {
		t.Error("mutable-tag image not flagged")
	}
	if has("pinned:" + FlagUnpinnedImage) {
		t.Error("digest-pinned image wrongly flagged")
	}
	if has("builtlocal:" + FlagUnpinnedImage) {
		t.Error("build-only service (no image) wrongly flagged")
	}
}

func TestLintDirAgainstLiveStyleRecipe(t *testing.T) {
	// The extractor fixture (goodRecipe) is not policy-complete; use a
	// codex-shaped pair: two services, one talking to the other only.
	dir := writeRecipeDir(t, `
services:
  agent:
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
    volumes:
      - ${WORKSPACE_DIR:-./}:/workspace
  gateway:
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
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

func TestCheckComposeReferencesRejectsExternalFiles(t *testing.T) {
	external := []string{
		"services:\n  app:\n    env_file: /etc/passwd\n",
		"services:\n  app:\n    env_file: [../../secret.env]\n",
		"services:\n  app:\n    env_file:\n      - path: /etc/shadow\n",
		"services:\n  app:\n    extends:\n      file: /etc/compose.yaml\n      service: base\n",
		"services:\n  app:\n    extends:\n      file: ../../other/compose.yaml\n      service: base\n",
		"include:\n  - ../../elsewhere/compose.yaml\n",
		"include:\n  - path: /abs/compose.yaml\n",
		"configs:\n  c:\n    file: /etc/shadow\n",
		"secrets:\n  s:\n    file: ../../../key.pem\n",
		// label_file is read by compose-go at load time, just like env_file.
		"services:\n  app:\n    label_file: /etc/passwd\n",
		"services:\n  app:\n    label_file: [../../labels.txt]\n",
		// include.project_directory redirects relative resolution and makes
		// compose-go load <project_directory>/.env.
		"include:\n  - path: sub.yaml\n    project_directory: /etc\n",
		"include:\n  - path: sub.yaml\n    project_directory: ../../elsewhere\n",
		// include long-form env_file.
		"include:\n  - path: sub.yaml\n    env_file: /etc/passwd\n",
		// Interpolated paths cannot be confined statically: with an empty env
		// the default resolves to an external absolute path at load time.
		"services:\n  app:\n    env_file: ${P:-/etc/passwd}\n",
		"services:\n  app:\n    env_file: ${HOME}/secret.env\n",
		"services:\n  app:\n    extends:\n      file: ${DIR}/compose.yaml\n      service: base\n",
	}
	for _, doc := range external {
		dir := t.TempDir()
		f := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(f, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkComposeReferences(dir, []string{f}); err == nil {
			t.Errorf("external reference not rejected:\n%s", doc)
		} else if !strings.Contains(err.Error(), "outside the recipe directory") {
			t.Errorf("unexpected error %v for:\n%s", err, doc)
		}
	}
}

func TestCheckComposeReferencesRecursesIntoIncludes(t *testing.T) {
	// An in-tree include/extends target is accepted at the top level, but its
	// OWN external references must still be caught — compose-go loads it too.
	t.Run("included file with external env_file", func(t *testing.T) {
		dir := t.TempDir()
		top := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(top, []byte("include:\n  - sub/inner.yaml\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		// The escaping ref is relative to sub/, so ../../ leaves the recipe.
		if err := os.WriteFile(filepath.Join(dir, "sub", "inner.yaml"),
			[]byte("services:\n  x:\n    env_file: ../../outside.env\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkComposeReferences(dir, []string{top}); err == nil ||
			!strings.Contains(err.Error(), "outside the recipe directory") {
			t.Fatalf("err = %v, want recursive external-ref refusal", err)
		}
	})

	t.Run("extends target with absolute config file", func(t *testing.T) {
		dir := t.TempDir()
		top := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(top,
			[]byte("services:\n  app:\n    extends:\n      file: base.yaml\n      service: b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "base.yaml"),
			[]byte("configs:\n  c:\n    file: /etc/shadow\nservices:\n  b:\n    image: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkComposeReferences(dir, []string{top}); err == nil ||
			!strings.Contains(err.Error(), "outside the recipe directory") {
			t.Fatalf("err = %v, want recursive external-ref refusal", err)
		}
	})

	t.Run("recursion terminates on an include cycle", func(t *testing.T) {
		dir := t.TempDir()
		a := filepath.Join(dir, "compose.yaml")
		b := filepath.Join(dir, "b.yaml")
		if err := os.WriteFile(a, []byte("include:\n  - b.yaml\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b, []byte("include:\n  - compose.yaml\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkComposeReferences(dir, []string{a}); err != nil {
			t.Fatalf("clean in-tree cycle rejected: %v", err)
		}
	})

	// #2: project_directory redirects where the included content resolves.
	// inner.yaml sits in sub/, but project_directory: . rebases its refs to the
	// recipe root, so env_file ../outside.env escapes even though it would look
	// in-tree relative to sub/.
	t.Run("project_directory redirect is caught", func(t *testing.T) {
		dir := t.TempDir()
		top := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(top,
			[]byte("include:\n  - path: sub/inner.yaml\n    project_directory: .\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Relative to the recipe root (project_directory: .), ../outside.env is
		// outside; relative to sub/ it would have looked in-tree.
		if err := os.WriteFile(filepath.Join(dir, "sub", "inner.yaml"),
			[]byte("services:\n  x:\n    env_file: ../outside.env\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkComposeReferences(dir, []string{top}); err == nil ||
			!strings.Contains(err.Error(), "outside the recipe directory") {
			t.Fatalf("err = %v, want project_directory-rebased external-ref refusal", err)
		}
	})

	t.Run("external project_directory is caught", func(t *testing.T) {
		dir := t.TempDir()
		top := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(top,
			[]byte("include:\n  - path: inner.yaml\n    project_directory: ../../etc\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "inner.yaml"),
			[]byte("services:\n  x:\n    image: y\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkComposeReferences(dir, []string{top}); err == nil ||
			!strings.Contains(err.Error(), "outside the recipe directory") {
			t.Fatalf("err = %v, want external project_directory refusal", err)
		}
	})

	// #1: a deep in-tree chain must not let an external reference in the final
	// file slip past a recursion limit (the old depth cap exited fail-open).
	t.Run("deep chain does not fail open at the end", func(t *testing.T) {
		dir := t.TempDir()
		const depth = 40
		for i := 0; i < depth; i++ {
			var body string
			if i < depth-1 {
				body = fmt.Sprintf("include:\n  - c%d.yaml\n", i+1)
			} else {
				// The last file, far past any small depth cap, escapes.
				body = "services:\n  x:\n    env_file: ../../outside.env\n"
			}
			name := "compose.yaml"
			if i > 0 {
				name = fmt.Sprintf("c%d.yaml", i)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := checkComposeReferences(dir, []string{filepath.Join(dir, "compose.yaml")}); err == nil ||
			!strings.Contains(err.Error(), "outside the recipe directory") {
			t.Fatalf("err = %v, want deep-chain external-ref refusal (no fail-open)", err)
		}
	})

	t.Run("deep clean chain is accepted", func(t *testing.T) {
		dir := t.TempDir()
		const depth = 40
		for i := 0; i < depth; i++ {
			var body string
			if i < depth-1 {
				body = fmt.Sprintf("include:\n  - c%d.yaml\n", i+1)
			} else {
				body = "services:\n  x:\n    image: y\n"
			}
			name := "compose.yaml"
			if i > 0 {
				name = fmt.Sprintf("c%d.yaml", i)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := checkComposeReferences(dir, []string{filepath.Join(dir, "compose.yaml")}); err != nil {
			t.Fatalf("clean deep chain rejected: %v", err)
		}
	})
}

func TestCheckComposeReferencesAllowsInTreeFiles(t *testing.T) {
	inTree := []string{
		"services:\n  app:\n    image: x\n",
		"services:\n  app:\n    env_file: ./app.env\n",
		"services:\n  app:\n    env_file: [conf/a.env, conf/b.env]\n",
		"services:\n  app:\n    env_file:\n      - path: conf/c.env\n",
		"services:\n  app:\n    label_file: ./labels.txt\n",
		"services:\n  app:\n    label_file: [conf/a.labels, conf/b.labels]\n",
		"services:\n  app:\n    extends:\n      file: base/compose.yaml\n      service: base\n",
		"services:\n  app:\n    extends:\n      service: sibling\n", // same-file, no path
		"include:\n  - sub/compose.yaml\n",
		"include:\n  - path: sub/compose.yaml\n    project_directory: sub\n",
		"include:\n  - path: sub/compose.yaml\n    env_file: sub/.env\n",
		"configs:\n  c:\n    file: ./conf.txt\n",
		"secrets:\n  s:\n    file: secrets/key.pem\n",
	}
	for _, doc := range inTree {
		dir := t.TempDir()
		f := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(f, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := checkComposeReferences(dir, []string{f}); err != nil {
			t.Errorf("in-tree reference rejected: %v\n%s", err, doc)
		}
	}
}

func TestLintDirRejectsExternalReference(t *testing.T) {
	// End-to-end: an external env_file must make LintDir fail BEFORE compose-go
	// reads the host file, not surface later as a risk flag.
	dir := writeRecipeDir(t, `
services:
  app:
    image: alpine:3.20@sha256:0000000000000000000000000000000000000000000000000000000000000000
    env_file: /etc/passwd
`, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`)
	_, err := LintDir(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "outside the recipe directory") {
		t.Fatalf("err = %v, want external-reference refusal", err)
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
