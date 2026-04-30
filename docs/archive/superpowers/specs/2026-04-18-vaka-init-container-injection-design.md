# vaka-init Container Injection Design

> Historical implementation note. This document is not the current user or maintainer reference. See the maintained docs in `docs/` for current behavior.

**Feature:** vaka automatically injects `vaka-init` and `nft` binaries into managed containers via a `__vaka-init` container, eliminating the requirement that users bake these binaries into their container images.

**Issue:** [#8](https://github.com/infrasecture/vaka/issues/8)

**Deferred issues filed:** [#16](https://github.com/infrasecture/vaka/issues/16) (multi-arch), [#17](https://github.com/infrasecture/vaka/issues/17) (pre-5.x kernel)

---

## 1. Goal

Remove the requirement that users modify their container images to include `vaka-init` and `nft`. After this change, the README claim — "without changing your container images" — becomes accurate.

---

## 2. Approach

vaka injects a `__vaka-init` container into the compose override alongside existing service patches. The container image (`emsi/vaka-init:<vaka-version>`) contains both binaries at `/opt/vaka/sbin/`. Each managed service mounts its filesystem via `volumes_from` and uses `/opt/vaka/sbin/vaka-init` as its entrypoint.

The `__vaka-init` container runs `entrypoint: ["/opt/vaka/sbin/vaka-init"]` — no arguments. vaka-init detects the absence of `--` and prints its usage message then exits 0. Its filesystem persists for the lifetime of the Compose project. `vaka down` intercepts the `down` command and injects a minimal override so Compose knows to tear the `__vaka-init` container down.

---

## 3. Binary paths

All paths are standardised to `/opt/vaka/sbin/` in both injected and baked-in modes:

| Binary | Path |
|---|---|
| `vaka-init` | `/opt/vaka/sbin/vaka-init` |
| `nft` | `/opt/vaka/sbin/nft` |

The injected entrypoint is always `/opt/vaka/sbin/vaka-init` — no conditional logic based on mode.

---

## 4. vaka-init Dockerfile changes

```dockerfile
FROM scratch
COPY vaka-init /opt/vaka/sbin/vaka-init
COPY nft       /opt/vaka/sbin/nft
VOLUME /opt/vaka
```

`VOLUME /opt/vaka` (not `/opt/vaka/sbin`) is declared to expose the full tree, leaving room for future additions alongside `sbin/`.

---

## 5. Compose override structure

```yaml
services:
  __vaka-init:
    image: emsi/vaka-init:v0.1.2   # tag = vaka CLI version
    entrypoint: ["/opt/vaka/sbin/vaka-init"]
    restart: "no"

  myapp:
    entrypoint: ["/opt/vaka/sbin/vaka-init", "--"]
    command: [<original entrypoint + command>]
    depends_on:
      __vaka-init:
        condition: service_completed_successfully
    volumes_from:
      - __vaka-init:ro
    cap_add: [NET_ADMIN]
    secrets: [...]
```

`service_completed_successfully` ensures `__vaka-init` has exited with code 0 before the managed service starts. A non-zero exit (e.g., image pull failure) causes Compose to refuse to start the managed service.

`__vaka-init` is emitted once per stack, not once per service.

### Teardown

The lifecycle commands `down`, `stop`, `kill`, and `rm` are all intercepted (not passthrough). vaka injects a minimal override containing only the `__vaka-init` service definition. This tells Docker Compose the service exists so it is included in the operation. No policy parsing or secret injection is needed for these commands.

```yaml
services:
  __vaka-init:
    image: emsi/vaka-init:v0.1.2
    entrypoint: ["/opt/vaka/sbin/vaka-init"]
    restart: "no"
```

When `--vaka-init-present` is passed to any lifecycle command, no override is injected (no `__vaka-init` container was created on `up`).

---

## 6. Opt-out mechanisms

Two opt-out mechanisms allow users to indicate that `vaka-init` and `nft` are already present in the container image (e.g., air-gapped environments).

### 6a. Per-service label in docker-compose.yaml

```yaml
services:
  myapp:
    labels:
      agent.vaka.init: present
```

When this label is present on a service, vaka skips `volumes_from`, `depends_on`, for that service. The entrypoint is still `/opt/vaka/sbin/vaka-init` — the user is responsible for placing the binary there in their image.

The `__vaka-init` container is only omitted from the override if **all** managed services carry the opt-out label.

### 6b. CLI flag

```
vaka up --vaka-init-present [compose-flags...]
vaka run --vaka-init-present [compose-flags...]
```

Equivalent to all services carrying the opt-out label. No `__vaka-init` container injected, no image check or pull performed.

### 6c. Baked-in image instructions

When opting out, users must place binaries at the canonical path:

```dockerfile
FROM emsi/vaka-init:v0.1.2 AS vaka
FROM ubuntu:24.04
COPY --from=vaka /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
COPY --from=vaka /opt/vaka/sbin/nft       /opt/vaka/sbin/nft
```

---

## 7. Docker Go client — entrypoint resolution and image check/pull

The Docker Go client (`github.com/docker/docker/client`) is used for two operations, both via the `DockerServices` interface:

**Entrypoint resolution** (`ResolveEntrypoint`): for each managed service, if the compose file declares neither `entrypoint` nor `command`, vaka inspects the service image to obtain the Dockerfile defaults. This happens unconditionally for every service in the policy.

**Image check and pull** (`EnsureImage`): injection is considered active only after all per-service opt-out labels have been evaluated. The check and pull are skipped entirely when `--vaka-init-present` is set **or** when every managed service carries the `agent.vaka.init: present` label — the same guarantee in both cases.

When injection is active, before invoking `docker compose`, vaka verifies the correct image is present locally:

```
ImageInspect("emsi/vaka-init:<vaka-version>")
  → found: proceed
  → not found: ImagePull("emsi/vaka-init:<vaka-version>")
      → success: proceed
      → failure: fatal error:
          "failed to pull emsi/vaka-init:v0.1.2 — check network connectivity
           or use --vaka-init-present if binaries are baked into the image"
```

The implementation therefore evaluates per-service labels first (building the `entries` list), resolves entrypoints, then decides whether to pull, then calls `BuildOverride`.

A single client is created via `client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())`, respecting `DOCKER_HOST`, TLS settings, and the active Docker context. It is reused for both operations.

Pull progress is streamed to stderr.

`DockerServices` is the interface covering both operations; `*client.Client` satisfies the underlying `dockerClient` narrow interface. Unit tests inject a `fakeDockerClient` stub without a live daemon.

---

## 8. vakaVersion — generation and validation

### 8a. Purpose

`vakaVersion` is a document-level field in the generated per-service policy YAML. It records the version of the vaka CLI that produced the document, allowing vaka-init to verify binary compatibility before applying any rules.

It is separate from `apiVersion` (which is the user-facing schema contract) and is never written by users.

### 8b. Schema

`ServicePolicy` gains a top-level field:

```yaml
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
vakaVersion: v0.1.2          # injected by vaka CLI; never user-written
services:
  ...
```

### 8c. vaka CLI behaviour

- Populates `vakaVersion` from its embedded version string (set via `ldflags` at build time) before marshaling each per-service policy.
- If `vakaVersion` is present in the **user-written** `vaka.yaml`, vaka treats it as a **validation error**. It is a generated field.

### 8d. vaka-init behaviour

- Reads `vakaVersion` before parsing any other fields.
- If absent → fatal error. No rules are applied.
- Validates against its own embedded version using these rules:

| CLI version | vaka-init version | Result |
|---|---|---|
| `v0.1.2` | `v0.1.0` | ✅ same major.minor |
| `v0.1.2` | `v0.2.0` | ❌ minor mismatch — fatal |
| `v0.2.0` | `v0.1.0` | ❌ minor mismatch — fatal |
| `4178cc0` | `4178cc0` | ✅ exact match |
| `4178cc0` | `4178cc0-dirty` | ❌ must match exactly — fatal |

On mismatch, vaka-init exits non-zero before touching nftables. Paired with `service_completed_successfully`, this surfaces as a Compose startup failure.

---

## 9. apiVersion domain rename

All references to `vaka.dev/v1alpha1` are replaced with `agent.vaka/v1alpha1`. This is a breaking change. No migration path is provided — no public release has been made.

Affected locations: `validate.go`, all test fixtures, README, spec documents.

---

## 10. Files changed

| File | Change |
|---|---|
| `docker/init/Dockerfile` | `COPY` paths → `/opt/vaka/sbin/`; add `VOLUME /opt/vaka` |
| `pkg/policy/types.go` | Add `VakaVersion string` to `ServicePolicy` |
| `pkg/policy/validate.go` | Error if `vakaVersion` present in user YAML; update `apiVersion` string |
| `pkg/policy/validate_test.go` | Update `apiVersion` fixtures; add `vakaVersion` error test |
| `pkg/policy/marshal_test.go` | Update `apiVersion` in `roundTripInput` |
| `pkg/policy/parse_test.go` | Mass rename `apiVersion` fixtures |
| `pkg/compose/override.go` | Add `__vaka-init` container; `volumes_from`; `depends_on`; `imageRef string` parameter; `OptOut bool` on `ServiceEntry`; label detection; entrypoint path; add `BuildVakaInitOnlyOverride` |
| `pkg/compose/override_test.go` | Full `__vaka-init` container injection tests; opt-out tests; mixed-stack test |
| `cmd/vaka/inject.go` | Add `vakaFlagsBool` map; update `extractVakaFlags` for boolean flags; `--vaka-init-present` support |
| `cmd/vaka/images.go` | New: `DockerServices` interface (`EnsureImage` + `ResolveEntrypoint`); `dockerClient` narrow interface for testability; `dockerServices` struct; `NewDockerServices()` wires real `*client.Client` once |
| `cmd/vaka/images_test.go` | New: `fakeDockerClient` stub; `dockerServices` unit tests for `EnsureImage` (present/absent/pull-fail) and `ResolveEntrypoint` (compose-declared/inspect/not-found) |
| `cmd/vaka/up.go` → `cmd/vaka/intercept.go` | Rename; `classifySubcmd` (full/lifecycle/passthrough/cobra); `execDockerCompose` shared helper (conditionally injects `-f -`); `runFull` handles `up`/`run`/`create`; `runLifecycle` + `lifecycleOverrideYAML` handle `down`/`stop`/`kill`/`rm` |
| `cmd/vaka/intercept_test.go` | New: `TestClassifySubcmd`; `TestLifecycleOverrideYAMLPassthrough`; `TestLifecycleOverrideYAMLInjectsContainer`; `TestExtractVakaFlagsBool` |
| `cmd/vaka/main.go` | Use `classifySubcmd` dispatch; add cobra stubs for `create`, `down`, `stop`, `kill`, `rm` |
| `cmd/vaka-init/main.go` | `nftBin` const → `/opt/vaka/sbin/nft`; read and validate `vakaVersion`; no-args case exits 0 (prints usage) instead of fatal |
| `cmd/vaka-init/main_test.go` | Rename `apiVersion`; add `checkVersion` tests |
| `README.md` | Update paths, `apiVersion`, baked-in instructions, opening claim |
| `docs/superpowers/specs/2026-04-14-vaka-secure-container-design.md` | Update paths and `apiVersion` |

---

## 11. Testing strategy

**`pkg/compose/override_test.go`:**
- `__vaka-init` container emitted with correct image tag, `entrypoint: ["/opt/vaka/sbin/vaka-init"]`, `restart: "no"`
- `depends_on: service_completed_successfully` on each managed service
- `volumes_from: [__vaka-init:ro]` on each managed service
- Entrypoint always `/opt/vaka/sbin/vaka-init`
- Per-service label opt-out: service skips `volumes_from`/`depends_on`; `__vaka-init` container still emitted if another service needs it
- CLI flag opt-out: no `__vaka-init` container emitted, no `volumes_from` on any service
- All-services opt-out: no `__vaka-init` container emitted

**`pkg/policy/validate_test.go`:**
- `vakaVersion:` in user vaka.yaml → validation error
- `apiVersion: agent.vaka/v1alpha1` → accepted
- `apiVersion: vaka.dev/v1alpha1` → rejected

**`cmd/vaka-init` tests:**
- `vakaVersion` absent → fatal before nftables
- `vakaVersion` minor mismatch → fatal
- `vakaVersion` git hash mismatch → fatal
- `vakaVersion` patch-only difference → accepted

**`cmd/vaka/intercept_test.go`:**
- `classifySubcmd`: up/run/create → pathFull; down/stop/kill/rm → pathLifecycle; validate/show/version → pathCobra; logs/ps/exec → pathPassthrough
- `lifecycleOverrideYAML(true, …)` → returns `""` (passthrough)
- `lifecycleOverrideYAML(false, …)` → returns YAML containing `__vaka-init`

**`cmd/vaka/images_test.go`:**
- `fakeDockerClient` implements the narrow `dockerClient` interface; tests construct `dockerServices{c: dc}` directly
- `EnsureImage`: image present → no pull called; image absent + pull succeeds → pull called; image absent + pull fails → error propagated
- `ResolveEntrypoint`: entrypoint declared in compose → returned directly, no inspect; no entrypoint + image present → inspect used, Dockerfile defaults returned; no entrypoint + image not found → error

**Docker Go client:** `DockerServices` is the single interface for all Docker daemon operations. `dockerServices` holds a `dockerClient` narrow interface; `*client.Client` satisfies it. One `*client.Client` is created by `NewDockerServices()` and reused for both `EnsureImage` and `ResolveEntrypoint`. Unit tests use `fakeDockerClient` without a live daemon.

---

## 12. Known limitations (deferred)

- **Multi-arch:** On mixed-arch hosts, the pulled image may not match the target container architecture. Tracked in [#16](https://github.com/infrasecture/vaka/issues/16). Mitigation: add `arch:` to `runtime:` config.
- **Pre-5.x kernel:** Static `nft` binary may fail on old kernels. Tracked in [#17](https://github.com/infrasecture/vaka/issues/17). Same limitation exists in the baked-in approach.
