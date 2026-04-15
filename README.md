# vaka  `v0.1.0`

vaka is a secure container layer that controls which network endpoints your containers are allowed to reach. Write a short policy file describing what outbound connections each service needs, run `vaka up` instead of `docker compose up`, and a kernel-level firewall is applied inside each container before its application process starts. Everything not on the allowlist is blocked.

It works without modifying your `docker-compose.yaml`, without writing any files to disk on the host, and without changing your container images.

---

## Why vaka?

Containers share the host's network stack by default. A running container can reach any IP address the host can reach: your internal services, cloud metadata endpoints, package registries, and the open internet. Container runtimes provide coarse controls (network namespaces, published ports) but nothing that lets you say "this specific service may only call these specific endpoints, and nothing else."

vaka fills that gap with nftables: a kernel firewall applied inside each container's own network namespace, configured per service, and enforced before the application binary runs. The firewall is loaded by the kernel itself. The application process cannot disable or modify it.

### Use cases

**AI agentic harnesses** are the primary motivation. Agentic AI systems run tools, write code, call external APIs, and browse the web. Limiting their network access to the services they legitimately need reduces the blast radius when a model misbehaves, is prompted to exfiltrate data, or starts calling unexpected endpoints.

**Untrusted vendor software** is a common need in enterprise environments. Third-party monitoring agents, analytics platforms, security tools, and SaaS connectors often have opaque network behaviour. Running them under vaka lets you verify that "phone home" traffic goes only where the vendor claims.

**Build and CI environments** are a supply-chain risk when they can reach your production secrets managers or internal APIs. Restricting build containers to package registries and artifact stores only closes an entire class of attack.

**Dev and staging isolation** prevents development containers from accidentally reaching production endpoints. A misconfigured environment variable should not be able to hit a live database.

**Regulatory compliance** requirements such as PCI-DSS, HIPAA, and SOC 2 mandate network segmentation. vaka provides auditable, declarative egress rules that map directly to those controls.

**Data processing pipelines** that ingest or transform sensitive data should be able to egress only to your data warehouse and logging endpoint. vaka enforces that boundary with a one-page policy file.

**Suspicious binary analysis** becomes safer when you run an untrusted binary under a deny-all or allow-specific-IPs policy. The binary cannot reach any destination outside the allowlist, no matter what it tries.

**Third-party plugins and extensions** running in your infrastructure get their own egress policy regardless of what libraries they bundle internally.

---

## How it works

### How vaka starts your containers

vaka reads your policy and compose files, generates a compose override in memory, and pipes it to `docker compose` via stdin. The per-service policy is delivered as a Docker secret — nothing is written to disk on the host.

```mermaid
flowchart LR
    vf["vaka.yaml"] --> cli["vaka up"]
    cf["docker-compose.yaml"] --> cli
    cli -- "compose override<br/>piped via stdin<br/>no files on disk" --> dc["docker compose"]
    dc -- "policy as<br/>Docker secret<br/>+ NET_ADMIN cap" --> c["container"]
```

### What vaka-init does at container startup

vaka-init runs as the container entrypoint before the application. It sets up the firewall, drops the capabilities vaka added (or an explicit `dropCaps` list if configured), optionally switches identity, and then hands off to the original binary.

```mermaid
flowchart TB
    s(["container starts"]) --> i["vaka-init reads policy<br/>from Docker secret"]
    i --> f["resolves hostnames,<br/>loads nftables egress rules"]
    f --> d["drops capabilities vaka added<br/>(or explicit dropCaps list if set)"]
    d --> u["switches UID / GID<br/>(if runAs is configured)"]
    u --> e["execve — replaces itself<br/>with the original entrypoint"]
    e --> a(["application runs<br/>under enforced egress policy"])
```

### No files written to disk

vaka never writes the policy to disk on the host. The per-service policy is base64-encoded and passed to `docker compose` as an environment variable. Docker turns that into a secret mounted on a kernel `tmpfs` inside the container at `/run/secrets/vaka.yaml`, invisible to the host filesystem.

The compose override that rewrites entrypoints and adds the secret is piped to `docker compose` via stdin using the `-f -` flag. Nothing is written to `/tmp`, the working directory, or anywhere else.

### Inside the container: vaka-init

`vaka-init` runs as the container entrypoint before the application. It executes these steps in order and exits with an error if any of them fails:

| Step | Action |
|------|--------|
| 1 | Parse `/run/secrets/vaka.yaml` (unknown fields are errors) |
| 2 | Resolve `dns: {}` rules and hostnames in `to:` lists to IP addresses |
| 3 | Apply nftables ruleset atomically via `nft -f /dev/stdin` |
| 4 | Drop capabilities: auto-computed set vaka added, or the explicit `dropCaps` list if set |
| 5 | `setresgid` / `setresuid` if `runAs` is configured |
| 6 | `execve` the original application entrypoint |

vaka-init is fail-closed. The application cannot start until the firewall is loaded. If the policy is malformed, the container exits before the application runs.

### What the nftables ruleset looks like

vaka generates an `inet` family table covering both IPv4 and IPv6, with an `output` hook chain. A typical ruleset:

```nft
table inet vaka {
  chain egress {
    type filter hook output priority 0;
    policy accept;

    # implicit invariants
    ct state established,related accept
    oif "lo" accept

    # metadata endpoint block (block_metadata: true)
    ip  daddr { 169.254.169.254, 100.100.100.200 } drop
    ip6 daddr { fd00:ec2::254, fd20:ce::254 } drop

    # explicit accept rules
    ip daddr { 93.184.216.34 } tcp dport { 443 } accept

    # default action
    reject
  }
}
```

Established connections are always accepted so in-flight requests are not dropped mid-session. Loopback is always allowed. Rules are evaluated in order: drop, then reject, then accept, then the default action.

---

## Getting started

### Install the vaka CLI

Download the binary for your platform from the [releases page](https://github.com/infrasecture/vaka/releases) and place it on your `PATH`:

```bash
# Linux amd64
curl -fsSL https://github.com/infrasecture/vaka/releases/download/v0.1.0/vaka-linux-amd64 -o vaka
chmod +x vaka
sudo mv vaka /usr/local/bin/vaka
```

Or build from the repository: see [Building and releasing](#building-and-releasing).

### Add vaka-init to your container image

`vaka-init` and `nft` are distributed together as a single Docker image (`emsi/vaka-init`). Both binaries are fully static and run on any Linux base including Alpine, Ubuntu, Debian, Fedora, and scratch.

**Option A: copy into your image at build time (recommended)**

```dockerfile
FROM emsi/vaka-init:0.1.0 AS vaka

FROM ubuntu:24.04
# ... rest of your image ...
COPY --from=vaka /opt/vaka/bin/vaka-init /usr/local/sbin/vaka-init
COPY --from=vaka /opt/vaka/bin/nft       /usr/local/sbin/nft
```

**Option B: bind-mount from the host at runtime**

Use this when you cannot modify the image, such as with vendor-supplied or third-party containers.

First, extract the binaries from the image to your current directory:

```bash
id=$(docker create emsi/vaka-init:0.1.0)
docker cp "$id:/opt/vaka/bin/vaka-init" ./
docker cp "$id:/opt/vaka/bin/nft"       ./
docker rm "$id"
```

Then bind-mount them in your `docker-compose.yaml`:

```yaml
services:
  myapp:
    image: vendor/myapp:latest
    volumes:
      - ./vaka-init:/usr/local/sbin/vaka-init:ro
      - ./nft:/usr/local/sbin/nft:ro
```

---

## Usage

### Write a vaka.yaml

Create `vaka.yaml` in the same directory as `docker-compose.yaml`. Each key under `services` must match a service name in your compose file.

```yaml
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  llm-gateway:
    network:
      egress:
        defaultAction: reject
        block_metadata: true
        accept:
          - dns: {}                        # allow DNS to resolv.conf servers
          - proto: tcp
            to: [api.openai.com]
            ports: [443]
          - proto: tcp
            to: [api.anthropic.com]
            ports: [443]
    runtime:
      runAs:
        uid: 1000
        gid: 1000
```

### Run

Use `vaka up` wherever you would use `docker compose up`. All compose flags are passed through:

```bash
vaka up               # start with policy enforcement
vaka up --build -d    # build images first, then start detached
vaka run llm-gateway bash    # policy is enforced for run too
```

Commands that do not need policy injection are forwarded to `docker compose` verbatim:

```bash
vaka logs -f llm-gateway
vaka exec llm-gateway bash
vaka down --volumes
vaka ps
```

vaka is a drop-in replacement for `docker compose`. Compose global flags work identically:

```bash
vaka -f prod.yaml up --build -d
vaka --vaka-file policies/prod.yaml -f prod.yaml up
```

### Validate before deploying

Check that `vaka.yaml` is valid before running containers:

```bash
vaka validate                                # policy only
vaka validate --compose docker-compose.yaml  # cross-check service names and network_mode
```

### Preview the firewall rules for a service

```bash
vaka show llm-gateway
```

Prints the nft ruleset that would be loaded inside the container. Hostnames are shown as comments rather than being resolved, so this works without network access.

---

## Configuration reference

### Full schema

```yaml
apiVersion: vaka.dev/v1alpha1
kind: ServicePolicy
services:
  <service-name>:
    network:
      egress:
        defaultAction: reject    # accept | reject | drop  (default: reject)
        block_metadata: false    # drops 169.254.169.254, 100.100.100.200, fd00:ec2::254, fd20:ce::254
        accept: [<rule>, ...]
        reject: [<rule>, ...]
        drop:   [<rule>, ...]
    runtime:
      dropCaps: [NET_RAW, SYS_ADMIN]
      runAs:
        uid: 1000
        gid: 1000
```

`<service-name>` must match a service name in `docker-compose.yaml`.

### Rule types

**DNS shorthand** permits UDP and TCP port 53 to the servers listed in `/etc/resolv.conf`:

```yaml
- dns: {}
```

To specify DNS servers explicitly, useful in minimal images that have no `/etc/resolv.conf`:

```yaml
- dns:
    servers: [1.1.1.1, 8.8.8.8]
```

**Address and port rule** permits or blocks traffic to specific hosts and ports:

```yaml
- proto: tcp
  to:
    - api.example.com     # hostname resolved at container start
    - 10.0.0.0/8          # CIDR
    - 192.168.1.1         # literal IP
  ports:
    - 443
    - "8080-8090"         # port range
```

`proto` is required when `ports` are specified. Valid protocols: `tcp`, `udp`, `icmp`, `icmpv6`.

**Protocol-only rule** matches all traffic of a given protocol regardless of destination:

```yaml
- proto: udp
```

**ICMP type filter** matches a specific ICMP message type:

```yaml
- proto: icmp
  type: echo-request    # or a numeric code: type: 8
```

Named ICMP types: `echo-request`, `echo-reply`, `destination-unreachable`, `time-exceeded`, `redirect`, `parameter-problem`, `timestamp-request`, `timestamp-reply`.

Named ICMPv6 types: `nd-neighbor-solicit`, `nd-neighbor-advert`, `nd-router-solicit`, `nd-router-advert`, `mld-listener-query`, `mld-listener-report`.

### defaultAction

| Value | Behaviour |
|-------|-----------|
| `reject` | Unmatched packets receive a TCP RST or ICMP port-unreachable. The application sees a connection refused immediately. **(default)** |
| `drop` | Unmatched packets are silently discarded. The application waits until it times out. |
| `accept` | Unmatched packets are allowed through. Use the `drop` or `reject` lists to block specific destinations. Emits a warning. |

### block_metadata

When set to `true`, drops all traffic to cloud instance metadata endpoints before any other rules are evaluated:

| Address | Provider |
|---------|----------|
| `169.254.169.254/32` | AWS, GCP, Azure, DigitalOcean, Hetzner, OCI, Linode |
| `100.100.100.200/32` | Alibaba Cloud |
| `fd00:ec2::254/128` | AWS IPv6 IMDS (Nitro instances) |
| `fd20:ce::254/128` | GCP IPv6 IMDS (IPv6-only instances) |

Recommended for any container that should not have access to cloud credentials through IMDS.

### runtime.dropCaps

Controls which Linux capabilities are dropped after the firewall is applied, before `execve`. Both short-form names (`NET_RAW`) and prefixed names (`CAP_NET_RAW`) are accepted.

**Automatic behavior (no `dropCaps` in vaka.yaml):** `vaka up` adds `CAP_NET_ADMIN` to the compose override so that `vaka-init` can load the nftables ruleset. It tracks which capabilities it added on top of what the service already declares in `cap_add`. After the firewall is loaded, vaka-init drops exactly those added capabilities — for most services that means `NET_ADMIN` is dropped automatically, and the application process never holds it. If `NET_ADMIN` was already present in the service's `cap_add` before vaka ran, vaka treats that as intentional and leaves it in place.

**Explicit override (`dropCaps` set in vaka.yaml):** The list you provide replaces the auto-computed set entirely — it is not additive. You are responsible for the complete drop list. If you want `NET_ADMIN` dropped but also need to drop other capabilities, include all of them:

```yaml
runtime:
  dropCaps: [NET_ADMIN, NET_RAW, SYS_PTRACE]
```

### runtime.runAs

Switches UID and GID before execve-ing the application. `setresgid` is called before `setresuid` as required by POSIX. The kernel clears Effective and Permitted capabilities automatically on the transition from UID 0 to a non-zero UID.

---

## CLI reference

### `vaka up`

Validates `vaka.yaml`, generates a compose override in memory, and starts the stack with the override piped via stdin. All `docker compose up` flags are passed through.

```
vaka [--vaka-file vaka.yaml] up [compose-flags...]
```

### `vaka run`

Same injection path as `up` but for `docker compose run`.

```
vaka [--vaka-file vaka.yaml] run [compose-flags...] <service> [command...]
```

### `vaka validate`

Parses and validates `vaka.yaml`. Pass `--compose` to also cross-check service names against `docker-compose.yaml` and enforce that no service uses `network_mode: host`.

```
vaka validate [-f vaka.yaml] [--compose docker-compose.yaml]
```

### `vaka show <service>`

Prints the nft ruleset that would be loaded inside the named service's container. Hostnames in `to:` lists appear as comments rather than resolved IPs, so no network access is needed.

```
vaka show [-f vaka.yaml] <service>
```

### Passthrough commands

Any subcommand not listed above is forwarded verbatim to `docker compose`:

```bash
vaka logs -f llm-gateway       # docker compose logs -f llm-gateway
vaka exec llm-gateway bash     # docker compose exec llm-gateway bash
vaka down --volumes            # docker compose down --volumes
```

### Global flags

| Flag | Default | Description |
|------|---------|-------------|
| `--vaka-file` | `vaka.yaml` | Path to the vaka policy file |

All Docker Compose global flags (`-f`, `-p`, `--profile`, `--env-file`, `--project-directory`, `--context` and others) are passed through unchanged.

---

## Security model

vaka loads egress firewall rules into the kernel nftables subsystem before the application binary starts. The application cannot bypass or modify those rules without `CAP_NET_ADMIN`. vaka adds `NET_ADMIN` to the container so that vaka-init can load the nftables rules, then drops it before execve — so the running application does not hold that capability. If `NET_ADMIN` was already present in the service's `cap_add` before vaka ran, vaka treats that as intentional and leaves it in place. If `runtime.dropCaps` is set explicitly, that list replaces the auto-computed set and takes full effect instead.

Ingress traffic is not modified. Containers remain reachable on their published ports.

Inter-container traffic is also subject to egress policy. The rules are installed on the kernel OUTPUT hook inside each container's network namespace, so all packets leaving that namespace — including those destined for other containers on the same bridge — are evaluated. If service A needs to reach service B, service B's hostname or IP range must appear in service A's egress allowlist. Docker's internal DNS resolves compose service names to container IPs, so `to: [db]` in the policy works as expected.

Containers using `network_mode: host` share the host network namespace and cannot be isolated per-container. vaka rejects these at validation time.

vaka is designed to contain well-behaved but potentially over-reaching software: AI agents calling unexpected APIs, vendor tools phoning home, build environments with broad access. It is not designed to contain actively hostile root-level processes that might exploit kernel vulnerabilities to bypass nftables. For that threat model, enforce isolation at the hypervisor or host network layer instead.

---

## Building and releasing

### Requirements

- Docker with [buildx](https://docs.docker.com/build/buildx/) (no local Go toolchain required; the build script uses a golang container)
- Linux or macOS host

### Build

```bash
./build.sh
```

This builds fully static binaries for `linux/amd64` and `linux/arm64` inside a `golang:1.25-alpine` container and produces arch-specific `emsi/vaka-init` Docker images. Output lands in `./dist/`:

```
dist/vaka-linux-amd64
dist/vaka-linux-arm64
dist/vaka-init-linux-amd64
dist/vaka-init-linux-arm64
dist/nft-linux-amd64
dist/nft-linux-arm64
```

The script verifies that each binary is statically linked and that the Docker image contains exactly `nft` and `vaka-init`.

To build for a single architecture:

```bash
ARCHS=amd64 ./build.sh
```

### Install the CLI binary

```bash
sudo install -m 0755 dist/vaka-linux-amd64 /usr/local/bin/vaka
```

### Build Linux packages

```bash
./build.sh --packages
```

Produces `.deb` and `.rpm` packages in `./dist/` using [nfpm](https://nfpm.goreleaser.com/) run in Docker (`ghcr.io/goreleaser/nfpm:latest`). Install with:

```bash
# Debian / Ubuntu
sudo dpkg -i dist/vaka_0.1.0_amd64.deb

# Fedora / RHEL / CentOS
sudo rpm -i dist/vaka-0.1.0-1.x86_64.rpm
```

### Versioning and releasing

Versions come from git tags. Tag a release and push:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Then run `./build.sh`. The script reads `git describe --tags --always` and stamps the version into:

- the `vaka version` command output (`vaka v0.1.0`)
- the `vaka-init` binary metadata (visible via `strings`)
- the `emsi/vaka-init` OCI image label (`org.opencontainers.image.version`)
- `.deb` and `.rpm` package metadata

Publish the Docker images. The build produces arch-specific staging tags locally; `--push` pushes those and assembles multi-arch manifest lists in the registry:

**Single host with QEMU** (builds and pushes all arches in one step):

```bash
sudo apt-get install -y qemu-user-static   # Debian/Ubuntu — one-time setup
./build.sh --push
```

**Separate native hosts** (no QEMU needed — run each on its matching hardware):

```bash
ARCHS=amd64 ./build.sh --push   # on amd64 host
ARCHS=arm64 ./build.sh --push   # on arm64 host
./build.sh --manifest            # on any host after both arch pushes complete
```

Both workflows produce the same registry result:

```
emsi/nft-static:1.1.6          ← multi-arch manifest list
emsi/nft-static:latest
emsi/vaka-init:v0.1.0          ← multi-arch manifest list
emsi/vaka-init:latest
```

### Run the test suite

Tests run locally and require Go 1.25:

```bash
go test ./...
```

Or inside Docker:

```bash
docker run --rm \
    -v "$(pwd):/src:ro" -w /src \
    -e GOWORK=off \
    golang:1.25-alpine \
    go test ./...
```
