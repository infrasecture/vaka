# Troubleshooting

Start with:

```bash
vaka doctor
```

Then retry fixable checks:

```bash
vaka doctor --fix
```

## Known Issues

### Docker Engine 29.0/29.1 Image-Mount Path Length

Docker Engine 29.0.x and 29.1.x contain an image-mount path-length bug. Without
Vaka's compatibility handling, creating a managed container can fail with an
error similar to:

```text
Error response from daemon: mkdir /var/lib/docker/image/overlay2/layerdb/mounts/<long-hex-name>: file name too long
```

Those Engine versions encode the container ID, image-mount source, and mount
destination into one filesystem name. A complete `sha256:` image ID makes that
name exceed the filesystem limit. The long value is hexadecimal encoding, not
repeated or corrupted image data.

Vaka uses a 40-hex-character immutable image-ID prefix on the affected Engines.
Docker resolves that prefix against the local image store and rejects it if it
is ambiguous; Vaka retains the complete image ID in container labels and
override metadata. Compose 5.1.0 and newer expand the prefix back to the complete
ID, so they cannot be combined with the affected Engine versions.

| Docker Engine | Docker Compose | Vaka behavior |
| --- | --- | --- |
| 28.0.0 through 28.x | 2.35.0 or newer | Supported; complete image ID |
| 29.0.x or 29.1.x | 2.35.0 through 5.0.x | Supported; compact image-ID prefix |
| 29.0.x or 29.1.x | 5.1.0 or newer | Rejected before Compose runs |
| 29.2.0 or newer | 2.35.0 or newer | Supported; complete image ID |

Prefer upgrading Docker Engine to 29.2.0 or newer. If that is not immediately
possible, keep Docker Compose between 2.35.0 and 5.0.x while using Engine 29.0
or 29.1. Run `vaka doctor` to verify the selected Engine and Compose pairing.

The Engine-side path construction was corrected by
[moby/moby#51827](https://github.com/moby/moby/pull/51827). The relevant Compose
source expansion was introduced by
[docker/compose#13549](https://github.com/docker/compose/pull/13549).

## Runtime Image Missing Or Incompatible

`vaka doctor --fix` pulls or refreshes the required
`emsi/vaka-init:runtime-vX.Y.Z` image and validates its runtime-version label.

The runtime version is independent of `vaka version`, so development CLI builds
do not require a special `:dev` runtime image.

## Exec Says The Egress Policy Is Not Installed Yet

Vaka refuses to start a managed exec or healthcheck until the kernel `inet vaka`
table exists. Immediately after container creation, retry once startup
has completed. If the error persists, inspect the service logs: startup
`vaka-init` may have failed before installing policy. The check only establishes
readiness; it does not compare the complete live ruleset with `vaka.yaml`.

If Vaka instead reports an older or mutable runtime, refresh the required image
and recreate the container; restart or unpause cannot change stored container
capabilities, healthchecks, or mounts:

```bash
vaka doctor --fix
vaka up --force-recreate
```

## Docker API Is Pinned Too Old

If `doctor` reports an old Docker client API while the Engine itself is current,
check `DOCKER_API_VERSION`. Unset it, or set it to 1.48 or newer. Vaka checks the
effective client API because Compose inherits the same override.

## Baked Runtime Mode Is Rejected

`--vaka-init-present` and `agent.vaka.init: present` are rejected for managed
services. Remove them and let Vaka mount its verified runtime image read-only.
For air-gapped targets, preload the exact runtime image reported by
`vaka doctor` instead of baking the binaries into the service image.

## Legacy Helper Volume During Upgrade

Vaka releases using image mounts automatically migrate anonymous `/opt/vaka`
volumes created by older releases. Detection requires the historical helper
service plus Docker's anonymous-volume marker; a lookalike named volume is left
untouched. A partial `up <service>` can report:

```text
vaka: retained 1 legacy runtime volume(s) still used by existing containers
```

This is expected. Recreate the remaining policy-managed services normally; Vaka
removes the helper and anonymous volume after the final consumer moves. `vaka
down` also captures and removes the old volume safely. Cleanup never uses force
and never removes named volumes. It rechecks Docker's anonymous-volume marker
immediately before deletion, so `--renew-anon-volumes`/`-V` is neither required
nor recommended for this migration.

## Compose Reports Image Mount Is Experimental

Current Compose versions may print `Image mount is an experimental feature`
while creating a service. This is a Compose status message; the mount remains
read-only and is supported by Vaka's minimum Engine and Compose versions.

## Shared `network_mode`

Vaka rejects managed services using `network_mode: host`, `service:...`, or
`container:...`. Those modes share the host or another container's network
namespace, so Vaka cannot install an independently owned per-container egress
policy.

Use a normal bridge network or move enforcement to a host/VM firewall layer.

## Build-Only Services

If a managed service uses `build:` with no `image:`, Vaka builds it before
inspection when the Compose-generated project image is missing. A previously
built image is reused unless `--build` requests a refresh.

Fix by adding an image name:

```yaml
services:
  app:
    build: .
    image: app:local
```

Adding an explicit image name remains useful for predictable external tooling,
but is not required for Vaka's image inspection.

## DNS Or Hostname Surprises

Hostnames in policy are resolved inside the container when it starts. This is intentional: Docker embedded DNS, split-horizon networks, and CDN/anycast endpoints may resolve differently inside the container than on the host.

If an endpoint changes, restart the service so `vaka-init` resolves it again.

## Exact Image Inspection And Execution

Vaka consumes `--build` and `--pull=always` before inspecting managed service
images. It then runs each managed service by the exact local image ID it
inspected, with pulling disabled in the generated override. This covers
inherited entrypoints, users, healthchecks, shells, and image-declared volumes.

For example, this refreshes `app:latest` first and then pins the resulting ID:

```bash
# local app:latest is image A; the registry now serves image B
vaka --vaka-pull=missing compose up --pull=always
```

If the exact image is concurrently removed after inspection, creation fails
closed instead of falling back to a mutable tag. Digest-pinned image references
remain recommended for reproducible projects and recipes.

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
