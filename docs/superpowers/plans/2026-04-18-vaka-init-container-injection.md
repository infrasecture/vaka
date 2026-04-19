# vaka-init Container Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically inject `vaka-init` and `nft` binaries into managed containers via a `__vaka-init` container, so users no longer need to bake them into their images.

**Architecture:** vaka intercepts compose commands along three paths. *Full path* (`up`, `run`, `create`): load vaka.yaml, build a policy override with modified entrypoint/caps/secrets plus `__vaka-init` container injection, pipe via `-f -`. *Lifecycle path* (`down`, `stop`, `kill`, `rm`): inject a minimal override declaring the `__vaka-init` container so compose includes it in teardown (passthrough when `--vaka-init-present`). *Passthrough*: everything else forwarded unchanged. A shared `execDockerCompose` helper conditionally adds `-f -` only when an override is present, making the passthrough path correct by construction. A `vakaVersion` field is injected into every generated policy YAML and validated by vaka-init before touching nftables.

**Tech Stack:** Go 1.25, `github.com/docker/docker/client`, `gopkg.in/yaml.v3`, Docker Compose v2, scratch container image.

---

## File map

| File | Action |
|---|---|
| `pkg/policy/types.go` | Add `VakaVersion string` to `ServicePolicy` |
| `pkg/policy/validate.go` | Rename `vaka.dev/v1alpha1` → `agent.vaka/v1alpha1`; error on user-supplied `vakaVersion` |
| `pkg/policy/validate_test.go` | Mass rename + new vakaVersion test |
| `pkg/policy/parse_test.go` | Mass rename |
| `pkg/policy/marshal_test.go` | Mass rename |
| `pkg/compose/override.go` | Add `OptOut bool` to `ServiceEntry`; update `BuildOverride` signature; add `__vaka-init` container; `volumes_from`/`depends_on`; path `→ /opt/vaka/sbin/vaka-init`; add `BuildVakaInitOnlyOverride` |
| `pkg/compose/override_test.go` | Update for new signature; add `__vaka-init` container/opt-out/path tests |
| `cmd/vaka/inject.go` | Add `--vaka-init-present` boolean flag extraction |
| `cmd/vaka/images.go` | New: `DockerServices` interface (`EnsureImage` + `ResolveEntrypoint`); `dockerClient` narrow interface for testability; `dockerServices` struct; `NewDockerServices()` wires real client once |
| `cmd/vaka/images_test.go` | New: `fakeDockerClient` stub; `dockerServices` unit tests for `EnsureImage` (present/absent/pull-fail) and `ResolveEntrypoint` (compose-declared/inspect/not-found) |
| `cmd/vaka/up.go` → `cmd/vaka/intercept.go` | Rename; `classifySubcmd`; `execDockerCompose` shared helper; `runFull` (was `runInjection`); `runLifecycle` (down/stop/kill/rm); `lifecycleOverrideYAML` helper; populate `VakaVersion`; label detection |
| `cmd/vaka/intercept_test.go` | New: `TestClassifySubcmd`; `TestLifecycleOverrideYAMLPassthrough`; `TestLifecycleOverrideYAMLInjectsContainer`; `TestExtractVakaFlagsBool` |
| `cmd/vaka/main.go` | Use `classifySubcmd` dispatch; add cobra stubs for `create`, `down`, `stop`, `kill`, `rm` |
| `cmd/vaka-init/main.go` | `nftBin` path; no-args exits 0; `checkVersion`; validate `vakaVersion` before proceeding; rename `vaka.dev/v1alpha1` |
| `cmd/vaka-init/main_test.go` | Rename apiVersion; add `checkVersion` tests |
| `docker/init/Dockerfile` | `COPY` paths → `/opt/vaka/sbin/`; add `VOLUME /opt/vaka` |
| `README.md` | Rename `apiVersion`; update paths; update opening claim; baked-in instructions |
| `docs/superpowers/specs/2026-04-14-vaka-secure-container-design.md` | Rename `apiVersion`; update paths |

**Test command (run after every task):**
```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```

---

### Task 1: apiVersion domain rename

**Files:**
- Modify: `pkg/policy/validate.go:70-71`
- Modify: `cmd/vaka-init/main.go:48-49`
- Modify: `pkg/policy/validate_test.go` (mass replace)
- Modify: `pkg/policy/parse_test.go` (mass replace)
- Modify: `pkg/policy/marshal_test.go` (mass replace)
- Modify: `cmd/vaka-init/main_test.go` (mass replace)

This is a pure mechanical rename. `vaka.dev/v1alpha1` → `agent.vaka/v1alpha1` everywhere in Go source. Docs are updated in Task 10.

- [ ] **Step 1: Update validate.go**

In `pkg/policy/validate.go` replace:
```go
	if p.APIVersion != "vaka.dev/v1alpha1" {
		add("apiVersion: must be \"vaka.dev/v1alpha1\", got %q", p.APIVersion)
	}
```
with:
```go
	if p.APIVersion != "agent.vaka/v1alpha1" {
		add("apiVersion: must be \"agent.vaka/v1alpha1\", got %q", p.APIVersion)
	}
```

- [ ] **Step 2: Update vaka-init/main.go**

In `cmd/vaka-init/main.go` replace:
```go
	if p.APIVersion != "vaka.dev/v1alpha1" {
		fatal("unsupported apiVersion: %s", p.APIVersion)
	}
```
with:
```go
	if p.APIVersion != "agent.vaka/v1alpha1" {
		fatal("unsupported apiVersion: %s", p.APIVersion)
	}
```

- [ ] **Step 3: Mass-replace in all test files**

Run (from repo root):
```bash
sed -i 's|vaka\.dev/v1alpha1|agent.vaka/v1alpha1|g' \
  pkg/policy/validate_test.go \
  pkg/policy/parse_test.go \
  pkg/policy/marshal_test.go \
  cmd/vaka-init/main_test.go
```

- [ ] **Step 4: Add explicit rejection test for old apiVersion**

Add to `pkg/policy/validate_test.go`:
```go
func TestValidateRejectsOldAPIVersion(t *testing.T) {
	p := mustParse(t, `
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for old apiVersion vaka.dev/v1alpha1, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "apiVersion") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning apiVersion, got: %v", errs)
	}
}
```

- [ ] **Step 5: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`.

- [ ] **Step 6: Commit**

```bash
git add pkg/policy/validate.go cmd/vaka-init/main.go \
        pkg/policy/validate_test.go pkg/policy/parse_test.go \
        pkg/policy/marshal_test.go cmd/vaka-init/main_test.go
git commit -m "refactor: rename apiVersion domain vaka.dev → agent.vaka"
```

---

### Task 2: vakaVersion in ServicePolicy types + validation

**Files:**
- Modify: `pkg/policy/types.go:13-17`
- Modify: `pkg/policy/validate.go` (add vakaVersion check)
- Modify: `pkg/policy/validate_test.go` (add test)

`vakaVersion` is a top-level field on `ServicePolicy`. It is always empty in the user's `vaka.yaml` — the CLI injects it before marshaling the generated policy. If a user accidentally writes it, validation must error.

- [ ] **Step 1: Write the failing test**

Add to `pkg/policy/validate_test.go`:
```go
func TestValidateVakaVersionForbiddenInUserYAML(t *testing.T) {
	p := mustParse(t, `
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
vakaVersion: v0.1.0
services:
  s: {}
`)
	errs := policy.Validate(p, nil)
	if len(errs) == 0 {
		t.Fatal("expected error for user-supplied vakaVersion, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "vakaVersion") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning vakaVersion, got: %v", errs)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/policy/... -run TestValidateVakaVersionForbiddenInUserYAML -v 2>&1
```
Expected: FAIL (field doesn't exist yet).

- [ ] **Step 3: Add VakaVersion to ServicePolicy**

In `pkg/policy/types.go` replace:
```go
type ServicePolicy struct {
	APIVersion string                    `yaml:"apiVersion"`
	Kind       string                    `yaml:"kind"`
	Services   map[string]*ServiceConfig `yaml:"services"`
}
```
with:
```go
type ServicePolicy struct {
	APIVersion  string                    `yaml:"apiVersion"`
	Kind        string                    `yaml:"kind"`
	VakaVersion string                    `yaml:"vakaVersion,omitempty"`
	Services    map[string]*ServiceConfig `yaml:"services"`
}
```

- [ ] **Step 4: Add validation rule**

In `pkg/policy/validate.go`, after the `Kind` check (after line 75), add:
```go
	if p.VakaVersion != "" {
		add("vakaVersion: must not be set in vaka.yaml (it is injected by the vaka CLI)")
	}
```

- [ ] **Step 5: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`.

- [ ] **Step 6: Commit**

```bash
git add pkg/policy/types.go pkg/policy/validate.go pkg/policy/validate_test.go
git commit -m "feat(policy): add VakaVersion field; reject user-supplied vakaVersion"
```

---

### Task 3: vaka-init — no-args exits 0, nftBin path

**Files:**
- Modify: `cmd/vaka-init/main.go:27,30-32`

Two independent changes: the `nftBin` constant path, and the no-args behavior (exits 0 so `service_completed_successfully` works when vaka-init is used as the `__vaka-init` container entrypoint with no arguments).

- [ ] **Step 1: Write the failing test for no-args exit-0 behavior**

Add to `cmd/vaka-init/main_test.go`:
```go
func TestNoArgExitsZero(t *testing.T) {
	// Subprocess trick: re-run this test binary as vaka-init with no "--".
	// When BE_VAKA_INIT=1 the subprocess calls main(); os.Args has no "--",
	// so main() hits fmt.Fprintln + os.Exit(0). Parent asserts exit code 0.
	if os.Getenv("BE_VAKA_INIT") == "1" {
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestNoArgExitsZero")
	cmd.Env = append(os.Environ(), "BE_VAKA_INIT=1")
	if err := cmd.Run(); err != nil {
		t.Errorf("vaka-init with no args: expected exit 0, got: %v", err)
	}
}
```

(Requires `"os/exec"` import in the test file.)

- [ ] **Step 2: Run test — expect FAIL**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./cmd/vaka-init/... -run TestNoArgExitsZero -v 2>&1
```
Expected: FAIL — no-args path still calls `fatal(...)`.

- [ ] **Step 3: Update nftBin constant**

In `cmd/vaka-init/main.go` replace:
```go
const nftBin = "/usr/local/sbin/nft"
```
with:
```go
const nftBin = "/opt/vaka/sbin/nft"
```

- [ ] **Step 4: Change no-args behavior to exit 0**

In `cmd/vaka-init/main.go` replace:
```go
func main() {
	if len(os.Args) < 2 || os.Args[1] != "--" {
		fatal("usage: vaka-init -- <entrypoint> [args...]")
	}
	harness := os.Args[2:]
	if len(harness) == 0 {
		fatal("vaka-init: no harness command after --")
	}
```
with:
```go
func main() {
	if len(os.Args) < 2 || os.Args[1] != "--" {
		fmt.Fprintln(os.Stderr, "vaka-init: usage: vaka-init -- <entrypoint> [args...]")
		os.Exit(0)
	}
	harness := os.Args[2:]
	if len(harness) == 0 {
		fatal("vaka-init: no harness command after --")
	}
```

- [ ] **Step 5: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`, including `TestNoArgExitsZero`.

- [ ] **Step 6: Commit**

```bash
git add cmd/vaka-init/main.go cmd/vaka-init/main_test.go
git commit -m "fix(vaka-init): update nftBin path to /opt/vaka/sbin/nft; exit 0 on no-args"
```

---

### Task 4: vaka-init — vakaVersion validation

**Files:**
- Modify: `cmd/vaka-init/main.go` (add `checkVersion`, call it early in `main`)
- Modify: `cmd/vaka-init/main_test.go` (add `TestCheckVersion`)

`checkVersion` compares the policy's `VakaVersion` against vaka-init's own embedded `version` variable. Semver major.minor must match; git hashes must match exactly.

- [ ] **Step 1: Write failing tests**

Add to `cmd/vaka-init/main_test.go`:
```go
func TestCheckVersion(t *testing.T) {
	tests := []struct {
		policy  string
		self    string
		wantErr bool
	}{
		{"v0.1.2", "v0.1.0", false},  // same major.minor, patch differs → ok
		{"v0.1.2", "v0.1.2", false},  // exact match → ok
		{"v0.1.0", "v0.2.0", true},   // minor mismatch → error
		{"v0.2.0", "v0.1.0", true},   // minor mismatch → error
		{"v1.0.0", "v0.1.0", true},   // major mismatch → error
		{"4178cc0", "4178cc0", false}, // git hash exact match → ok
		{"4178cc0", "4178cc0-dirty", true},  // git hash mismatch → error
		{"4178cc0-dirty", "4178cc0", true},  // git hash mismatch → error
		{"", "v0.1.0", true},          // missing → error
	}
	for _, tc := range tests {
		err := checkVersion(tc.policy, tc.self)
		if tc.wantErr && err == nil {
			t.Errorf("checkVersion(%q, %q): expected error, got nil", tc.policy, tc.self)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("checkVersion(%q, %q): unexpected error: %v", tc.policy, tc.self, err)
		}
	}
}
```

- [ ] **Step 2: Run test — expect FAIL (function doesn't exist)**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./cmd/vaka-init/... -run TestCheckVersion -v 2>&1
```
Expected: compile error — `checkVersion` undefined.

- [ ] **Step 3: Add checkVersion function**

Add to `cmd/vaka-init/main.go` (after the `fatal` function at end of file):
```go
// checkVersion validates that the policy's vakaVersion is compatible with this
// vaka-init binary. Semver (vX.Y.Z): major.minor must match, patch is free.
// Development builds (git hashes): must match exactly.
func checkVersion(policyVer, selfVer string) error {
	if policyVer == "" {
		return fmt.Errorf("vakaVersion: missing — policy was generated by an incompatible or unknown CLI version")
	}
	if policyVer == selfVer {
		return nil
	}
	pTrimmed := strings.TrimPrefix(policyVer, "v")
	sTrimmed := strings.TrimPrefix(selfVer, "v")
	pParts := strings.SplitN(pTrimmed, ".", 3)
	sParts := strings.SplitN(sTrimmed, ".", 3)
	if len(pParts) == 3 && len(sParts) == 3 {
		if pParts[0] == sParts[0] && pParts[1] == sParts[1] {
			return nil
		}
		return fmt.Errorf("vakaVersion: policy %s not compatible with vaka-init %s (major.minor must match)", policyVer, selfVer)
	}
	return fmt.Errorf("vakaVersion: policy %s does not match vaka-init %s (development builds must match exactly)", policyVer, selfVer)
}
```

- [ ] **Step 4: Call checkVersion in main, before apiVersion check**

In `cmd/vaka-init/main.go`, in the `main` function, after `p, err := readPolicy(secretPath)` and the `len(p.Services) != 1` check, add before the `apiVersion` check:
```go
	if err := checkVersion(p.VakaVersion, version); err != nil {
		fatal("%v", err)
	}
```

So the block reads:
```go
	p, err := readPolicy(secretPath)
	if err != nil {
		fatal("%v", err)
	}
	if len(p.Services) != 1 {
		fatal("policy must contain exactly one service, got %d", len(p.Services))
	}
	if err := checkVersion(p.VakaVersion, version); err != nil {
		fatal("%v", err)
	}
	if p.APIVersion != "agent.vaka/v1alpha1" {
		fatal("unsupported apiVersion: %s", p.APIVersion)
	}
```

- [ ] **Step 5: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`.

- [ ] **Step 6: Commit**

```bash
git add cmd/vaka-init/main.go cmd/vaka-init/main_test.go
git commit -m "feat(vaka-init): validate vakaVersion compatibility before applying rules"
```

---

### Task 5: Override generator — vaka-init container, paths, opt-out

**Files:**
- Modify: `pkg/compose/override.go` (full rewrite of structs + BuildOverride; add BuildVakaInitOnlyOverride)
- Modify: `pkg/compose/override_test.go` (update existing tests; add new tests)

`BuildOverride` gains an `imageRef string` parameter. When non-empty and at least one service has `OptOut: false`, a `__vaka-init` container is added and managed services get `volumes_from` + `depends_on`. The entrypoint always changes from `"vaka-init"` to `"/opt/vaka/sbin/vaka-init"`.

- [ ] **Step 1: Write failing tests**

Replace the entire content of `pkg/compose/override_test.go` with:

```go
// pkg/compose/override_test.go
package compose_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
)

type overrideDoc struct {
	Secrets  map[string]struct {
		Environment string `yaml:"environment"`
	} `yaml:"secrets"`
	Services map[string]struct {
		Image       string   `yaml:"image"`
		Entrypoint  []string `yaml:"entrypoint"`
		Command     []string `yaml:"command"`
		CapAdd      []string `yaml:"cap_add"`
		Restart     string   `yaml:"restart"`
		VolumesFrom []string `yaml:"volumes_from"`
		DependsOn   map[string]struct {
			Condition string `yaml:"condition"`
		} `yaml:"depends_on"`
		Secrets []struct {
			Source string `yaml:"source"`
			Target string `yaml:"target"`
		} `yaml:"secrets"`
	} `yaml:"services"`
}

func parseOverride(t *testing.T, yaml_str string) overrideDoc {
	t.Helper()
	var doc overrideDoc
	if err := yaml.Unmarshal([]byte(yaml_str), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

const testImage = "emsi/vaka-init:v0.1.0"

func singleEntry(name string) []compose.ServiceEntry {
	return []compose.ServiceEntry{{
		Name:       name,
		Entrypoint: []string{"claude"},
		Command:    []string{"--dangerously-skip-permissions"},
		CapDelta:   []string{"NET_ADMIN"},
		EnvVarName: "VAKA_CODEX_CONF",
	}}
}

func TestOverrideSecretNameDerivedFromService(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	if _, ok := doc.Secrets["vaka_codex_conf"]; !ok {
		t.Errorf("expected secret key 'vaka_codex_conf'; got secrets: %+v", doc.Secrets)
	}
	if doc.Secrets["vaka_codex_conf"].Environment != "VAKA_CODEX_CONF" {
		t.Errorf("secret env = %q, want VAKA_CODEX_CONF", doc.Secrets["vaka_codex_conf"].Environment)
	}
}

func TestOverrideEntrypointIsVakaInitAbsPath(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]
	if len(svc.Entrypoint) < 2 || svc.Entrypoint[0] != "/opt/vaka/sbin/vaka-init" || svc.Entrypoint[1] != "--" {
		t.Errorf("entrypoint = %v, want [/opt/vaka/sbin/vaka-init --]", svc.Entrypoint)
	}
}

func TestOverrideCommandIsOriginalEntrypoint(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]
	if len(svc.Command) == 0 || svc.Command[0] != "claude" {
		t.Errorf("command = %v, want [claude --dangerously-skip-permissions]", svc.Command)
	}
}

func TestOverrideCapAddContainsDelta(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	for _, cap := range doc.Services["codex"].CapAdd {
		if cap == "NET_ADMIN" {
			return
		}
	}
	t.Errorf("cap_add does not contain NET_ADMIN; got %v", doc.Services["codex"].CapAdd)
}

func TestOverrideSecretMountTargetIsVakaYaml(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	secrets := doc.Services["codex"].Secrets
	if len(secrets) == 0 {
		t.Fatal("no secrets in service override")
	}
	if secrets[0].Target != "vaka.yaml" {
		t.Errorf("secret target = %q, want vaka.yaml", secrets[0].Target)
	}
}

func TestOverrideHyphensInServiceNameBecomesUnderscores(t *testing.T) {
	entries := []compose.ServiceEntry{{
		Name:       "llm-gateway",
		Entrypoint: []string{"/usr/local/bin/litellm"},
		EnvVarName: "VAKA_LLM_GATEWAY_CONF",
	}}
	out, _ := compose.BuildOverride(entries, testImage)
	if !strings.Contains(out, "vaka_llm_gateway_conf") {
		t.Errorf("expected secret key with underscores; got:\n%s", out)
	}
}

func TestOverrideVakaInitContainerEmitted(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	container, ok := doc.Services["__vaka-init"]
	if !ok {
		t.Fatalf("__vaka-init service not in override:\n%s", out)
	}
	if container.Image != testImage {
		t.Errorf("__vaka-init image = %q, want %q", container.Image, testImage)
	}
	if len(container.Entrypoint) != 1 || container.Entrypoint[0] != "/opt/vaka/sbin/vaka-init" {
		t.Errorf("__vaka-init entrypoint = %v, want [/opt/vaka/sbin/vaka-init]", container.Entrypoint)
	}
	if container.Restart != "no" {
		t.Errorf("__vaka-init restart = %q, want no", container.Restart)
	}
}

func TestOverrideServiceGetsVolumesFromAndDependsOn(t *testing.T) {
	out, _ := compose.BuildOverride(singleEntry("codex"), testImage)
	doc := parseOverride(t, out)
	svc := doc.Services["codex"]
	if len(svc.VolumesFrom) != 1 || svc.VolumesFrom[0] != "__vaka-init:ro" {
		t.Errorf("volumes_from = %v, want [__vaka-init:ro]", svc.VolumesFrom)
	}
	dep, ok := svc.DependsOn["__vaka-init"]
	if !ok {
		t.Errorf("depends_on missing __vaka-init; got %+v", svc.DependsOn)
	}
	if dep.Condition != "service_completed_successfully" {
		t.Errorf("depends_on condition = %q, want service_completed_successfully", dep.Condition)
	}
}

func TestOverrideNoVakaInitContainerWhenImageEmpty(t *testing.T) {
	out, err := compose.BuildOverride(singleEntry("codex"), "")
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	if _, ok := doc.Services["__vaka-init"]; ok {
		t.Errorf("__vaka-init must not be emitted when imageRef is empty:\n%s", out)
	}
	svc := doc.Services["codex"]
	if len(svc.VolumesFrom) != 0 {
		t.Errorf("volumes_from must be empty when imageRef is empty, got %v", svc.VolumesFrom)
	}
}

func TestOverridePerServiceOptOut(t *testing.T) {
	entries := []compose.ServiceEntry{
		{Name: "svc-a", Entrypoint: []string{"a"}, EnvVarName: "VAKA_SVC_A_CONF", OptOut: false},
		{Name: "svc-b", Entrypoint: []string{"b"}, EnvVarName: "VAKA_SVC_B_CONF", OptOut: true},
	}
	out, err := compose.BuildOverride(entries, testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	// __vaka-init container still emitted because svc-a needs it.
	if _, ok := doc.Services["__vaka-init"]; !ok {
		t.Errorf("__vaka-init must be emitted when at least one service needs injection:\n%s", out)
	}
	// svc-a gets volumes_from.
	if len(doc.Services["svc-a"].VolumesFrom) == 0 {
		t.Errorf("svc-a must have volumes_from")
	}
	// svc-b does NOT get volumes_from.
	if len(doc.Services["svc-b"].VolumesFrom) != 0 {
		t.Errorf("svc-b must not have volumes_from when OptOut=true, got %v", doc.Services["svc-b"].VolumesFrom)
	}
}

func TestOverrideAllOptOutNoVakaInitContainer(t *testing.T) {
	entries := []compose.ServiceEntry{
		{Name: "svc-a", Entrypoint: []string{"a"}, EnvVarName: "VAKA_SVC_A_CONF", OptOut: true},
		{Name: "svc-b", Entrypoint: []string{"b"}, EnvVarName: "VAKA_SVC_B_CONF", OptOut: true},
	}
	out, err := compose.BuildOverride(entries, testImage)
	if err != nil {
		t.Fatalf("BuildOverride: %v", err)
	}
	doc := parseOverride(t, out)
	if _, ok := doc.Services["__vaka-init"]; ok {
		t.Errorf("__vaka-init must not be emitted when all services opt out:\n%s", out)
	}
}

func TestBuildVakaInitOnlyOverride(t *testing.T) {
	out, err := compose.BuildVakaInitOnlyOverride(testImage)
	if err != nil {
		t.Fatalf("BuildVakaInitOnlyOverride: %v", err)
	}
	doc := parseOverride(t, out)
	container, ok := doc.Services["__vaka-init"]
	if !ok {
		t.Fatalf("__vaka-init not in vaka-init-only override:\n%s", out)
	}
	if container.Image != testImage {
		t.Errorf("image = %q, want %q", container.Image, testImage)
	}
	// Must not contain any other services or secrets.
	if len(doc.Secrets) != 0 {
		t.Errorf("vaka-init-only override must have no secrets, got %+v", doc.Secrets)
	}
	if len(doc.Services) != 1 {
		t.Errorf("vaka-init-only override must have exactly 1 service, got %d", len(doc.Services))
	}
}
```

- [ ] **Step 2: Run tests — expect FAIL**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/compose/... -v 2>&1
```
Expected: compile errors (wrong signature, missing fields).

- [ ] **Step 3: Rewrite override.go**

Replace the entire content of `pkg/compose/override.go` with:

```go
// pkg/compose/override.go
package compose

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const vakaInitServiceName = "__vaka-init"
const vakaInitPath = "/opt/vaka/sbin/vaka-init"

// ServiceEntry holds per-service data needed to build the compose override.
type ServiceEntry struct {
	Name       string
	Entrypoint []string
	Command    []string
	CapDelta   []string
	EnvVarName string
	// OptOut is true when the service carries the agent.vaka.init: present label,
	// meaning vaka-init is already baked into the image at /opt/vaka/sbin/.
	OptOut bool
}

// secretKey returns the compose secret key for a service name.
// "llm-gateway" → "vaka_llm_gateway_conf"
func secretKey(serviceName string) string {
	return "vaka_" + strings.ReplaceAll(strings.ToLower(serviceName), "-", "_") + "_conf"
}

type composeOverride struct {
	Secrets  map[string]secretDef       `yaml:"secrets,omitempty"`
	Services map[string]serviceOverride `yaml:"services,omitempty"`
}

type secretDef struct {
	Environment string `yaml:"environment"`
}

type serviceOverride struct {
	Image       string             `yaml:"image,omitempty"`
	Entrypoint  []string           `yaml:"entrypoint,omitempty"`
	Command     []string           `yaml:"command,omitempty"`
	CapAdd      []string           `yaml:"cap_add,omitempty"`
	Secrets     []secretMount      `yaml:"secrets,omitempty"`
	VolumesFrom []string           `yaml:"volumes_from,omitempty"`
	DependsOn   map[string]depCond `yaml:"depends_on,omitempty"`
	Restart     string             `yaml:"restart,omitempty"`
}

type secretMount struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

type depCond struct {
	Condition string `yaml:"condition"`
}

// BuildOverride constructs the compose override YAML string from entries.
// imageRef is the fully-qualified image reference for the __vaka-init container
// (e.g. "emsi/vaka-init:v0.1.2"). Pass "" to disable injection globally
// (--vaka-init-present flag).
func BuildOverride(entries []ServiceEntry, imageRef string) (string, error) {
	override := composeOverride{
		Secrets:  make(map[string]secretDef),
		Services: make(map[string]serviceOverride),
	}

	injectVakaInit := imageRef != "" && anyNeedsInjection(entries)
	if injectVakaInit {
		override.Services[vakaInitServiceName] = serviceOverride{
			Image:      imageRef,
			Entrypoint: []string{vakaInitPath},
			Restart:    "no",
		}
	}

	for _, e := range entries {
		key := secretKey(e.Name)
		override.Secrets[key] = secretDef{Environment: e.EnvVarName}

		cmd := make([]string, 0, len(e.Entrypoint)+len(e.Command))
		cmd = append(cmd, e.Entrypoint...)
		cmd = append(cmd, e.Command...)

		svc := serviceOverride{
			Entrypoint: []string{vakaInitPath, "--"},
			Command:    cmd,
			CapAdd:     e.CapDelta,
			Secrets:    []secretMount{{Source: key, Target: "vaka.yaml"}},
		}

		if injectVakaInit && !e.OptOut {
			svc.VolumesFrom = []string{vakaInitServiceName + ":ro"}
			svc.DependsOn = map[string]depCond{
				vakaInitServiceName: {Condition: "service_completed_successfully"},
			}
		}

		override.Services[e.Name] = svc
	}

	data, err := yaml.Marshal(override)
	if err != nil {
		return "", fmt.Errorf("marshal compose override: %w", err)
	}
	return string(data), nil
}

// BuildVakaInitOnlyOverride returns a minimal compose override YAML containing
// only the __vaka-init service definition. Used by vaka down to include the
// __vaka-init container in teardown even though the full policy override is not re-generated.
func BuildVakaInitOnlyOverride(imageRef string) (string, error) {
	override := composeOverride{
		Services: map[string]serviceOverride{
			vakaInitServiceName: {
				Image:      imageRef,
				Entrypoint: []string{vakaInitPath},
				Restart:    "no",
			},
		},
	}
	data, err := yaml.Marshal(override)
	if err != nil {
		return "", fmt.Errorf("marshal vaka-init container override: %w", err)
	}
	return string(data), nil
}

func anyNeedsInjection(entries []ServiceEntry) bool {
	for _, e := range entries {
		if !e.OptOut {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`. (Note: `cmd/vaka` will fail to compile because `BuildOverride` call in `up.go` has wrong signature — fix in Task 7.)

If `cmd/vaka` compile fails, verify only that `pkg/...` passes for now:
```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... 2>&1
```

- [ ] **Step 5: Commit**

```bash
git add pkg/compose/override.go pkg/compose/override_test.go
git commit -m "feat(compose): vaka-init container injection in BuildOverride; add BuildVakaInitOnlyOverride"
```

---

### Task 6: Docker services interface

**Files:**
- Create: `cmd/vaka/images.go`
- Create: `cmd/vaka/images_test.go`

All Docker daemon interactions — image inspection for entrypoint resolution and image check/pull for `__vaka-init` — live in `images.go` behind a single `DockerServices` interface. `runFull` calls `NewDockerServices()` once; a single `*client.Client` is used for both operations. `dockerClient` is a narrow internal interface enabling test doubles without a live daemon; `*client.Client` satisfies it automatically.

- [ ] **Step 1: Create cmd/vaka/images.go**

```go
// cmd/vaka/images.go
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// DockerServices is the interface for all Docker daemon interactions in vaka.
// A single implementation is created per runFull invocation; a test double can
// replace it entirely.
type DockerServices interface {
	// EnsureImage inspects ref locally and pulls it if absent.
	EnsureImage(ctx context.Context, ref string) error
	// ResolveEntrypoint returns the effective entrypoint and command for a
	// compose service. If the service declares either field they are returned
	// directly; otherwise the image is inspected to obtain defaults.
	ResolveEntrypoint(ctx context.Context, svcName string, svc composetypes.ServiceConfig) ([]string, []string, error)
}

// dockerClient is a narrow interface over the Docker API operations used by
// dockerServices. *client.Client satisfies it; tests inject a stub.
type dockerClient interface {
	ImageInspect(ctx context.Context, ref string) (dockerimage.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, opts dockerimage.PullOptions) (io.ReadCloser, error)
}

// dockerServices is the production DockerServices backed by the Docker daemon.
type dockerServices struct {
	c dockerClient
}

// NewDockerServices creates a DockerServices using the Docker environment
// (DOCKER_HOST, TLS settings, active context). The underlying client is
// created once and reused for all operations. Close is intentionally omitted:
// this is a short-lived CLI process that exits immediately after the operation,
// so the OS reclaims all resources without an explicit teardown.
func NewDockerServices() (DockerServices, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &dockerServices{c: c}, nil
}

// EnsureImage inspects ref locally; pulls it if absent.
func (d *dockerServices) EnsureImage(ctx context.Context, ref string) error {
	_, err := d.c.ImageInspect(ctx, ref)
	if err == nil {
		return nil
	}
	if !errdefs.IsNotFound(err) {
		return fmt.Errorf("inspect %s: %w", ref, err)
	}
	rc, pullErr := d.c.ImagePull(ctx, ref, dockerimage.PullOptions{})
	if pullErr != nil {
		return fmt.Errorf("failed to pull %s — check network connectivity or use --vaka-init-present if binaries are baked into the image: %w", ref, pullErr)
	}
	defer rc.Close()
	_, err = io.Copy(os.Stderr, rc)
	return err
}

// ResolveEntrypoint returns the effective entrypoint and command for svc.
// If the compose service declares either field they are returned directly;
// otherwise the image is inspected to obtain the Dockerfile defaults.
func (d *dockerServices) ResolveEntrypoint(ctx context.Context, svcName string, svc composetypes.ServiceConfig) ([]string, []string, error) {
	if len(svc.Entrypoint) > 0 || len(svc.Command) > 0 {
		return svc.Entrypoint, svc.Command, nil
	}
	if svc.Image == "" {
		return nil, nil, fmt.Errorf("service %s: no image and no entrypoint/command declared", svcName)
	}
	inspect, err := d.c.ImageInspect(ctx, svc.Image)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil, fmt.Errorf(
				"service %s: image %q not available locally and no entrypoint/command declared — pull first or add entrypoint:",
				svcName, svc.Image)
		}
		return nil, nil, fmt.Errorf("service %s: inspect %q: %w", svcName, svc.Image, err)
	}
	if inspect.Config == nil {
		return nil, nil, fmt.Errorf("service %s: image %q has no Config", svcName, svc.Image)
	}
	return inspect.Config.Entrypoint, inspect.Config.Cmd, nil
}
```

- [ ] **Step 2: Create cmd/vaka/images_test.go with fakes and unit tests**

`fakeDockerClient` implements `dockerClient` and controls both inspect and pull behaviour. `inspectResult` is returned when `notFound == false`; leave it zero-value for `EnsureImage` tests (only the absence/presence matters) and populate `Config` for `ResolveEntrypoint` tests. Tests construct `dockerServices{c: dc}` directly — exercising the real logic without a live daemon.

```go
// cmd/vaka/images_test.go
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	containertypes "github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// fakeDockerClient implements dockerClient for unit tests without a live daemon.
type fakeDockerClient struct {
	notFound      bool                      // ImageInspect returns NotFound when true
	inspectResult dockerimage.InspectResponse // returned when notFound == false
	pullErr       error                     // error to return from ImagePull; nil = success
	pullCalled    bool
}

func (f *fakeDockerClient) ImageInspect(_ context.Context, _ string) (dockerimage.InspectResponse, error) {
	if f.notFound {
		return dockerimage.InspectResponse{}, errdefs.NotFound(errors.New("not found"))
	}
	return f.inspectResult, nil
}

func (f *fakeDockerClient) ImagePull(_ context.Context, _ string, _ dockerimage.PullOptions) (io.ReadCloser, error) {
	f.pullCalled = true
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return io.NopCloser(&bytes.Buffer{}), nil
}

// --- EnsureImage tests ---

func TestEnsureImagePresent(t *testing.T) {
	dc := &fakeDockerClient{notFound: false}
	e := &dockerServices{c: dc}
	if err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0"); err != nil {
		t.Fatalf("present: unexpected error: %v", err)
	}
	if dc.pullCalled {
		t.Error("present: pull must not be called when image is already present")
	}
}

func TestEnsureImageAbsentPullSucceeds(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	e := &dockerServices{c: dc}
	if err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0"); err != nil {
		t.Fatalf("absent+pull succeeds: unexpected error: %v", err)
	}
	if !dc.pullCalled {
		t.Error("absent+pull succeeds: pull must be called when image is absent")
	}
}

func TestEnsureImageAbsentPullFails(t *testing.T) {
	pullErr := errors.New("network unreachable")
	dc := &fakeDockerClient{notFound: true, pullErr: pullErr}
	e := &dockerServices{c: dc}
	err := e.EnsureImage(context.Background(), "emsi/vaka-init:v0.1.0")
	if err == nil {
		t.Fatal("pull fails: expected error, got nil")
	}
	if !errors.Is(err, pullErr) {
		t.Errorf("pull fails: expected %v wrapped, got %v", pullErr, err)
	}
}

// --- ResolveEntrypoint tests ---

func TestResolveEntrypointFromCompose(t *testing.T) {
	// Entrypoint declared in compose — no image inspect needed.
	dc := &fakeDockerClient{}
	ds := &dockerServices{c: dc}
	svc := composetypes.ServiceConfig{
		Entrypoint: []string{"/app"},
		Command:    []string{"--flag"},
	}
	ep, cmd, err := ds.ResolveEntrypoint(context.Background(), "myapp", svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ep) != 1 || ep[0] != "/app" {
		t.Errorf("entrypoint = %v, want [/app]", ep)
	}
	if len(cmd) != 1 || cmd[0] != "--flag" {
		t.Errorf("command = %v, want [--flag]", cmd)
	}
}

func TestResolveEntrypointFromInspect(t *testing.T) {
	// No compose entrypoint — should inspect image and use Dockerfile defaults.
	dc := &fakeDockerClient{
		inspectResult: dockerimage.InspectResponse{
			Config: &containertypes.Config{
				Entrypoint: []string{"/docker-entrypoint.sh"},
				Cmd:        []string{"nginx", "-g", "daemon off;"},
			},
		},
	}
	ds := &dockerServices{c: dc}
	svc := composetypes.ServiceConfig{Image: "nginx:latest"}
	ep, cmd, err := ds.ResolveEntrypoint(context.Background(), "web", svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ep) != 1 || ep[0] != "/docker-entrypoint.sh" {
		t.Errorf("entrypoint = %v, want [/docker-entrypoint.sh]", ep)
	}
	if len(cmd) != 3 || cmd[0] != "nginx" {
		t.Errorf("command = %v, want [nginx -g daemon off;]", cmd)
	}
}

func TestResolveEntrypointImageNotFound(t *testing.T) {
	dc := &fakeDockerClient{notFound: true}
	ds := &dockerServices{c: dc}
	svc := composetypes.ServiceConfig{Image: "myapp:latest"}
	_, _, err := ds.ResolveEntrypoint(context.Background(), "myapp", svc)
	if err == nil {
		t.Fatal("expected error for missing image, got nil")
	}
}
```

- [ ] **Step 3: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```

- [ ] **Step 4: Commit**

```bash
git add cmd/vaka/images.go cmd/vaka/images_test.go
git commit -m "feat(vaka): add DockerServices interface consolidating image check/pull and entrypoint resolution"
```

---

### Task 7: intercept.go — rename, three-path dispatch, shared execDockerCompose

**Files:**
- Rename: `cmd/vaka/up.go` → `cmd/vaka/intercept.go`
- Modify: `cmd/vaka/inject.go` (add boolean flag support)
- Create: `cmd/vaka/intercept_test.go`
- Modify: `cmd/vaka/main.go` (classifySubcmd dispatch; cobra stubs for all intercepted commands)

Three compose intercept paths share a single `execDockerCompose` execution helper. The helper conditionally adds `-f -` only when an override is non-empty, so the passthrough path is correct by construction — it never injects `-f -` when no override YAML was produced.

**Pre-existing helpers from `up.go`:** The rename in Step 1 carries over `allFileFlags`, `discoverComposeFiles`, `injectStdinOverride`, `loadAndValidate`, and `findSubcmd` unchanged. These are already implemented in the current `up.go`; do not rewrite them.

- [ ] **Step 1: Rename up.go → intercept.go**

```bash
git mv cmd/vaka/up.go cmd/vaka/intercept.go
```

Update the package comment at line 1:
```go
// cmd/vaka/intercept.go
```

- [ ] **Step 2: Add boolean flag support to inject.go**

In `cmd/vaka/inject.go`, add after `vakaFlagsTakingValue`:
```go
// vakaFlagsBool lists --vaka-* boolean flags (no value token consumed).
var vakaFlagsBool = map[string]bool{
	"--vaka-init-present": true,
}
```

Update `extractVakaFlags` to handle boolean flags:
```go
func extractVakaFlags(argv []string) (flags map[string]string, rest []string) {
	flags = make(map[string]string)
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if vakaFlagsTakingValue[arg] {
			if i+1 < len(argv) {
				flags[arg] = argv[i+1]
				i++
			}
			continue
		}
		if vakaFlagsBool[arg] {
			flags[arg] = "true"
			continue
		}
		rest = append(rest, arg)
	}
	return flags, rest
}
```

- [ ] **Step 3: Write failing tests**

Create `cmd/vaka/intercept_test.go`:

```go
// cmd/vaka/intercept_test.go
package main

import (
	"strings"
	"testing"
)

func TestClassifySubcmd(t *testing.T) {
	tests := []struct {
		subcmd string
		want   subcmdPath
	}{
		{"up", pathFull},
		{"run", pathFull},
		{"create", pathFull},
		{"down", pathLifecycle},
		{"stop", pathLifecycle},
		{"kill", pathLifecycle},
		{"rm", pathLifecycle},
		{"validate", pathCobra},
		{"show", pathCobra},
		{"version", pathCobra},
		{"", pathCobra},
		{"logs", pathPassthrough},
		{"ps", pathPassthrough},
		{"exec", pathPassthrough},
		{"pull", pathPassthrough},
	}
	for _, tc := range tests {
		if got := classifySubcmd(tc.subcmd); got != tc.want {
			t.Errorf("classifySubcmd(%q) = %v, want %v", tc.subcmd, got, tc.want)
		}
	}
}

func TestLifecycleOverrideYAMLPassthrough(t *testing.T) {
	yaml, err := lifecycleOverrideYAML(true, "emsi/vaka-init:v0.1.0")
	if err != nil {
		t.Fatalf("passthrough: unexpected error: %v", err)
	}
	if yaml != "" {
		t.Errorf("passthrough: expected empty string, got:\n%s", yaml)
	}
}

func TestLifecycleOverrideYAMLInjectsContainer(t *testing.T) {
	yaml, err := lifecycleOverrideYAML(false, "emsi/vaka-init:v0.1.0")
	if err != nil {
		t.Fatalf("injection: unexpected error: %v", err)
	}
	if !strings.Contains(yaml, "__vaka-init") {
		t.Errorf("injection: expected __vaka-init in YAML, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "emsi/vaka-init:v0.1.0") {
		t.Errorf("injection: expected image ref in YAML, got:\n%s", yaml)
	}
}

func TestExtractVakaFlagsBool(t *testing.T) {
	// --vaka-init-present is a boolean flag: no value token consumed.
	flags, rest := extractVakaFlags([]string{"up", "--vaka-init-present", "--remove-orphans"})
	if flags["--vaka-init-present"] != "true" {
		t.Errorf("expected --vaka-init-present=true, got %q", flags["--vaka-init-present"])
	}
	want := []string{"up", "--remove-orphans"}
	if strings.Join(rest, " ") != strings.Join(want, " ") {
		t.Errorf("rest = %v, want %v", rest, want)
	}
}
```

Run — expected FAIL (compile error: `subcmdPath`, `classifySubcmd`, `lifecycleOverrideYAML` undefined):

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./cmd/vaka/... -v 2>&1
```

- [ ] **Step 4: Replace intercept.go with new architecture**

Replace the full content of `cmd/vaka/intercept.go` with:

```go
// cmd/vaka/intercept.go
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"
	"vaka.dev/vaka/pkg/compose"
	"vaka.dev/vaka/pkg/policy"
)

const vakaInitLabel = "agent.vaka.init"
const vakaInitBaseImage = "emsi/vaka-init"

var defaultDockerCaps = map[string]bool{
	"CAP_CHOWN": true, "CAP_DAC_OVERRIDE": true, "CAP_FOWNER": true,
	"CAP_FSETID": true, "CAP_KILL": true, "CAP_SETGID": true,
	"CAP_SETUID": true, "CAP_SETPCAP": true, "CAP_NET_BIND_SERVICE": true,
	"CAP_NET_RAW": true, "CAP_SYS_CHROOT": true, "CAP_MKNOD": true,
	"CAP_AUDIT_WRITE": true, "CAP_SETFCAP": true,
}

// subcmdPath classifies how a compose subcommand is handled.
type subcmdPath int

const (
	pathCobra       subcmdPath = iota // validate, show, version — cobra commands
	pathFull                          // up, run, create — full policy + __vaka-init injection
	pathLifecycle                     // down, stop, kill, rm — __vaka-init container only
	pathPassthrough                   // all others — forwarded unchanged to docker compose
)

func classifySubcmd(subcmd string) subcmdPath {
	switch subcmd {
	case "validate", "show", "version", "":
		return pathCobra
	case "up", "run", "create":
		return pathFull
	case "down", "stop", "kill", "rm":
		return pathLifecycle
	default:
		return pathPassthrough
	}
}

// execDockerCompose executes docker compose with the given args.
// When overrideYAML is non-empty it is injected via -f - (with default compose
// files also passed via -f so compose merges them correctly).
// extraEnv, when non-nil, is appended to the inherited environment.
func execDockerCompose(args []string, overrideYAML string, extraEnv []string) error {
	var dockerArgs []string
	if overrideYAML != "" {
		defaults := []string{}
		if len(allFileFlags(args)) == 0 {
			defaults = discoverComposeFiles(".")
		}
		dockerArgs = injectStdinOverride(append([]string{"compose"}, args...), defaults)
	} else {
		dockerArgs = append([]string{"compose"}, args...)
	}
	c := exec.Command("docker", dockerArgs...)
	if overrideYAML != "" {
		c.Stdin = strings.NewReader(overrideYAML)
	} else {
		c.Stdin = os.Stdin
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if extraEnv != nil {
		c.Env = append(os.Environ(), extraEnv...)
	}
	return c.Run()
}

// runFull handles full-override commands: up, run, create.
// It loads and validates vaka.yaml, ensures the __vaka-init image when needed,
// builds the full compose override, and delegates to execDockerCompose.
func runFull(vakaFile string, args []string, vakaInitPresent bool) error {
	composeFiles := allFileFlags(args)
	if len(composeFiles) == 0 {
		composeFiles = discoverComposeFiles(".")
		if len(composeFiles) == 0 {
			return fmt.Errorf("no compose configuration file found in current directory")
		}
	}

	p, project, err := loadAndValidate(vakaFile, composeFiles)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Create ONE Docker client for both entrypoint resolution and image ensuring.
	ds, err := NewDockerServices()
	if err != nil {
		return err
	}

	var entries []compose.ServiceEntry
	var extraEnv []string

	for svcName, svc := range p.Services {
		composeSvc, ok := project.Services[svcName]
		if !ok {
			return fmt.Errorf("service %q: not found in compose files %v", svcName, composeFiles)
		}

		entrypoint, cmd, err := ds.ResolveEntrypoint(ctx, svcName, composeSvc)
		if err != nil {
			return err
		}

		delta := computeCapDelta(composeSvc)
		if svc.Runtime == nil {
			svc.Runtime = &policy.RuntimeConfig{}
		}
		if len(svc.Runtime.DropCaps) == 0 {
			svc.Runtime.DropCaps = delta
		}
		fmt.Fprintf(os.Stderr, "vaka: service %s: dropCaps: %v\n", svcName, svc.Runtime.DropCaps)

		sliced, err := policy.SliceService(p, svcName)
		if err != nil {
			return err
		}
		sliced.VakaVersion = version

		raw, err := yaml.Marshal(sliced)
		if err != nil {
			return fmt.Errorf("marshal policy for %s: %w", svcName, err)
		}

		envKey := "VAKA_" + strings.ToUpper(strings.ReplaceAll(svcName, "-", "_")) + "_CONF"
		extraEnv = append(extraEnv, envKey+"="+base64.StdEncoding.EncodeToString(raw))

		entries = append(entries, compose.ServiceEntry{
			Name:       svcName,
			Entrypoint: entrypoint,
			Command:    cmd,
			CapDelta:   delta,
			EnvVarName: envKey,
			OptOut:     composeSvc.Labels[vakaInitLabel] == "present",
		})
	}

	// Pull __vaka-init image only when injection is actually needed.
	needsInjection := false
	for _, e := range entries {
		if !e.OptOut {
			needsInjection = true
			break
		}
	}
	vakaInitImageRef := ""
	if !vakaInitPresent && needsInjection {
		vakaInitImageRef = vakaInitBaseImage + ":" + version
		if err := ds.EnsureImage(ctx, vakaInitImageRef); err != nil {
			return err
		}
	}

	overrideYAML, err := compose.BuildOverride(entries, vakaInitImageRef)
	if err != nil {
		return fmt.Errorf("build override: %w", err)
	}

	return execDockerCompose(args, overrideYAML, extraEnv)
}

// lifecycleOverrideYAML returns the minimal compose override YAML declaring the
// __vaka-init container so lifecycle commands (down/stop/kill/rm) include it in
// their operation. Returns "" when vakaInitPresent is true; execDockerCompose
// then runs as a pure passthrough without injecting -f -.
func lifecycleOverrideYAML(vakaInitPresent bool, imageRef string) (string, error) {
	if vakaInitPresent {
		return "", nil
	}
	return compose.BuildVakaInitOnlyOverride(imageRef)
}

// runLifecycle handles lifecycle commands: down, stop, kill, rm.
func runLifecycle(args []string, vakaInitPresent bool) error {
	overrideYAML, err := lifecycleOverrideYAML(vakaInitPresent, vakaInitBaseImage+":"+version)
	if err != nil {
		return fmt.Errorf("build vaka-init container override: %w", err)
	}
	return execDockerCompose(args, overrideYAML, nil)
}

// computeCapDelta returns capabilities vaka needs that are absent from Docker's
// default set and not already in the merged compose service's cap_add.
func computeCapDelta(svc composetypes.ServiceConfig) []string {
	existing := map[string]bool{}
	for _, cap := range svc.CapAdd {
		existing[strings.ToUpper(cap)] = true
	}
	var delta []string
	for _, cap := range []string{"NET_ADMIN"} {
		if !existing[cap] && !defaultDockerCaps["CAP_"+cap] {
			delta = append(delta, cap)
		}
	}
	return delta
}
```

- [ ] **Step 5: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`.

- [ ] **Step 6: Update main.go — classifySubcmd dispatch, cobra stubs**

Replace `cmd/vaka/main.go` with:

```go
// cmd/vaka/main.go
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:   "vaka",
	Short: "Secure container layer for AI agentic harnesses",
	Long: `vaka enforces nftables egress policy inside Docker containers running
AI agentic harnesses. Run 'vaka up' instead of 'docker compose up'.`,
	SilenceUsage: true,
}

func main() {
	rootCmd.AddCommand(
		newValidateCmd(),
		newShowCmd(),
		&cobra.Command{
			Use:                "up [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose up",
			Long:               "Use --vaka-init-present to skip __vaka-init container injection (binaries baked into image).",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "run [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose run",
			Long:               "Use --vaka-init-present to skip __vaka-init container injection (binaries baked into image).",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "create [compose-flags...]",
			Short:              "Validate, inject vaka policy, and proxy to docker compose create",
			Long:               "Use --vaka-init-present to skip __vaka-init container injection (binaries baked into image).",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "down [compose-flags...]",
			Short:              "Tear down the stack including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "stop [compose-flags...]",
			Short:              "Stop services including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "kill [compose-flags...]",
			Short:              "Kill services including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:                "rm [compose-flags...]",
			Short:              "Remove stopped containers including the __vaka-init container",
			Long:               "Use --vaka-init-present if the stack was started with that flag.",
			DisableFlagParsing: true,
			Run:                func(*cobra.Command, []string) {},
		},
		&cobra.Command{
			Use:   "version",
			Short: "Print version",
			Run: func(cmd *cobra.Command, args []string) {
				fmt.Println("vaka", version)
			},
		},
	)

	raw := os.Args[1:]
	vakaFlags, rest := extractVakaFlags(raw)
	vakaFile := vakaFlags["--vaka-file"]
	if vakaFile == "" {
		vakaFile = "vaka.yaml"
	}
	vakaInitPresent := vakaFlags["--vaka-init-present"] == "true"

	subcmd := findSubcmd(rest)

	switch classifySubcmd(subcmd) {
	case pathCobra:
		rootCmd.SetArgs(rest)
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}

	case pathFull:
		if err := runFull(vakaFile, rest, vakaInitPresent); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}

	case pathLifecycle:
		if err := runLifecycle(rest, vakaInitPresent); err != nil {
			fmt.Fprintln(os.Stderr, "vaka:", err)
			os.Exit(exitCode(err))
		}

	default: // pathPassthrough
		if err := execDockerCompose(rest, "", nil); err != nil {
			os.Exit(exitCode(err))
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
```

- [ ] **Step 7: Run all tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`.

- [ ] **Step 8: Commit**

```bash
git add cmd/vaka/intercept.go cmd/vaka/intercept_test.go cmd/vaka/inject.go cmd/vaka/main.go
git commit -m "feat(vaka): rename up.go→intercept.go; three-path dispatch (full/lifecycle/passthrough); add create/stop/kill/rm interception"
```

---

### Task 8: Dockerfile — binary paths and VOLUME

**Files:**
- Modify: `docker/init/Dockerfile`

Change `COPY` destinations from `/opt/vaka/bin/` to `/opt/vaka/sbin/` and add `VOLUME /opt/vaka`.

- [ ] **Step 1: Update Dockerfile**

Replace the content of `docker/init/Dockerfile` with:

```dockerfile
# docker/init/Dockerfile
# Assembles the emsi/vaka-init scratch image from two pre-built static binaries.
# The build context must contain exactly two files:
#   vaka-init   — static vaka-init binary (native arch, built by build.sh)
#   nft         — static nft binary (native arch, extracted from emsi/nft-static)
#
# Usage when baking into a harness image:
#   FROM emsi/vaka-init:latest AS vaka
#   FROM ubuntu:24.04
#   COPY --from=vaka /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
#   COPY --from=vaka /opt/vaka/sbin/nft       /opt/vaka/sbin/nft

ARG VERSION=dev
ARG NFTABLES_VERSION=unknown

FROM scratch
ARG VERSION=dev
LABEL org.opencontainers.image.title="emsi/vaka-init" \
      org.opencontainers.image.description="vaka-init + nft static binaries for the vaka secure container layer" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/infrasecture/vaka"

COPY vaka-init /opt/vaka/sbin/vaka-init
COPY nft       /opt/vaka/sbin/nft
VOLUME /opt/vaka
```

- [ ] **Step 2: Update build.sh verification paths**

`build.sh` Phase 5 verifies the built image contains the expected binary paths. After moving from `bin` to `sbin` in the Dockerfile, the verification loop must also change.

In `build.sh`, find the verification loop (around line 384):
```bash
for expected in opt/vaka/bin/nft opt/vaka/bin/vaka-init; do
```
Replace with:
```bash
for expected in opt/vaka/sbin/nft opt/vaka/sbin/vaka-init; do
```

- [ ] **Step 3: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`.

- [ ] **Step 4: Commit**

```bash
git add docker/init/Dockerfile build.sh
git commit -m "feat(dockerfile): move binaries to /opt/vaka/sbin; add VOLUME /opt/vaka; fix build.sh verify paths"
```

---

### Task 9: README and spec docs

**Files:**
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-04-14-vaka-secure-container-design.md`

The goal is not just mechanical string replacement — the README must be structurally rewritten to reflect the feature's intent: **no container image changes required by default**. Baked-in instructions move from the main onboarding path to an advanced opt-out section.

- [ ] **Step 1: Mechanical replacements in README.md**

```bash
sed -i \
  's|vaka\.dev/v1alpha1|agent.vaka/v1alpha1|g; s|/usr/local/sbin/vaka-init|/opt/vaka/sbin/vaka-init|g; s|/usr/local/sbin/nft|/opt/vaka/sbin/nft|g' \
  README.md
```

- [ ] **Step 2: Confirm the opening claim is now accurate**

Find the sentence that lists what vaka works without. It must include "without changing your container images" as a true, unconditional statement. Verify it reads as a top-level claim, not buried in a conditional.

- [ ] **Step 3: Rewrite the "Installation / Getting started" onboarding section**

Remove any steps that tell users to add `vaka-init` or `nft` to their image as part of the normal setup. The new onboarding path is:

1. Add vaka policy (`vaka.yaml`)
2. Run `vaka up` — binaries are injected automatically from `emsi/vaka-init:<version>`

No Dockerfile changes. No `COPY` steps. No manual binary installation.

- [ ] **Step 4: Add "Advanced: opt-out for air-gapped environments" section**

Create a new section (e.g. `### Air-gapped / opt-out`) explaining that users who cannot pull `emsi/vaka-init` from the internet can bake the binaries into their image and skip injection:

```markdown
### Air-gapped / opt-out

If your environment cannot pull `emsi/vaka-init` from the internet, bake the binaries
into your image and pass `--vaka-init-present` to skip automatic injection:

```dockerfile
FROM emsi/vaka-init:v0.1.2 AS vaka
FROM ubuntu:24.04
COPY --from=vaka /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
COPY --from=vaka /opt/vaka/sbin/nft       /opt/vaka/sbin/nft
```

Then run with:
```bash
vaka up --vaka-init-present
vaka down --vaka-init-present
```

Per-service opt-out via `docker-compose.yaml` label:
```yaml
services:
  myapp:
    labels:
      agent.vaka.init: present
```
```

- [ ] **Step 5: Document `vaka down`**

Add a short section (or extend the existing CLI reference) documenting that `vaka down` tears down the full stack including the `__vaka-init` container, and that `--vaka-init-present` must be passed if the stack was started with that flag.

- [ ] **Step 6: Mechanical replacements in spec doc**

```bash
sed -i \
  's|vaka\.dev/v1alpha1|agent.vaka/v1alpha1|g; s|/usr/local/sbin/vaka-init|/opt/vaka/sbin/vaka-init|g; s|/usr/local/sbin/nft|/opt/vaka/sbin/nft|g' \
  docs/superpowers/specs/2026-04-14-vaka-secure-container-design.md
```

- [ ] **Step 7: Run tests — expect all pass**

```bash
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... 2>&1
```
Expected: all `ok`.

- [ ] **Step 8: Commit**

```bash
git add README.md docs/superpowers/specs/2026-04-14-vaka-secure-container-design.md
git commit -m "docs: restructure README for no-image-changes default; add opt-out section; update apiVersion and paths"
```

---

## Verification checklist

After all tasks:

```bash
# All tests green
docker run --rm -v "$(pwd)":/src -w /src golang:1.25-alpine go test ./pkg/... ./cmd/... -v 2>&1

# No remaining vaka.dev/v1alpha1 references in code or docs
# (plans/ and specs/ are excluded: they legitimately reference the old value in
#  before/after examples and test-case descriptions)
grep -rn "vaka\.dev/v1alpha1" . --include="*.go" --include="*.md" --include="*.yaml" \
  | grep -v ".worktrees" | grep -v "docs/superpowers/"

# No old binary paths in Go source
grep -rn "/usr/local/sbin" . --include="*.go"

# __vaka-init container name consistent
grep -rn "__vaka-init\|vaka-init:ro\|service_completed_successfully" pkg/compose/ cmd/vaka/

# three-path dispatch wired
grep -n "classifySubcmd\|pathFull\|pathLifecycle\|pathPassthrough" cmd/vaka/intercept.go cmd/vaka/main.go

# vakaVersion wired in CLI
grep -n "VakaVersion" cmd/vaka/intercept.go pkg/policy/types.go

# checkVersion wired in vaka-init
grep -n "checkVersion" cmd/vaka-init/main.go
```
