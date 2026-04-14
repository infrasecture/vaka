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
│   ├── nft/                       # policy → nft ruleset string generator
│   │   ├── generate.go
│   │   ├── resolve.go             # DNS + service name resolution
│   │   └── templates/
│   │       └── egress.nft.tmpl   # Go text/template for nft DSL; embedded via embed.FS
│   └── compose/                   # compose override generator
│       └── override.go            # builds override struct, marshals to YAML via yaml.Marshal()
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
      dropCaps:                     # optional user override; see Section 4.3 and Section 8
        - NET_ADMIN                 # normally auto-computed by vaka CLI (delta caps)
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
      dropCaps: [NET_ADMIN]         # injected by vaka CLI: the delta caps it added
      runAs:
        uid: 1000
        gid: 1000
```

Delivered to the container at `/run/secrets/vaka.yaml` via the Docker secrets mechanism.

The `dropCaps` list in the injected document is **auto-computed by the vaka CLI** as the delta between the capabilities it added to the compose override and the capabilities already declared in the original `docker-compose.yaml`. The harness operator does not write `dropCaps` — vaka owns it. If `dropCaps` is explicitly set in `vaka.yaml` by the operator, that value takes precedence over the auto-computed delta (see Section 8).

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
oif "lo" accept                       # implicit invariant (output chain → oif)
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
| `runtime.dropCaps` | list of string | Linux capability names (short form); normally auto-computed by vaka CLI; explicit value in `vaka.yaml` overrides auto-computation | no |
| `runtime.runAs.uid` | int | ≥ 0 | no |
| `runtime.runAs.gid` | int | ≥ 0 | no |

---

## 5. `vaka-init` — Container Init Binary

### 5.1 How vaka-init is invoked

`vaka-init` must be present in the harness image (see Section 7) but is **not** set as the image `ENTRYPOINT`. The `vaka` CLI injects it transparently at `vaka up` time by rewriting `entrypoint:` and `command:` in the compose override, passing the original harness entrypoint and command as arguments to `vaka-init` after `--`. Isolation only takes effect when the container is started via `vaka up` — running directly with `docker compose up` bypasses vaka entirely.

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

5. Drop capabilities listed in dropCaps from ALL five capability sets, in this order:
   a. Inheritable (I): cap_set_proc() with each dropCap cleared from I.
      No special capability required to lower your own inheritable set.
   b. Ambient (A): prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0).
      No special capability required.
   c. Bounding (B): prctl(PR_CAPBSET_DROP, cap) for each cap in dropCaps.
      Requires CAP_SETPCAP in the effective set. CAP_SETPCAP is present in the
      default Docker effective set (see Section 8) and is not itself in dropCaps
      unless the user explicitly adds it — so this call succeeds.
      Once a capability is removed from B, no future execve in this process tree
      can regain it, regardless of file capabilities on executed binaries.
   d. Effective (E) and Permitted (P): cap_set_proc() with each dropCap cleared
      from both E and P. The capability is now immediately inactive.

   After step 5, the dropped capabilities are absent from all sets. The remaining
   default Docker capabilities (SETUID, SETGID, SETPCAP, etc.) are still present
   in E and P — they are needed for the next step.

6. Apply runAs (if specified) — MUST happen after cap drop, while SETUID/SETGID
   are still in the effective set:
   setresgid(gid, gid, gid)  — sets real, effective, AND saved-set GID.
                                CAP_SETGID is present in E (default Docker cap).
   setresuid(uid, uid, uid)  — sets real, effective, AND saved-set UID.
                                CAP_SETUID is present in E (default Docker cap).
   Using setresuid/setresgid (not setuid/setgid) ensures the saved-set-ID is also
   changed. GID must be changed before UID.

   When all three UIDs transition from 0 to nonzero, the Linux kernel automatically
   clears the effective (E) and permitted (P) capability sets (capabilities(7),
   "UID fixup"). This is unconditional when SECBIT_KEEP_CAPS and
   SECBIT_NO_SETUID_FIXUP are both unset (the Docker default). No manual
   cap_set_proc() call is needed to clear the remaining caps — the kernel does it.

   After setresuid returns: E={}, P={}, I={dropCaps cleared}, A={},
   B={default Docker bounding minus dropCaps}.

7. execve(argv[1:])
   Replaces vaka-init with the harness. vaka-init ceases to exist.
   The harness inherits empty E and P sets. The bounding set does not contain
   the dropped caps, so execve cannot restore them.
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
    oif "lo" accept
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
    oif "lo" accept
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
   b. Otherwise → call Docker daemon API via Go SDK (github.com/docker/docker/client):
      client.ImageInspect(ctx, image) → read .Config.Entrypoint and .Config.Cmd.
      This uses the Docker socket directly; no subprocess is spawned.
   c. If the image is not available locally (not pulled, not built), ImageInspect
      returns a not-found error → vaka up errors:
      "Error: service <name>: image <image> is not available locally and no
       entrypoint/command is declared in docker-compose.yaml. Either pull the
       image first or add entrypoint: to the service definition."

5a. For each service, compute the capability delta:
   - Parse the service's existing cap_add/cap_drop declarations from docker-compose.yaml
     to determine what capabilities the service already has.
   - vaka always needs CAP_NET_ADMIN (for nft). If NET_ADMIN is not already in cap_add,
     add it to the override and record it in the delta.
   - If dropCaps is explicitly set in vaka.yaml for this service, use that value and skip
     auto-computation. Document this: "runtime.dropCaps in vaka.yaml overrides the
     auto-computed delta. Use this only if you need to drop additional capabilities
     beyond what vaka adds, or if your compose setup differs from Docker defaults."
   - Write the final dropCaps list back into the per-service policy before serialising
     in step 4.

6. Build override YAML in memory via `pkg/compose` — constructs a typed Go struct
   representing the override and marshals it to YAML with `yaml.Marshal()`.
   No template is used; the struct is the source of truth for the override shape:

   secrets:
     vaka_codex_conf:
       environment: "VAKA_CODEX_CONF"
     vaka_llm_gateway_conf:
       environment: "VAKA_LLM_GATEWAY_CONF"

   services:
     codex:
       entrypoint: ["vaka-init", "--"]
       command: ["claude", "--dangerously-skip-permissions"]
       cap_add:
         - NET_ADMIN               # delta caps added by vaka for this service
       secrets:
         - source: vaka_codex_conf
           target: vaka.yaml
     llm-gateway:
       entrypoint: ["vaka-init", "--"]
       command: ["/usr/local/bin/litellm", "--config", "/etc/litellm.yaml"]
       cap_add:
         - NET_ADMIN
       secrets:
         - source: vaka_llm_gateway_conf
           target: vaka.yaml

   The override does NOT emit cap_drop: ALL. It only adds what vaka needs.
   The original docker-compose.yaml's cap_drop/cap_add declarations are preserved
   unchanged — Compose merges the two files.

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

Runs steps 1–2 (parse and validate `vaka.yaml`), then generates the nft ruleset for the named service via `pkg/nft` and prints it to stdout. No docker interaction. DNS names and service names in `to:` are not resolved — they are left as inline comments so the output is still readable and auditable:

```nft
# unresolved: llm-gateway
ip daddr { /* llm-gateway */ } tcp dport { 443 } accept
```

Useful for auditing the exact ruleset that will be applied inside the container before launch.

### 6.5 `vaka validate`

Runs steps 1–2 (parse and validate `vaka.yaml`) only. No docker interaction. Prints a per-service summary on success:

```
$ vaka validate
✓ codex        — 3 accept rules, 1 drop rule,  defaultAction: reject
✓ llm-gateway  — 1 accept rule,  0 drop rules, defaultAction: reject
```

Exits non-zero on any validation failure with a precise error message pointing to the offending field:

```
Error: services.codex.network.egress.accept[1].proto: unknown value "udpp" (expected tcp, udp, icmp, icmpv6)
```

---

## 7. `emsi/vaka-init` Base Image

The `docker/init/` directory builds and publishes `emsi/vaka-init` to Docker Hub. It is a `scratch`-based image containing exactly two binaries: the statically-linked `nft` from `emsi/nft-static` and the statically-linked `vaka-init` built from this repo.

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

### Including vaka-init in a harness image

Any harness image that will be used with `vaka up` must include the `vaka-init` and `nft` binaries. Copy them in from `emsi/vaka-init` using a multi-stage build:

```dockerfile
FROM emsi/vaka-init:latest AS vaka

FROM ubuntu:24.04
# ... harness setup ...
COPY --from=vaka /opt/vaka/bin/vaka-init /usr/local/sbin/vaka-init
COPY --from=vaka /opt/vaka/bin/nft       /usr/local/sbin/nft
```

The `ENTRYPOINT` in the image remains the harness's own entry point. The `vaka` CLI injects `vaka-init` transparently at `vaka up` time by rewriting `entrypoint:` and `command:` in the compose override. No changes to the harness image `ENTRYPOINT` are required or expected. The container carries only two vaka binaries; no other vaka code runs inside the container.

---

## 8. Capability Model

### 8.1 Default Docker container capabilities

A default Docker container (no `cap_drop`/`cap_add` in compose) starts with the following capabilities in both the effective (E) and bounding (B) sets:

```
cap_chown, cap_dac_override, cap_fowner, cap_fsetid, cap_kill,
cap_setgid, cap_setuid, cap_setpcap, cap_net_bind_service,
cap_net_raw, cap_sys_chroot, cap_mknod, cap_audit_write, cap_setfcap
```

Ambient (A) and inheritable (I) sets are empty. `cap_net_admin` is **not** in the default set.

Key capabilities relevant to vaka-init:
- `CAP_SETPCAP` — already present; required for `prctl(PR_CAPBSET_DROP, ...)`
- `CAP_SETUID` — already present; required for `setresuid()` when switching from uid 0
- `CAP_SETGID` — already present; required for `setresgid()` when switching from gid 0
- `CAP_NET_ADMIN` — **absent** by default; required for nft; added by vaka CLI

### 8.2 What vaka CLI adds

The vaka CLI only adds the capabilities that vaka-init actually needs but are not already present. In the common case (no `cap_drop: ALL` in the original compose):

- **`NET_ADMIN`** is the only cap added to the override's `cap_add` list.

`SETPCAP`, `SETUID`, and `SETGID` are already in the default Docker cap set and do not need to be added.

### 8.3 Delta-based dropCaps

The vaka CLI computes the **capability delta**: the set of capabilities it added to the override that were not already declared in the original `docker-compose.yaml`. This delta is written as `dropCaps` into the per-container policy injected via Docker secret.

In the common case: `dropCaps: [NET_ADMIN]`.

vaka-init then drops exactly those capabilities from all five sets (I → A → B → E/P) before executing the harness. The result: `NET_ADMIN` is gone from every set and cannot be regained by any binary the harness executes.

The remaining default Docker capabilities (`SETPCAP`, `SETUID`, `SETGID`, etc.) are cleared from E and P automatically by the Linux kernel's **UID fixup** when `setresuid(uid, uid, uid)` transitions all UIDs from 0 to nonzero (see §5.2 step 6). No manual clearing of those caps is needed.

**User override:** If `runtime.dropCaps` is explicitly set in `vaka.yaml`, that value takes precedence over the auto-computed delta. Use this only when the compose setup differs from Docker defaults or when additional caps need to be dropped. This is documented behaviour — the auto-computed value is always logged at `vaka up` time so the operator can see what would have been used.

### 8.4 Edge case: `cap_drop: ALL` in the original compose

If the operator's `docker-compose.yaml` specifies `cap_drop: ALL` (with or without subsequent `cap_add` entries), then `SETPCAP`, `SETUID`, and `SETGID` may be absent from the effective set. In that scenario:

- `prctl(PR_CAPBSET_DROP, NET_ADMIN)` fails at step 5c (EPERM — no `SETPCAP` in E)
- `setresuid`/`setresgid` fail at step 6 (EPERM — no `SETUID`/`SETGID` in E)

**This case is not handled in v1alpha1.** vaka-init will detect the failure, emit a clear error message, and exit (fail-closed). The operator must either remove `cap_drop: ALL` from the compose file or explicitly add the required caps back via `cap_add`. Automatic detection and injection of these caps by vaka CLI is planned for a future version.

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
| vaka CLI adds only `NET_ADMIN` (common case) | Default Docker caps already include `SETPCAP`, `SETUID`, `SETGID`; adding only what is actually missing keeps the cap footprint minimal and auditable |
| Delta-based `dropCaps` auto-computed by vaka CLI | vaka is responsible for what it adds; computing the delta means vaka-init cleans up exactly its own additions without touching caps the operator intentionally set |
| `NET_ADMIN` dropped from all five sets post-nft | Removing from B prevents any execve in the harness tree from regaining it; removing from E/P makes it immediately inactive; kernel UID fixup clears remaining caps on UID transition |
| Strict YAML parsing (unknown fields = error) | Typos in config keys would silently have no effect; hard errors prevent misconfiguration |
| Fail-closed on any init error | A partial security posture is worse than no startup; harness does not run if init fails |
| nft ruleset via Go `text/template` | nft syntax is a custom DSL with no Go marshaler; template keeps formatting readable and auditable; rule expansion logic stays in Go; embedded via `embed.FS` |
| compose override via `yaml.Marshal()` | The override has a well-defined Go struct shape; marshaling from structs is safer than a YAML template (no indentation fragility, no drift between template and types) |
| `defaultAction` defaults to `reject` | Secure by default; operators must explicitly choose `accept` and are warned when they do |
| `block_metadata: false` by default | Opt-in avoids surprising behaviour on non-cloud deployments; operators in cloud environments should enable it explicitly |
| Hard error on `network_mode: host` | Sharing the host network namespace defeats container egress isolation entirely and risks applying nft rules to the host; this must never be allowed silently |
| nft application is atomic | `nft -f` commits the full ruleset in a single kernel transaction — no intermediate half-loaded state is possible |
