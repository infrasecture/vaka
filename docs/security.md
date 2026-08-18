# Security Model

vaka enforces outbound network policy inside each managed container's Linux network namespace.

A managed container is a Compose service explicitly listed under `services` in
`vaka.yaml`. Compose services omitted from `vaka.yaml` receive no Vaka override
and retain their existing egress behavior; enforcement is opt-in per service.

Vaka protects against accidental or implicit policy bypasses in Vaka-controlled
workflows. The host operator, Compose configuration, service image, Docker
daemon, kernel, and direct Docker access are trusted. Vaka is not a sandbox for
a hostile container or operator.

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

1. vaka computes and adds any capability missing for startup, including
   temporary `SETPCAP`, `SETUID`, and `SETGID` when required.
2. `vaka-init` loads the firewall.
3. `vaka-init` begins dropping the capabilities vaka added, retaining only
   identity-transition capabilities until they have been used.
4. `vaka-init` switches to the service's effective Compose/image user, drops
   any deferred capabilities, and verifies the final capability sets.
5. The application starts.

Capabilities deliberately supplied by Compose, including privileged mode and
`cap_add: ALL`, are preserved unless you provide an explicit
`runtime.dropCaps` list. Vaka warns when a managed service is privileged,
requests all capabilities, retains `SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE`, or
other high-impact kernel capabilities, has Docker or container-runtime daemon
access, shares a PID namespace, weakens the default seccomp/AppArmor/SELinux
profiles, or uses `init: true`. Vaka also warns about custom seccomp and
AppArmor profiles because it cannot verify that they still prevent
mount-namespace manipulation. With `init: true`, Docker's trusted init shim
remains outside Vaka's capability-drop path. Vaka also warns when an unmanaged
service joins a managed service's network or PID namespace, including
`container:` joins to an explicit managed `container_name`. Enforcement is
best-effort across these deliberately weakened boundaries.

Container-creation warnings use the fully merged Compose model. For
`vaka exec` and operations that resume an existing container (`start`, `restart`,
and `unpause`), Vaka instead derives these warnings from the selected
container's live `HostConfig`, realized mounts, applied AppArmor profile, and
immutable injected policy. Editing the Compose file without recreating the
container therefore cannot hide a retained live privilege or produce a warning
for a setting that is not present on the container being operated on.

The verified runtime image subpath is mounted read-only directly at `/vaka`.
Making the mountpoint a direct child of `/` prevents a root workload from
replacing it by renaming a writable parent directory. It does not make the
mount immutable to a process that has acquired mount-namespace control.
Before creating a managed container, Vaka inspects the exact pinned service
image without executing it. `/vaka` must be absent or a real directory, and
every service, image-volume, and Docker-supplied mount destination is resolved
through the image's symlinks. Vaka rejects any effective destination that
overlaps `/vaka` or another reserved path. This prevents runc from following an
untrusted image symlink and installing an apparently unrelated writable mount
over the runtime. Vaka also evaluates Docker's parent-before-child mount order.
An externally populated mount may not be an ancestor of a later mount, device,
masked path, or read-only path: content in that parent could introduce a
symlink after image inspection and redirect the later operation into `/vaka`.
Docker-owned nesting such as `/dev` with `/dev/pts`, and freshly created tmpfs
parents, remain supported because they cannot supply attacker-controlled
symlinks during mount setup. Path validation preserves whitespace exactly as
part of the Linux pathname.

On startup and in the exec/healthcheck trampoline, `vaka-init` independently
requires `/vaka` to be a non-symlink directory, the single literal `/vaka`
entry in `/proc/self/mountinfo`, and read-only with no nested mounts. Host-side
validation also checks the lexical path in existing containers before exec or
resume, and reconstructs the live effective mount topology before any operation
that could reapply it. These checks fail closed; operators who intentionally
need conflicting mount topology must use raw Docker or Compose outside Vaka's
managed path.
In particular, Vaka emits a precise warning for the combination of
`seccomp=unconfined` with `apparmor=unconfined` or disabled SELinux labeling;
on susceptible hosts that combination can let a root workload create a mount
namespace and overmount `/vaka`. Privileged mode and retained `SYS_ADMIN` are
reported separately as stronger bypass conditions.

When Compose leaves `init` unset, Vaka emits `init: false` for the managed
service so a daemon-wide default cannot silently insert a capable parent ahead
of `vaka-init`. An explicit `init: true` remains preserved and produces the
best-effort warning described above.

Docker stores the startup user and capabilities on the container, so processes
created later do not inherit the application's dropped process state. Vaka
therefore wraps healthchecks and `vaka exec` commands with the verified
read-only `vaka-init` runtime. The trampoline first verifies that the kernel
nftables table exists, then drops policy capabilities from all five capability
sets and verifies their absence,
runs as the service's effective Compose/image user or the identity explicitly
selected with `exec --user`, and starts the command. A service intended to run
as root remains root, but without Vaka-added capabilities. The trampoline does
not repeat startup ownership changes.

The table query is a fail-closed readiness gate: Docker can accept an exec or
begin a healthcheck after the container is running but before startup policy
installation has completed. It confirms that the `inet vaka` table exists; it
does not compare the complete ruleset with policy and is not a security audit.
It performs no DNS resolution, rule generation, or ownership changes.

After upgrading from an affected Vaka release, refresh the runtime and recreate
every managed project:

```bash
vaka doctor --fix
vaka up --force-recreate
```

Run the second command from each project's directory (and with its usual Vaka
and Compose options). Restarting, starting, or unpausing an existing container
is not sufficient because Docker retains that container's original user,
capabilities, healthcheck, and runtime mount. Fixed Vaka releases block these
resume operations when they detect an older or mutable managed runtime, or a
current policy-managed service that was created without Vaka. Ordinary
unmanaged services retain native Compose behavior; stop, down, and remove
remain available for containment, subject to executable-hook validation.

Compose `post_start` and `pre_stop` hooks and `develop.watch` actions `sync`,
`sync+restart`, `sync+exec`, and `rebuild` are currently rejected on managed
services.
Compose implements watched-file deletion with an internal container exec, even
for plain `sync`. `run --entrypoint`, unsafe mounts over Vaka runtime/policy
paths, protected-label overrides, `up --no-recreate`, and `watch --no-up` are
also rejected; the reuse restriction applies to `create --no-recreate` too.
These restrictions fail closed rather than guessing how a new
process or command-line override interacts with the security boundary.
`down --remove-orphans` also checks managed one-off containers and rejects a
current `pre_stop` hook when the container is in a state where Compose can
execute it outside the trampoline.

## What It Does Not Enforce

- Inbound traffic and published ports.
- Host-level firewall policy.
- Network isolation for `network_mode: host`, `service:...`, or `container:...`.
- Filesystem secrecy inside mounted directories.
- Protection from Docker, kernel, or hypervisor escapes.
- A full hostile-code sandbox for root-level adversaries.
- Commands run directly through Docker or Docker Compose instead of Vaka. Anyone
  with Docker-daemon access already controls the container security boundary.

For hostile code, use stronger isolation such as VMs, separate hosts, or host-network firewall controls.

## Recipe Registry Supply Chain

Published recipes are downloaded from an HTTPS (or explicitly local
`file://`) registry and accepted only when the tarball matches the index's
sha256 digest. Extraction rejects path traversal, escaping symlinks, hardlinks,
special files, oversized payloads, and Vaka's reserved state paths. The staged
manifest, Compose model, and policy are validated before an install or update
is committed. A recipe is still arbitrary code that the user may later run;
the digest establishes what was fetched, not that the code is benign.

Git preview registries are opt-in development sources. `registry add-git` and
`registry refresh` resolve the configured ref to one full commit and package
only objects tracked by that commit. Vaka reads Git trees and blobs directly,
without `git archive`, so `.gitattributes` export rules and user archive config
cannot change the candidate. It does not check out the repository, run its
hooks, initialize submodules, or read ignored/untracked worktree files. The
generated artifact is validated through the same hardened path and its digest
plus source commit are recorded. Ordinary `get` and catalog commands use the
generated cache and do not contact Git; branch movement takes effect only after
an explicit successful refresh.

The temporary Git object store for one refresh and each registry's artifact
cache each have a 512 MiB aggregate limit in addition to per-recipe limits.
Preview cache directories and files are private to the user (`0700`/`0600`).
Atomic index replacement plus a 24-hour artifact grace period lets readers
finish across rapid refreshes; a failed refresh keeps the last complete
snapshot. Removing a registry also removes its cached data.

Preview refs are mutable and do not have the release trust semantics of a
published registry. They must be named explicitly as `registry/recipe`, cannot
participate in unqualified release resolution, and should be installed into a
separate disposable directory. Git credentials come from the user's normal
credential helper or SSH setup; Vaka rejects unsafe transports and embedded
HTTPS credentials and disables Git credential terminal prompts. SSH host-key
handling remains governed by the user's SSH configuration.

## Docker Desktop And macOS

The `vaka` CLI can run on macOS. Enforcement still happens in Linux containers inside Docker Desktop's Linux VM. The macOS host does not need native nftables support.

The same caveats apply as on Linux: the container backend must be Linux, and a
managed service must own its network namespace. Vaka rejects the shared network
modes `host`, `service:...`, and `container:...`.

## No Host Policy File

vaka never writes the generated per-service policy to disk on the host. The
policy and its semantic revision are encoded into the immutable container
configuration. `vaka-init` verifies that revision on every startup,
healthcheck, and managed exec before trusting the policy.

The Compose override is streamed through an inherited `/dev/fd/3` pipe instead of being written to `/tmp` or the project directory.

The runtime image is validated by its exact bundle-version label, resolved on
the selected Docker target, and mounted by immutable local image ID. The mount
is read-only. `vaka-init` also requires the generated policy's exact runtime
bundle version before loading nftables, so an incorrectly tagged or stale
runtime fails closed.

Service images are also inspected and executed by exact local image ID. Vaka
consumes requested build or forced-pull operations before inspection, rejects
protected image-declared volumes, explicitly disables an absent healthcheck,
and prevents Compose from refreshing the image again during container creation.

Normal Docker state still exists where Docker keeps it: containers, images,
volumes, and Docker-managed metadata. Vaka's current delivery path does not
create a helper container or helper volume; conservative cleanup exists only
for volumes left by older releases.

Compose verbs are classified explicitly. Verbs known to create containers use
the full policy override, `exec` uses its dedicated trampoline path, and unknown
future verbs are rejected until reviewed. This prevents Compose feature growth
from silently introducing an unprotected process-creation path.

## Kernel And nftables Compatibility

`vaka-init` uses the Linux kernel nftables subsystem through the `nft` binary. Very old kernels may not support all nftables features used by vaka, such as `inet` family tables or `icmpx`.

Pre-5.x kernels are uncommon on currently supported mainstream distributions;
test the generated ruleset on older kernels before relying on enforcement.
