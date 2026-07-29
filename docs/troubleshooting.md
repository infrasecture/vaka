# Troubleshooting

Start with:

```bash
vaka doctor
```

Then retry fixable checks:

```bash
vaka doctor --fix
```

## Helper Image Missing

`vaka doctor --fix` pulls `emsi/vaka-init:<vaka-version>`.

If you built a local development binary with version `dev`, there is no published `emsi/vaka-init:dev`. Build the helper image locally with `./build.sh` or use a stamped release binary.

## Version Mismatch After Upgrade

Existing containers may keep an older anonymous helper volume. Refresh it:

```bash
vaka down --volumes
vaka up
```

or renew anonymous volumes:

```bash
vaka up -V
```

## `network_mode: host`

vaka rejects services using `network_mode: host`. Those services share the host network namespace, so vaka cannot install a per-container egress policy.

Use a normal bridge network or move enforcement to a host/VM firewall layer.

## Build-Only Services

If a service uses `build:` with no `image:`, vaka may not be able to inspect the runtime entrypoint or user before build.

Fix by adding an image name:

```yaml
services:
  app:
    build: .
    image: app:local
```

or explicitly declare runtime metadata:

```yaml
services:
  app:
    build: .
    user: "1000:1000"
    entrypoint: ["/usr/local/bin/app"]
```

## DNS Or Hostname Surprises

Hostnames in policy are resolved inside the container when it starts. This is intentional: Docker embedded DNS, split-horizon networks, and CDN/anycast endpoints may resolve differently inside the container than on the host.

If an endpoint changes, restart the service so `vaka-init` resolves it again.

## Inspected Image May Differ From The Running Image

To build its egress override, vaka reads a service's default `ENTRYPOINT`/`CMD`/
`USER` from the image — but only when the Compose file leaves them unset. That
inspection happens **before** vaka hands off to `docker compose`, so if Compose
then pulls or rebuilds a *different* image, vaka can inject metadata from an image
that is not the one that actually runs.

This only happens with a **mutable tag** plus an explicit refresh:

```bash
# local app:latest is image A; the registry now serves image B
vaka --vaka-pull=missing compose up --pull=always
```

vaka inspects the local A (present, so `--vaka-pull=missing` does not refresh it),
while Compose's `--pull=always` fetches and runs B. If A and B differ in
`ENTRYPOINT`/`USER`, the wrapped command or restored user is wrong.

**Digest-pinned images are immune.** `repo@sha256:…` is immutable, so a forced
pull re-verifies the same digest — inspected == executed. Pinning is the
recommended practice and is what the recipe registry does.

Two ways to avoid it:

- **Pin the image by digest** (`image: repo@sha256:…`). Correct regardless of any
  `--pull` / `pull_policy` flags.
- **Declare `user:` and `entrypoint:` explicitly** on the service. vaka then never
  inspects the image — it wraps the *declared* entrypoint and applies it to
  whatever image Compose runs, so there is nothing stale to read. This also
  removes the "image not available locally" preflight for that service entirely.

Without a forced pull, or with either fix above, the inspected and executed
images match. This is a tracked known issue — see
[issue #85](https://github.com/infrasecture/vaka/issues/85).

## External DNS Fails On User-Defined Networks (Docker Engine < 28)

**Symptom.** A service can resolve sibling service names but not external
hostnames. External lookups fail with `SERVFAIL` or *"Temporary failure in name
resolution"* even though the policy allows DNS with `dns: {}`. A gateway that
must reach the internet (for example a LiteLLM sidecar resolving a model
provider) fails, while `curl http://other-service` still works.

**Cause.** On a user-defined network — the default for Compose — the container's
only resolver is Docker's embedded stub, `nameserver 127.0.0.11`. The stub
answers internal names locally, but for external names it forwards to the host's
upstream resolver. On **Docker Engine older than 28.0.0**, when that upstream is
a **non-loopback** address (a LAN/router DNS such as one handed out by DHCP),
Docker makes the forwarding query **from inside the container's network
namespace**. vaka's egress policy allows only `127.0.0.11:53`, not that upstream,
so the forwarded query is dropped. `dns: {}` expands to the nameservers listed in
`/etc/resolv.conf`, which on this topology is just the `127.0.0.11` stub — never
the hidden forward target.

This is environment-dependent: hosts whose resolver is a **loopback** address
(for example systemd-resolved at `127.0.0.53`) forward host-side even on older
Docker, so they are unaffected. That is why the same recipe can work on one
machine and fail on another.

**Fix — upgrade Docker Engine to 28.0.0 or newer.** From 28.0.0, Docker makes the
forwarding query for all host-configured upstreams from the **host** network
namespace, so it never traverses the container's netns and vaka does not
interfere. No policy or recipe change is needed. Egress enforcement is
unaffected: the container still cannot reach the resolver (or anything else)
directly — only Docker's own host-side forwarding is used.

```bash
docker version --format '{{.Server.Version}}'   # must be >= 28.0.0
```

Do **not** work around this by pinning a public resolver (`dns: [1.1.1.1]`): it
hardcodes a resolver that breaks when the host's DNS changes and, on a
user-defined network, it disables service-name resolution for that container.
Upgrade the Engine instead. The Engine behavior change is
[moby/moby#48290](https://github.com/moby/moby/pull/48290) — *"DNS nameservers read
from the host's `/etc/resolv.conf` are now always accessed from the host's network
namespace"* — first released in 28.0.0. See also
[issue #81](https://github.com/infrasecture/vaka/issues/81).

## Docker Context Or Remote Daemon

vaka follows the Docker CLI environment and active Docker context. Docker top-level flags such as `--context` and `--host` are not accepted as vaka arguments.

Use:

```bash
docker context use <name>
```

or environment variables such as:

```bash
DOCKER_CONTEXT=<name> vaka doctor
```

## Inspect Generated Output

Preview nftables rules:

```bash
vaka show-nft <service>
```

Preview the Compose override:

```bash
vaka show-compose
```

Write it to a file for inspection:

```bash
vaka show-compose -o /tmp/vaka-override.yaml
```

## Old Kernels

Very old Linux kernels may not support nftables features used by vaka. The failure should appear as an `nft` error before the app starts. See [issue #17](https://github.com/infrasecture/vaka/issues/17).
