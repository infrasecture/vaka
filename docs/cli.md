# CLI Reference

Vaka has native commands, recipe commands, fixed Compose shorthands, and the
complete Docker Compose surface under `vaka compose`.

```text
vaka [vaka-flags...] <native-or-recipe-command> [args...]
vaka [vaka-flags...] <shorthand> [compose-command-args...]
vaka [vaka-flags...] compose [compose-global-flags...] <command> [args...]
```

Vaka flags must appear before the command. Compose global flags belong after
`compose` and before the Compose command.

## Command Groups

| Group | Commands | Behavior |
| --- | --- | --- |
| Native | `validate`, `doctor`, `show-nft`, `show-compose`, `version`, `completion` | Handled by Vaka. |
| Recipes | `search`, `recipes`, `get`, `registry` | Browse, install, and update recipe files. Never runs Docker. |
| Shorthands | `up`, `down`, `start`, `stop`, `run`, `exec`, `logs`, `ps` | Equivalent to `vaka compose <command>`. |
| Compose | `vaka compose ...` | Exposes the full installed Docker Compose command surface. |

The shorthand set is intentionally fixed. Other Compose commands require the
namespace:

```bash
vaka pull          # error: use the compose namespace
vaka compose pull  # correct
```

A shorthand and its `vaka compose` form use the same Vaka dispatch and security
checks. Running `docker compose` directly bypasses Vaka and retains raw Compose
behavior. Vaka rejects Compose `--dry-run` because secured exec and image
preparation do not map safely to Compose's global dry-run mode; use raw
`docker compose --dry-run` when that deliberate bypass is intended.

## Recipe Commands

A recipe is a versioned Compose project with a Vaka policy and optional support
files. Registries may contain arbitrary code; inspect the catalog metadata,
risk summary, and installed files before running a recipe. See the
[registry design](design/recipes-registry.md) and [security model](security.md)
for the trust boundaries.

### Discover Recipes

```bash
vaka search [term]
vaka recipes list
vaka recipes info <[registry/]name>[@version]
```

- `search` matches recipe names, descriptions, and tags.
- `recipes list` shows the highest indexed version of every recipe.
- `recipes info` shows one recipe's metadata, indexed versions, digest,
  environment inputs, and advisory policy summary.

These commands browse registry catalogs and never scan local installations.
Published catalogs are revalidated after a 15-minute freshness window; Git
preview catalogs change only after an explicit refresh. Browsing is
best-effort: unavailable registries are omitted with a warning, and stale
caches may be used with a warning. The policy summary is what the registry
reports. After installing or updating files, `vaka get` recomputes it locally.

### `vaka get`

```text
vaka get <[registry/]name>[@version] [dir]
vaka get
vaka get @<version>
vaka get @<version> <dir>
```

With a recipe name, the target defaults to `./<name>`. An omitted version
selects the highest indexed version; `@version` accepts exact `X.Y.Z` SemVer
only. Version constraints are not supported.

A fresh install requires an existing parent directory and a target path that
does not exist. Vaka never adopts an existing ordinary directory. If the target
contains a matching `.vaka-recipe.lock`, the named form updates that managed
installation instead.

The bare forms read the registry and recipe identity from the target's lock:

```bash
cd codex
vaka get  # update to the highest indexed version
```

Use the `@<version>` forms in the synopsis to select an exact version, with or
without changing directories.

Before committing an install or update, Vaka verifies the artifact digest,
extracts it through a confined filesystem root, validates the manifest identity
and minimum Vaka version, loads the shipped Compose model, validates the policy
against its services, and builds the provenance lock. It never starts Docker or
runs recipe scripts. After the file transaction commits, Vaka reports a locally
computed policy and risk summary. Failure to produce that advisory report is a
warning, not a rollback.

`get` also reports required environment variables declared by the selected
catalog entry that are absent from the current process. It does not load a
recipe's `.env` file for this check.

Fresh installs are staged beside the target and published with a no-replace
rename, so a failed install does not expose a partial target. Updates are
journaled and recover by rerunning `vaka get` after interruption.

Update behavior is deliberately conservative:

| Local path state | Update result |
| --- | --- |
| Tracked and unchanged | Replaced or removed to match the new recipe. |
| Tracked, locally deleted, still shipped | Restored from the new recipe. |
| Tracked, locally edited, still shipped | Entire update is refused. |
| Tracked, locally edited, dropped upstream | Kept as an untracked deviation. |
| Untracked path is identical to a new recipe file | Adopted as a tracked recipe file. |
| Different untracked path collides with a new recipe file | User path is kept and recorded as a deviation. |
| Other untracked path | Left untouched. |

Render commands print a notice while an installation has recorded deviations.
There is no force or merge flag.

Unqualified recipe names resolve only when unique across the configured
published registries. For `get`, Vaka must revalidate every published registry
needed to prove that uniqueness; unreachable or stale state makes the command
fail. A qualified `registry/name` reference reads only that registry and may use
its stale published cache with a warning. Git preview recipes are always
qualified.

`vaka get` changes files only. Follow the installed README to start the recipe
or apply an update.

### `vaka registry`

```bash
vaka registry list
vaka registry add <name> <index-url>
vaka registry add-git <name> <repository-url> --ref <branch-or-ref>
vaka registry refresh [name]
vaka registry remove <name>
```

With no user configuration, Vaka uses its built-in `official` published
registry. The first add or remove writes `registries.yaml`; `vaka registry list`
shows its location and each cache state.

Registry names match `[a-z0-9-]+`. Published index URLs must use HTTPS;
`file://` URLs are allowed for local or air-gapped registries. Adding a registry
does not let it silently take over an unqualified name: ambiguous names must be
qualified.

`refresh` force-revalidates published indexes. Removing a registry deletes its
configuration and cache.

#### Git Preview Registries

Git previews package the committed state of a branch name or full Git ref for
development testing. Tags use the full `refs/tags/<tag>` form:

```bash
vaka registry add-git preview \
  https://github.com/your-org/vaka-recipes.git \
  --ref main
vaka recipes info preview/example
vaka get preview/example example-preview

# After the configured ref advances:
vaka registry refresh preview
cd example-preview
vaka get
```

`add-git` resolves the ref to one immutable commit immediately. Vaka reads
tracked Git trees and blobs directly, packages top-level recipe directories,
and validates them through the normal install path. It does not check out the
repository, run hooks, initialize submodules, or include ignored and untracked
worktree files.

The configured ref advances only through `registry refresh`. A failed refresh
keeps the last complete snapshot. Preview recipes must remain qualified and
should be installed into separate disposable targets.

HTTPS, SSH, scp-style SSH, and absolute `file://` Git URLs are supported. Plain
HTTP, `git://`, custom remote helpers, and embedded HTTPS credentials are
rejected. Authentication comes from the normal Git credential helper or SSH
agent; credential terminal prompts are disabled.

## Compose Commands

### `vaka compose`

```text
vaka [vaka-flags...] compose [compose-global-flags...] <command> [args...]
```

Compose global flags such as `-f`, `-p`, `--profile`, `--env-file`, and
`--project-directory` are accepted only between `compose` and its command:

```bash
vaka compose -f compose.yml up -d
vaka compose --profile dev up
vaka compose --project-directory srv logs -f app
```

When no explicit `--profile` is supplied, Vaka honors `COMPOSE_PROFILES` using
Compose's normal environment and `.env` resolution. Policy service names are
validated against all declared services, while rendering and image preparation
use only the active services selected by the command.

Commands that can create containers (`up`, `run`, `create`, `scale`, and
`watch`) validate policy and generate the complete Vaka override. Unknown
future Compose commands fail closed until their process-creation behavior is
reviewed in Vaka.

Before inspecting and pinning service images, Vaka applies the final requested
`--build`/`--pull` behavior and supported Compose-file `pull_policy` values to
the selected Vaka-managed services and their Compose dependencies. When a
mixed-project `up` or `create` must consume
`--build`, `--pull=always`, or `--pull=build`, Vaka also prepares only the
selected unmanaged services (and their Compose dependencies) so their native
result is preserved. For `run`, Vaka applies the same rule to the selected
target and its dependency graph unless `--no-deps` is enabled; when that entire
graph is unmanaged, the flags remain with Compose. Safe `--pull=missing`,
`if_not_present`, and `never` remain on the final Compose invocation. An absent
service `pull_policy` uses `--vaka-pull`; explicit Compose policies take
precedence. Invalid values and refresh options unsupported by the selected
command are rejected before preparation. Timed policies fail with guidance to
pull explicitly so Vaka does not inspect one image and execute another.

Known non-creating commands use a metadata-only reference override. Examples
include `logs`, `ps`, `pull`, `down`, `start`, `stop`, `kill`, and `rm`.
`exec` is different: Vaka inspects the selected live container, verifies its
exact service/runtime image identities and protected mounts, and executes the
trampoline against that container ID rather than resolving the service again.
It wraps commands for managed services with the immutable runtime trampoline,
which drops startup capabilities before running the command as the service's
effective Compose/image user, or as the identity explicitly selected with
`exec --user`. Unmanaged services retain normal Compose behavior.

For managed services, Vaka rejects `exec --privileged`; `run --entrypoint`,
`run --user`, capability or reserved-environment overrides, protected mount or
label overrides; `post_start` and `pre_stop` hooks; and Watch actions `sync`,
`sync+restart`, `sync+exec`, and `rebuild`. It also rejects
`up/create --no-recreate` and `watch --no-up`, and requires old, mutable, or
policy-managed-but-unlabeled containers to be recreated before Vaka-controlled
resume or exec operations.
For `restart`, the resume guard follows Compose's selected transitive
`restart: true` dependents unless `--no-deps` is enabled, and validates every
selected replica. Targeted `down` validates exactly the explicitly selected
services; untargeted `down` validates all active services. With
`--remove-orphans`, Vaka also validates managed one-off containers whose
current `pre_stop` hook Compose can execute. Fully unmanaged services retain
native Compose behavior.
`stop`, `down`, and `rm --stop` remain available for containment; executable
`pre_stop` hooks still receive the same validation. Watch actions without
those process or filesystem mutation surfaces remain available.

`vaka compose version`, `vaka compose ls`, bare `vaka compose`, and help forms
are proxied directly without loading a project.

Container-creating paths require the Docker versions listed in
[Installation](installation.md). See [Troubleshooting](troubleshooting.md) for
the Engine 29.0/29.1 and Compose 5.1 compatibility case.

### Compose Help

Help comes from the installed Docker Compose plugin, so flags match the local
Compose version:

```bash
vaka compose --help
vaka compose <command> --help
vaka <shorthand> --help
vaka help compose
vaka help <shorthand>
```

### Common Shorthands

```bash
vaka up --build -d
vaka run --rm app command
vaka ps
vaka logs -f app
vaka exec app sh
vaka stop
vaka down --volumes
```

`up` and `run` use full policy rendering, `exec` uses the guarded live-container
path, and the remaining examples use the reference path. `down` also performs
conservative cleanup when it finds the exact anonymous helper volume left by an
older Vaka release; it never removes named or in-use volumes.

## Native Commands

### `vaka validate`

```bash
vaka validate [-f vaka.yaml] [--compose compose.yaml]...
```

Parses and validates a host policy. Repeat `--compose` to check service names
against several Compose files. Omit it to validate policy syntax only.

### `vaka doctor`

```bash
vaka doctor [--fix]
```

Checks Docker availability, Engine and client API versions, Compose
compatibility, Linux-container mode, rootful or rootless operation, image-mount
support, runtime identity, and Docker context information. `--fix` pulls or
repairs the required versioned runtime image.

### `vaka show-nft`

```bash
vaka show-nft [-f vaka.yaml] [--compose compose.yaml]... <service>
```

Prints the nftables ruleset for one service without applying it. Hostnames in
`to:` rules remain comments rather than being resolved with the host resolver.

### `vaka show-compose`

```text
vaka [vaka-flags...] show-compose
  [-f compose.yml ...]
  [--project-directory <dir>]
  [-p <name>]
  [--profile <name> ...]
  [--env-file <path> ...]
  [--build]
  [-o override.yaml]
```

Prints the generated full-render override, including the resolved runtime image
mount and policy-revision labels. Per-service encoded policy values are never
printed. `--build` pre-builds eligible services before image metadata is
resolved. Compose inputs are flags of `show-compose`; Vaka flags still precede
the command.

### `vaka version`

```bash
vaka version
```

Prints the version stamped at build time.

### `vaka completion`

For Bash, Zsh, and Fish:

```bash
source <(vaka completion bash)
source <(vaka completion zsh)
vaka completion fish | source
```

For PowerShell:

```powershell
vaka completion powershell | Out-String | Invoke-Expression
```

Completion covers the native command tree, cached recipe names for `get` and
`recipes info`, and configured registry names for `remove` and `refresh`. It
uses local cache only and never contacts a registry. Compose arguments are left
to Docker Compose documentation.

## Vaka Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--vaka-file=<path>` | `vaka.yaml` | Select the host policy file. |
| `--vaka-init-present` | off | Deprecated compatibility flag; rejected when any service is managed because later execs require the verified read-only runtime mount. |
| `--vaka-pull=<policy>` | `missing-pinned` | Control whether Vaka pulls a missing service image when it must inspect image defaults. Values: `missing-pinned`, `missing`, `never`. |

`missing-pinned` pulls only missing digest-pinned service images.
`missing` also permits a missing tag-based image to be pulled. `never` makes
every missing image an error. Images already present are not refreshed by this
flag. On full-render commands, Compose `--pull` and `pull_policy` take
precedence and are applied before Vaka inspects the image.

Value-taking Vaka flags require `=` form and all Vaka flags precede the command:

```bash
vaka --vaka-file=policies/prod.yaml up
vaka --vaka-pull=missing compose -f compose.yml up
```

These placements are invalid:

```bash
vaka up --vaka-file policies/prod.yaml
vaka compose --vaka-pull=missing up
```

Docker top-level globals such as `--context`, `--host`, `--config`, TLS flags,
`--debug`, and `--log-level` are rejected. Select the Docker target through its
environment or configured context instead.
