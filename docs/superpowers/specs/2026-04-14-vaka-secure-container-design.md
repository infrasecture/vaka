# vaka — Secure Agentic Container Layer: Design Spec

**Date:** 2026-04-14
**Status:** Approved
**Scope:** `vaka-init` (container init binary) + `vaka` (host CLI) + `ServicePolicy` config schema.
Secret-isolation architecture (LLM proxy, MCP sidecar) is out of scope here and will be a separate spec.

---

## 1. Problem Statement

Standard Linux containers isolate the process from the host but do not restrict egress traffic. An AI agentic harness running inside a container (Claude Code, Codex CLI, OpenCode, etc.) may have access to sensitive credentials and API keys required for its operation. Two distinct threats:

1. **Unrestricted egress.** The harness can make outbound network connections to any destination — exfiltrating data, phoning home, or being redirected by a prompt-injection attack.
2. **Secret exposure.** API keys and tokens available to the harness process can be read from environment variables, process memory, or config files, and potentially exfiltrated via the unrestricted network.

This spec addresses threat 1 in full. Threat 2 is documented as future work (see Section 9).

---

## 2. Solution Overview

A two-component system:

- **`vaka-init`** — a minimal static Go binary that runs as the container's entrypoint. It reads a policy file injected via Docker secrets, configures nftables egress rules, drops Linux capabilities, optionally changes UID/GID, then `execve`s the actual harness. After handoff, `vaka-init` no longer exists in memory.
- **`vaka` CLI** — a host-side tool that reads `vaka.yaml`, validates it strictly, generates a Docker Compose override (piped via stdin), and proxies `docker compose up`. It injects per-service policy as Docker secrets via environment variables, rewrites service entrypoints to `vaka-init` transparently, and leaves no artifacts on disk.

The operator writes a normal `docker-compose.yaml` with no mention of `vaka`. The `vaka` CLI handles everything behind the scenes.

---

## 3. Repository Structure

```
vaka/                              # monorepo (LGPL-2.1)
├── LICENSE
├── go.work                        # Go workspace (single module: vaka.dev/vaka)
├── go.mod
├── nft/                           # existing — static nft binary build artifact
│   ├── Dockerfile
│   ├── build_and_test.sh
│   └── README.md
├── cmd/
│   ├── vaka/                      # host CLI binary
│   │   └── main.go
│   └── vaka-init/                 # container init binary
│       └── main.go
├── pkg/
│   ├── policy/                    # schema types, YAML parser, validator
│   │   ├── types.go
│   │   ├── parse.go
│   │   └── validate.go
│   └── nft/                       # policy → nft ruleset string generator
│       ├── generate.go
│       ├── resolve.go             # DNS + service name resolution
│       └── templates/
│           └── egress.nft.tmpl   # Go text/template; embedded via embed.FS
├── docker/
│   └── init/
│       └── Dockerfile             # base image: nft-static + vaka-init
└── docs/
    └── superpowers/specs/
        └── 2026-04-14-vaka-secure-container-design.md
```

**Build constraints:**
- `cmd/vaka-init` is built with `CGO_ENABLED=0 GOOS=linux` — fully static binary, no libc dependency.
- `docker/init/Dockerfile` is a multi-stage build: copies `/opt/nftables/bin/nft` from `emsi/nft-static` and the `vaka-init` binary from the Go build stage. The resulting base image can be used as a `COPY --from=` source in any harness Dockerfile.
- `cmd/vaka` is built for the host platform; no static requirement.

---

## 4. Config Schema

### 4.1 Format

Kubernetes-style resource manifest. `apiVersion` and `kind` provide a versioned upgrade path. Unknown fields are a hard error.

**File:** `vaka.yaml` (host-side, all services)

```yaml
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy

services:
  codex:
    network:
      egress:
        defaultAction: reject       # accept | reject | drop
        accept:
          - dns: {}                 # special: see Section 4.3
          - proto: tcp
            to: [llm-gateway, 10.20.0.0/16]
            ports: [443, 80]
          - proto: udp
            to: [0.0.0.0/0, ::/0]
            ports: [123]
        reject: []
        drop:
          - proto: icmp
            type: echo-request      # named or numeric (8)
        block_metadata: true        # optional; see Section 4.3; false if omitted
    runtime:
      dropAllCapsAfterInit: true    # drop ALL caps after nft applied; overrides dropCaps
      dropCaps:                     # specific caps to drop (used when dropAllCapsAfterInit: false)
        - NET_ADMIN
        - SYS_PTRACE
      runAs:
        uid: 1000
        gid: 1000

  llm-gateway:
    network:
      egress:
        defaultAction: reject
        accept:
          - proto: tcp
            to: [0.0.0.0/0]
            ports: [443]
    runtime:
      dropAllCapsAfterInit: true
      runAs:
        uid: 1000
        gid: 1000
```

### 4.2 Per-container injected document

The `vaka` CLI slices out exactly one service and serialises it. `vaka-init` reads whichever single service it finds — it does not need to know its own name:

```yaml
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy

services:
  codex:
    network:
      egress:
        defaultAction: reject
        accept:
          - dns: {}
          - proto: tcp
            to: [llm-gateway]
            ports: [443]
    runtime:
      dropAllCapsAfterInit: true
      runAs:
        uid: 1000
        gid: 1000
```

Delivered to the container at `/run/secrets/vaka.yaml` via the Docker secrets mechanism.

### 4.3 Special syntax

#### `dns: {}`

Shorthand that expands at `vaka-init` startup time:

1. Parse `/etc/resolv.conf` and extract all `nameserver` lines.
2. Generate `accept` rules for UDP/53 and TCP/53 to each nameserver address, for both IPv4 and IPv6.
3. If `/etc/resolv.conf` is absent or contains no `nameserver` lines, `vaka-init` exits with an error (fail-closed).

```yaml
- dns: {}                   # allows DNS to resolvers from /etc/resolv.conf
- dns:
    servers: [8.8.8.8, 1.1.1.1]   # narrows to specific servers (overrides resolv.conf)
```

#### `to:` field

A flat list of strings. Each entry is auto-detected by the parser:

| Pattern | Interpretation |
|---|---|
| Valid IPv4 address | Single host, `/32` implied |
| Valid IPv6 address | Single host, `/128` implied |
| Contains `/` | CIDR block (IPv4 or IPv6) |
| `0.0.0.0/0` | Any IPv4 destination |
| `::/0` | Any IPv6 destination |
| Anything else | DNS name — resolved to A + AAAA records at `vaka-init` startup |

Name resolution at init time means resolved IPs are baked into the static nft ruleset. Dynamic IP changes after startup are not reflected. This is intentional — it makes the ruleset auditable and deterministic.

#### `block_metadata`

A top-level boolean under `network.egress`. When `true`, `vaka-init` prepends explicit `drop` rules for all known cloud instance metadata service endpoints before any user rules. These rules are part of the implicit invariants section of the generated ruleset and take precedence over user-defined `accept` rules.

Covered ranges (as of v1alpha1):

| Cloud | IPv4 | IPv6 |
|---|---|---|
| AWS, GCP, Azure, DigitalOcean, Hetzner | `169.254.169.254/32` | `fd00:ec2::254/128` (AWS) |
| Alibaba Cloud | `100.100.100.200/32` | — |
| Azure IMDS (additional) | `169.254.169.254/32` | — |
| Link-local metadata range (general) | `169.254.0.0/16` | — |

```yaml
egress:
  block_metadata: true    # drop all known metadata endpoints; false if omitted
  defaultAction: reject
  accept:
    - dns: {}
```

When `block_metadata: false` (the default), no metadata rules are generated. Operators running in cloud environments where the metadata service exposes IAM credentials or tokens **should** set `block_metadata: true`.

The metadata block rules are inserted into the ruleset immediately after the implicit invariants (`established,related` and `lo`) and before any user-defined `drop`/`reject`/`accept` rules:

```
ct state established,related accept   # implicit invariant
iif "lo" accept                        # implicit invariant
ip  daddr 169.254.0.0/16 drop         # block_metadata: true
ip  daddr 100.100.100.200/32 drop     # block_metadata: true
ip6 daddr fd00:ec2::254/128 drop      # block_metadata: true
# ... user rules follow
```

#### Port ranges

```yaml
ports: [443, 80]          # list of integers
ports: ["8080-8090"]      # range as string; N < M, both 1–65535
ports: [443, "8080-8090"] # mixed
```

#### ICMP types

```yaml
type: echo-request    # nft named type (preferred)
type: 8               # numeric; normalised to nft name internally
```

Applies to both `icmp` (IPv4) and `icmpv6` (IPv6) when `proto` is not specified.
When `proto: icmp` or `proto: icmpv6` is specified, only that family is targeted.

### 4.4 Field reference

| Field | Type | Values | Required |
|---|---|---|---|
| `apiVersion` | string | `vaka.dev/v1alpha1` | yes |
| `kind` | string | `ServicePolicy` | yes |
| `services.<name>.network.egress.defaultAction` | string | `accept`, `reject`, `drop`; default `reject` if omitted | no |
| `services.<name>.network.egress.accept` | list | Rule entries | no |
| `services.<name>.network.egress.reject` | list | Rule entries | no |
| `services.<name>.network.egress.drop` | list | Rule entries | no |
| `rule.proto` | string | `tcp`, `udp`, `icmp`, `icmpv6` | no (omit = all) |
| `rule.to` | list of string | IP, CIDR, hostname | no (omit = any) |
| `rule.ports` | list | int or `"N-M"` string | no (omit = any) |
| `rule.type` | string or int | ICMP type name or 0–255 | no |
| `rule.dns` | map | `{}` or `servers: [...]` | special shorthand |
| `services.<name>.network.egress.block_metadata` | bool | `true`/`false`; default `false` | no |
| `runtime.dropAllCapsAfterInit` | bool | `true`/`false` | no (default false) |
| `runtime.dropCaps` | list of string | Linux capability names | no |
| `runtime.runAs.uid` | int | ≥ 0 | no |
| `runtime.runAs.gid` | int | ≥ 0 | no |

---

## 5. `vaka-init` — Container Init Binary

### 5.1 Usage

```dockerfile
# In the harness image:
COPY --from=ghcr.io/vaka/init:latest /opt/vaka/bin/vaka-init /usr/local/sbin/vaka-init
COPY --from=ghcr.io/vaka/init:latest /opt/nftables/bin/nft   /usr/local/sbin/nft

ENTRYPOINT ["vaka-init", "--"]
CMD ["claude", "--dangerously-skip-permissions"]
```

Arguments after `--` are the harness entrypoint and its arguments. `vaka-init` replaces itself with the harness via `execve` — PID 1 becomes the harness process.

### 5.2 Startup sequence

```
1. Read /run/secrets/vaka.yaml
   Parse strictly — exactly one service entry must be present.
   Schema version must be vaka.dev/v1alpha1.

2. Resolve dynamic values:
   a. dns: {}  → parse /etc/resolv.conf, extract nameserver entries
   b. service names in to: → DNS lookup (A + AAAA records)
   All resolution happens before any nft operation.

3. Generate nft ruleset string in memory via Go template (see Sections 5.3 and 5.4).

4. Apply ruleset:
   nft -f /dev/stdin  (ruleset piped to stdin — no temp file written)

   nft -f applies the entire ruleset atomically at the kernel level: nftables reads
   the full input, builds the new configuration in memory, and commits it in a single
   transaction. There is no intermediate "half-loaded" firewall state. If the ruleset
   is malformed or the kernel rejects it for any reason, the previous state (empty or
   existing tables) is preserved and vaka-init exits with an error (fail-closed).
   Reference: https://wiki.nftables.org/wiki-nftables/index.php/Atomic_rule_replacement

5. Drop capabilities:
   if dropAllCapsAfterInit: true:
     Iterate CAP_0..CAP_LAST_CAP calling prctl(PR_CAPBSET_DROP, cap) for each,
     removing all capabilities from the bounding set.
   else:
     Call prctl(PR_CAPBSET_DROP, cap) for each capability listed in dropCaps.
   NET_ADMIN is always dropped after step 4, regardless of config — it is the capability
   that allowed nft to run and must not be retained by the harness.

6. Apply runAs (if specified):
   setgid(gid) then setuid(uid) — gid must be set first.

7. execve(argv[1:])
   Replaces vaka-init with the harness. vaka-init ceases to exist.
```

### 5.3 Generated nft ruleset

`vaka-init` creates a single table `inet vaka` (the `inet` family covers both IPv4 and IPv6):

```
table inet vaka {
  chain egress {
    type filter hook output priority 0;
    policy accept;

    # ── IMPLICIT INVARIANTS ─────────────────────────────────────────────────
    # These rules are always generated first, in this order.
    # They are not configurable in v1 and cannot be suppressed.
    #
    # 1. Allow return traffic for connections already established outbound.
    #    Without this, TCP handshake replies would be blocked after the
    #    first packet was accepted.
    ct state established,related accept

    # 2. Allow loopback. Matched by interface name, not IP address.
    iif "lo" accept
    # ────────────────────────────────────────────────────────────────────────

    # ── EXPLICIT drop RULES (from drop: list, top to bottom) ────────────────
    meta l4proto icmp  icmp   type echo-request drop
    meta l4proto icmpv6 icmpv6 type echo-request drop

    # ── EXPLICIT reject RULES (from reject: list, top to bottom) ───────────
    # (none in this example)

    # ── EXPLICIT accept RULES (from accept: list, top to bottom) ───────────
    # dns: {} — resolved from /etc/resolv.conf
    ip  daddr { 8.8.8.8 } udp dport 53 accept
    ip  daddr { 8.8.8.8 } tcp dport 53 accept
    ip6 daddr { 2001:4860:4860::8888 } udp dport 53 accept
    ip6 daddr { 2001:4860:4860::8888 } tcp dport 53 accept

    # service: llm-gateway — resolved at init time
    ip  daddr { 192.168.1.10 } tcp dport { 443, 80 } accept
    ip  daddr { 10.20.0.0/16 } tcp dport { 443, 80 } accept

    # ── DEFAULT ACTION ───────────────────────────────────────────────────────
    # icmpx = protocol-agnostic: emits correct ICMP/ICMPv6 message automatically
    reject with icmpx type port-unreachable   # defaultAction: reject
    # (or: drop / accept)
  }
}
```

**Evaluation order:** nftables evaluates rules top to bottom; the first matching terminal verdict (`accept`, `reject`, `drop`) wins. The generated order is always:

1. Implicit invariants (`established,related` → `lo`)
2. `drop` rules (config order preserved)
3. `reject` rules (config order preserved)
4. `accept` rules (config order preserved)
5. Default action rule

This means explicit `drop` and `reject` rules take precedence over `accept` rules for the same traffic. A rule higher in the same list beats a rule lower in the same list.

**IPv4 + IPv6:** Every rule without an explicit CIDR family generates matching rules for both `ip` and `ip6`. A CIDR containing `:` is IPv6-only; a CIDR containing `.` is IPv4-only.

**Table ownership:** `vaka-init` creates only the `vaka` table. It does not flush or modify any pre-existing tables. Host network rules are untouched.

**Auditability:** After init, the live ruleset is always inspectable from the host with:
```
docker exec <container> nft list table inet vaka
```

### 5.4 Ruleset generation via Go templates

The nft ruleset string is produced by `pkg/nft` using Go's `text/template` package. This keeps rule formatting readable and auditable without string concatenation logic scattered across the codebase.

`pkg/nft/templates/egress.nft.tmpl`:

```
table inet vaka {
  chain egress {
    type filter hook output priority 0;
    policy accept;

    # implicit invariants
    ct state established,related accept
    iif "lo" accept
{{- if .BlockMetadata }}

    # metadata endpoint block (block_metadata: true)
{{- range .MetadataRanges }}
    {{ . }} drop
{{- end }}
{{- end }}
{{- if .DropRules }}

    # explicit drop rules
{{- range .DropRules }}
    {{ . }}
{{- end }}
{{- end }}
{{- if .RejectRules }}

    # explicit reject rules
{{- range .RejectRules }}
    {{ . }}
{{- end }}
{{- end }}
{{- if .AcceptRules }}

    # explicit accept rules
{{- range .AcceptRules }}
    {{ . }}
{{- end }}
{{- end }}

    # default action
    {{ .DefaultVerdict }}
  }
}
```

The `pkg/nft` generator:
1. Receives a resolved `policy.EgressPolicy` struct (all DNS names already expanded to IPs).
2. Expands each rule entry into one or more nft rule strings (one per IP family as needed).
3. Executes the template with a `RulesetData` struct containing the pre-rendered rule strings and metadata.
4. Returns the final ruleset string to `vaka-init`, which pipes it to `nft -f /dev/stdin`.

This approach means the template stays stable and auditable; complexity lives in the Go rule-expansion logic, not in the template itself. The template file can be embedded via `embed.FS` so no separate file is needed at runtime.

### 5.5 Error handling

`vaka-init` follows a fail-closed principle: any error in steps 1–7 causes immediate exit with a non-zero status and a descriptive message to stderr. It never proceeds to `execve` with a partial or uncertain security posture.

---

## 6. `vaka` CLI — Host Tool

### 6.1 Commands

```
vaka up [compose-flags...]   Validate, inject, proxy to docker compose up
vaka validate                Validate vaka.yaml; print per-service summary; exit non-zero on error
vaka show <service>          Print generated nft ruleset for <service> (dry-run, no DNS resolution)
vaka version                 Print version
```

All flags not recognised by `vaka up` are forwarded verbatim to `docker compose up`.

### 6.2 `vaka up` sequence

```
1. Locate vaka.yaml (current directory, or -f <path>).
   Locate docker-compose.yaml (standard Compose discovery rules).

2. Parse vaka.yaml strictly (unknown fields → hard error with field path).
   Parse docker-compose.yaml to read existing entrypoint/command per service.

3. Validate vaka.yaml (see Section 6.3).

4. For each service defined in vaka.yaml:
   a. Serialise as a single-service ServicePolicy YAML document.
   b. base64-encode (standard encoding, no line breaks).
   c. Store as VAKA_<UPPER_SERVICE_NAME>_CONF=<base64>.
      Hyphens in service names are replaced with underscores for the env var key.
      Example: service "llm-gateway" → VAKA_LLM_GATEWAY_CONF

5. For each service, determine the harness entrypoint + command:
   a. If service has explicit entrypoint: or command: in docker-compose.yaml → use those.
   b. Otherwise → docker image inspect <image> → read .Config.Entrypoint and .Config.Cmd.
   c. If neither yields a result → vaka up errors:
      "Error: service codex has no entrypoint. Add entrypoint: to docker-compose.yaml."

6. Build override YAML in memory:

   secrets:
     vaka_codex_conf:
       environment: "VAKA_CODEX_CONF"
     vaka_llm_gateway_conf:
       environment: "VAKA_LLM_GATEWAY_CONF"

   services:
     codex:
       entrypoint: ["vaka-init", "--"]
       command: ["claude", "--dangerously-skip-permissions"]
       secrets:
         - source: vaka_codex_conf
           target: vaka.yaml
     llm-gateway:
       entrypoint: ["vaka-init", "--"]
       command: ["/usr/local/bin/litellm", "--config", "/etc/litellm.yaml"]
       secrets:
         - source: vaka_llm_gateway_conf
           target: vaka.yaml

7. Exec docker compose:
   exec.Command("docker", "compose",
     "-f", "docker-compose.yaml",
     "-f", "-",                        # read override from stdin
     "up", <user-flags...>)
   cmd.Stdin  = strings.NewReader(overrideYAML)
   cmd.Stdout = os.Stdout
   cmd.Stderr = os.Stderr
   cmd.Env    = append(os.Environ(), "VAKA_CODEX_CONF=...", "VAKA_LLM_GATEWAY_CONF=...", ...)
   cmd.Run()
```

The override YAML exists only in process memory. No temp files are created. The `VAKA_*_CONF` environment variables are set on the `docker compose` child process and are read by Docker when materialising the declared secrets into containers.

### 6.3 Host-side validation

Validation runs before any docker interaction. A single validation failure prints a precise error and exits non-zero. All errors are surfaced, not just the first.

| Check | Detail |
|---|---|
| YAML parseable | `yaml.v3` strict decode into typed structs — unknown fields rejected with field path |
| `apiVersion` | Must be exactly `vaka.dev/v1alpha1` |
| `kind` | Must be exactly `ServicePolicy` |
| Service name | Valid DNS label; must exist in `docker-compose.yaml` |
| `defaultAction` | One of: `accept`, `reject`, `drop`. Defaults to `reject` if omitted. `vaka` CLI emits a prominent warning when `accept` is used: "WARNING: service <name> uses defaultAction: accept — all unmatched egress traffic is allowed." |
| `network_mode` (compose) | Services listed in `vaka.yaml` must not use `network_mode: host` in `docker-compose.yaml`. `vaka up` and `vaka validate` both hard-error if this condition is detected: "Error: service <name> uses network_mode: host. vaka cannot isolate a container sharing the host network namespace. Remove network_mode: host or exclude this service from vaka.yaml." |
| `proto` | One of: `tcp`, `udp`, `icmp`, `icmpv6` |
| `to:` entries | Each string: valid IPv4, valid IPv6, valid CIDR, or syntactically valid hostname (`[a-z0-9]([a-z0-9\-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9\-]*[a-z0-9])?)*`) |
| `ports:` entries | Integer 1–65535, or string `"N-M"` where `N < M` and both in range |
| `dropCaps` entries | Short-form capability names (`NET_ADMIN`, `SYS_PTRACE`); `CAP_` prefix accepted and stripped; validated against known Linux capability list |
| `uid` / `gid` | Non-negative integer |
| ICMP `type` | Named nft ICMP type, or integer 0–255 |

DNS resolution of `to:` hostnames and `dns: {}` nameserver lookup are **not** performed during host-side validation — they happen inside the container at `vaka-init` startup. Host validation checks syntax only.

### 6.4 `vaka show <service>`

Generates and prints the nft ruleset that would be applied for the named service. DNS names and service names in `to:` are not resolved — they appear as comments:

```nft
# NOTE: "llm-gateway" will be resolved at container init time
ip daddr { /* llm-gateway */ } tcp dport { 443 } accept
```

Useful for auditing before deployment.

### 6.5 `vaka validate`

```
$ vaka validate
✓ codex        — 2 accept rules, 1 drop rule,  defaultAction: reject
✓ llm-gateway  — 1 accept rule,  0 drop rules, defaultAction: reject
```

Exits 0 on success, non-zero with diagnostics on failure.

---

## 7. `docker/init` Base Image

`docker/init/Dockerfile`:

```dockerfile
FROM emsi/nft-static:1.1.6 AS nft

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/vaka-init ./cmd/vaka-init

FROM scratch
COPY --from=nft     /opt/nftables/bin/nft   /opt/vaka/bin/nft
COPY --from=builder /out/vaka-init          /opt/vaka/bin/vaka-init
```

Harness images use it as:

```dockerfile
FROM ghcr.io/vaka/init:latest AS vaka
FROM ubuntu:24.04
COPY --from=vaka /opt/vaka/bin/vaka-init /usr/local/sbin/vaka-init
COPY --from=vaka /opt/vaka/bin/nft       /usr/local/sbin/nft
ENTRYPOINT ["vaka-init", "--"]
CMD ["your-harness-entrypoint"]
```

The container image carries only two binaries from vaka: `vaka-init` and `nft`. No other vaka code runs inside the container.

---

## 8. Required Container Capabilities

`vaka-init` requires `CAP_NET_ADMIN` to configure nftables. This capability must be granted at container startup and is **always** dropped after the ruleset is applied, regardless of the `dropCaps` / `dropAllCapsAfterInit` config. This is enforced by `vaka-init` code, not config.

In `docker-compose.yaml`:

```yaml
services:
  codex:
    cap_add:
      - NET_ADMIN
    cap_drop:
      - ALL
```

The `cap_add: [NET_ADMIN]` is required. `vaka-init` drops it as part of its startup sequence before `execve`. The harness process never has `NET_ADMIN`.

---

## 9. Future Work: Secret Isolation (Threat 2)

The following ideas are documented for a future spec. No implementation is planned here.

**LLM API proxy as sidecar:**
Run a proxy (custom or LiteLLM) as a separate container on the same docker-compose network. The proxy holds the real Anthropic/OpenAI/Google API keys. The harness container is given a one-time ephemeral client key scoped to the proxy only. The harness has no path to the real API key — even inspecting its own process memory yields only the ephemeral key, which has no value outside the proxy.

**MCP tools as network services:**
Run MCP tool servers as separate containers accessible over the docker-compose network rather than as subprocesses inside the harness container. This limits the harness's ability to compromise tools and prevents tool processes from accessing harness memory.

**Filesystem secret scrubbing:**
After `vaka-init` reads `/run/secrets/vaka.yaml`, it could `umount` the secrets tmpfs before `execve`, preventing the harness from reading the policy document. Whether this is desirable (obscurity) or counterproductive (makes debugging harder) needs evaluation.

**Memory isolation:**
Preventing an agent from reading its own process memory (env vars, heap) is an unsolved problem in the general case. Possible mitigations include: injecting credentials via Unix domain socket after startup (rather than env vars), using kernel keyrings, or running the harness under a restricted seccomp profile that blocks `ptrace`.

---

## 10. Design Decisions and Rationale

| Decision | Rationale |
|---|---|
| Go for both binaries | Static binary support, good syscall coverage, fast iteration, ecosystem fit |
| No `x-vaka:` in compose (v1) | Avoids coupling vaka config to compose format; can be added later without breaking changes |
| Secrets via env var → Docker secret | No disk artifacts; Docker secrets are not visible in `docker inspect`; clean compose integration |
| Override passed via stdin (`-f -`) | No temp files, no race window, no cleanup on crash |
| Entrypoint rewriting in override | Original compose file needs no vaka awareness; transparent injection |
| Implicit invariants non-suppressible | `established,related` and `lo` are required for any useful container. Making them mandatory prevents footguns. Override mechanism may be added in v2. |
| `inet` table for nft rules | Single table covers IPv4 + IPv6; prevents IPv6 bypass of IPv4-only rules |
| DNS/name resolution at init time | Ruleset is static and auditable post-init; no dependency on dynamic resolution at runtime |
| `NET_ADMIN` always dropped post-nft | Capability that permits firewall modification must not survive into the harness |
| Strict YAML parsing (unknown fields = error) | Typos in config keys would silently have no effect; hard errors prevent misconfiguration |
| Fail-closed on any init error | A partial security posture is worse than no startup; harness does not run if init fails |
| nft ruleset via Go `text/template` | Keeps formatting readable and auditable; rule expansion logic stays in Go, not in template; template embedded via `embed.FS` so no runtime file dependency |
| `defaultAction` defaults to `reject` | Secure by default; operators must explicitly choose `accept` and are warned when they do |
| `block_metadata: false` by default | Opt-in avoids surprising behaviour on non-cloud deployments; operators in cloud environments should enable it explicitly |
| Hard error on `network_mode: host` | Sharing the host network namespace defeats container egress isolation entirely and risks applying nft rules to the host; this must never be allowed silently |
| nft application is atomic | `nft -f` commits the full ruleset in a single kernel transaction — no intermediate half-loaded state is possible |
