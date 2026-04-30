# Security Model

vaka enforces outbound network policy inside each managed container's Linux network namespace.

## What It Enforces

- nftables rules are loaded before the application process starts.
- The application runs after the firewall is installed.
- Unmatched outbound traffic is rejected, dropped, or accepted according to `defaultAction`.
- Loopback and established/related traffic are allowed as invariants.
- Inter-container traffic is also evaluated because it leaves the source container's network namespace.
- Cloud metadata endpoints can be explicitly blocked with `block_metadata`.

If service A needs to reach service B, allow service B's Compose hostname or IP range in service A's policy.

## Capabilities

vaka needs `NET_ADMIN` temporarily to load nftables rules. The normal path is:

1. vaka adds the required capability for startup.
2. `vaka-init` loads the firewall.
3. `vaka-init` drops the capabilities vaka added.
4. `vaka-init` restores the service user when one is configured.
5. The application starts.

If the original Compose service already had `NET_ADMIN`, vaka treats that as intentional and leaves it in place unless you provide an explicit `runtime.dropCaps` list.

## What It Does Not Enforce

- Inbound traffic and published ports.
- Host-level firewall policy.
- Network isolation for `network_mode: host`.
- Filesystem secrecy inside mounted directories.
- Protection from Docker, kernel, or hypervisor escapes.
- A full hostile-code sandbox for root-level adversaries.

For hostile code, use stronger isolation such as VMs, separate hosts, or host-network firewall controls.

## Docker Desktop And macOS

The `vaka` CLI can run on macOS. Enforcement still happens in Linux containers inside Docker Desktop's Linux VM. The macOS host does not need native nftables support.

The same caveats apply as on Linux: the container backend must be Linux, and `network_mode: host` cannot be isolated per container.

## No Host Policy File

vaka never writes the generated per-service policy to disk on the host. The policy is encoded into environment passed to `docker compose`; Docker materializes it as a secret mounted inside the container at `/run/secrets/vaka.yaml` on tmpfs.

The Compose override is streamed through an inherited `/dev/fd/3` pipe instead of being written to `/tmp` or the project directory.

Normal Docker state still exists where Docker keeps it: containers, images, volumes, and Docker-managed metadata.

## Kernel And nftables Compatibility

`vaka-init` uses the Linux kernel nftables subsystem through the `nft` binary. Very old kernels may not support all nftables features used by vaka, such as `inet` family tables or `icmpx`.

Pre-5.x kernels are uncommon on currently supported mainstream distributions, but the known limitation is tracked in [issue #17](https://github.com/infrasecture/vaka/issues/17).
