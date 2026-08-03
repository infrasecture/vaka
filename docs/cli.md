# CLI Reference

`vaka` has three command forms:

```bash
vaka [--vaka-file=<path>] [--vaka-init-present] <native-command> [flags...]
vaka [--vaka-file=<path>] [--vaka-init-present] <shorthand> [args...]
vaka [--vaka-file=<path>] [--vaka-init-present] compose [compose-global-flags...] <compose-command> [args...]
```

## Command Paths

| Path | Commands | Behavior |
|------|----------|----------|
| Native | `validate`, `show-nft`, `doctor`, `show-compose`, `version`, `help`, `completion` | Handled by vaka itself. |
| Recipes | `get`, `search`, `recipes`, `registry` | Registry consumption: fetch and update recipes, browse catalogs. Never runs docker. |
| Compose full render | `up`, `run`, `create`, `scale`, `watch`, and unclassified future verbs | Validate policy, generate the full Compose override, inject secrets and entrypoint changes, then call Compose. |
| Compose reference | Known non-creating commands such as `logs`, `exec`, `ps`, `pull`, `down`, `stop`, `kill`, `rm`, `volumes` | Call Compose through the same FD-injected path with metadata only; no policy evaluation or helper service. `pull` first ensures the Vaka runtime image. |
| Compose metadata | `vaka compose version`, `vaka compose ls`, and help forms | Proxied to Compose without any overlay; they work outside project directories. |
| Shorthands | `up`, `down`, `start`, `stop`, `run`, `exec`, `logs`, `ps` | Permanent top-level aliases: `vaka up ...` executes exactly like `vaka compose up ...`. |

The shorthand set is intentionally small and fixed. Every other Compose
command requires the `compose` namespace, which keeps the top-level namespace
free for future vaka commands:

```bash
vaka pull          # error: use `vaka compose pull`
vaka compose pull  # works
```

## `vaka compose`

```bash
vaka [vaka-flags...] compose [compose-global-flags...] <command> [args...]
```

The full docker compose surface with vaka policy injection. Compose global
flags (`-f/--file`, `-p/--project-name`, `--profile`, `--env-file`,
`--project-directory`, ...) are accepted only here, between `compose` and the
Compose command:

```bash
vaka compose -f compose.yml up -d
vaka compose --profile dev up
vaka compose --project-directory srv logs -f app
```

Compose commands Vaka does not yet know are forwarded through the full policy
path. This fail-closed default keeps new Docker Compose subcommands working
without allowing a newly introduced container-creating verb to bypass policy.
Known non-creating commands use the lighter reference path. All
container-creating policy paths require Docker Engine 28.0.0+ with an effective
API of 1.48+ and Docker Compose 2.35.0+ for read-only image mounts.
Engine 29.0.x/29.1.x must be paired with Compose earlier than 5.1.0 because of
an upstream image-mount path-length bug; `vaka doctor` reports this combination
with an upgrade recommendation.

## Compose Help

Help for the Compose namespace and its shorthands comes from the installed
Docker Compose plugin:

```bash
vaka compose --help
vaka compose <command> --help
vaka <shorthand> --help
vaka help compose
vaka help <shorthand>
```

These forms run Docker Compose help without loading a project or injecting a
vaka override, so the available commands and options match the installed
Compose version. Running bare `vaka compose` also displays Docker Compose
usage.

## `vaka up` (shorthand)

```bash
vaka [vaka-flags...] up [compose-up-flags...]
```

Starts the stack with policy enforcement; identical to `vaka compose up`.
All `docker compose up` flags after the command are passed through.

## `vaka run` (shorthand)

```bash
vaka [vaka-flags...] run [compose-run-flags...] <service> [command...]
```

Runs a one-off service with the same injection path as `up`.

## `vaka compose create`

```bash
vaka [vaka-flags...] compose create [compose-flags...]
```

Creates containers with the `vaka-init` entrypoint override but does not start
application services. `create` has no top-level shorthand.

## Teardown And Reference Shorthands

```bash
vaka down
vaka down --volumes
vaka stop
vaka logs -f app
vaka exec app sh
vaka ps
vaka compose kill
vaka compose rm
```

Reference commands do not load `vaka.yaml`. They inject only `x-vaka` runtime
metadata. `down` additionally performs conservative one-time cleanup if it
finds an anonymous helper volume created by an older Vaka release; named and
in-use volumes are never removed. `pull` resolves and validates the versioned
Vaka runtime image before Docker Compose pulls the project service images.

## `vaka validate`

```bash
vaka validate [-f vaka.yaml] [--compose docker-compose.yaml]
```

Parses and validates `vaka.yaml`. Repeat `--compose` for multiple compose files.

## `vaka doctor`

```bash
vaka doctor [--fix]
```

Checks Docker CLI availability, Docker Engine 28.0.0+, effective client API
1.48+, Docker Compose 2.35.0+, Linux-container backend, rootful/rootless mode,
image-mount support, runtime image identity, and Docker context information.

`--fix` pulls or repairs the required
`emsi/vaka-init:runtime-<runtime-version>` image. The runtime version is
independent of the CLI version, so this also works for CLI development builds.

## `vaka show-nft <service>`

```bash
vaka show-nft [-f vaka.yaml] <service>
```

Prints the nftables ruleset that would be loaded for a service.

Pass `--compose <path>` only when you explicitly want compose cross-checks against that file set. For normal project use, prefer the shorter form above; passing only `docker-compose.yaml` can ignore `docker-compose.override.yaml` and other compose files you normally rely on.

Current behavior: hostnames in `to:` lists are printed as comments instead of being resolved. This keeps the command offline and avoids using the host resolver when the container may resolve differently. The wording and optional resolved preview are tracked in [issue #57](https://github.com/infrasecture/vaka/issues/57).

## `vaka show-compose`

```bash
vaka [--vaka-file=<path>] [--vaka-init-present] show-compose [-f compose.yml ...] [--project-directory <dir>] [-p <name>] [--profile <name> ...] [--env-file <path> ...] [--build] [-o override.yaml]
```

Prints the generated Compose override used by full-render Compose commands,
including the exact runtime image ID mount and policy revision labels.

Notes:

- `--vaka-file` and `--vaka-init-present` must appear before `show-compose`.
- Compose inputs are command-local flags after `show-compose` (unlike
  `vaka compose`, where they precede the Compose command).
- Encoded per-service `VAKA_<SERVICE>_CONF` values are not printed.

## `vaka version`

```bash
vaka version
```

Prints the version stamped at build time.

## Recipe Commands

Recipes are ready-to-run, security-hardened compose projects published by
registries (see the
[registry design](design/recipes-registry.md) for the format and security
model). The official registry is
`https://github.com/infrasecture/vaka-registry`.

### `vaka get`

```bash
vaka get <[registry/]name>[@version] [dir]   # install/update ./<name> (or dir)
vaka get                                      # update the recipe in the current dir
vaka get @<version>                           # update the current dir to a version
vaka get @<version> <dir>                     # update <dir> to a version
```

Fetches a recipe into `dir` (default `./<name>`), verified against the
registry index's sha256 digest, and prints the locally computed egress
policy summary, risk flags, and any required-but-unset environment
variables. Versions are exact SemVer; omitted means newest.

`vaka get` is the single install-and-update verb (like `docker pull`). It
decides which by the target directory: an existing directory that carries a
`.vaka-recipe.lock` is **updated in place**; otherwise a new one is installed.

**Common scenarios**

```bash
# Install a recipe (into ./codex):
vaka get codex
vaka get codex@0.3.1            # a specific version
vaka get codex myproj/codex     # into a chosen directory

# Update a recipe you already have:
cd codex && vaka get            # update this recipe to the newest version
cd codex && vaka get @0.3.1     # update this recipe to an exact version
vaka get @0.3.1 codex           # same, without cd (update ./codex)
```

The bare `vaka get` and `vaka get @<version>` forms carry no name: they read
the recipe's name and registry from the directory's `.vaka-recipe.lock` and
resolve against that same registry, so you never repeat the name and the
result is never ambiguous. If the directory is not a vaka recipe, `vaka get`
says so and does nothing.

`vaka get` only changes recipe files, never docker images or containers — run
`vaka up` afterward to apply an updated recipe (Compose pulls a newly-pinned
image and recreates affected services).

Update safety:

- Updates only ever replace pristine files. A locally modified tracked file
  that the new version still ships rejects the whole update — vaka does not
  merge, and there is no override flag. Keep customizations in untracked
  files: `.env`, `compose.override.yaml`, or files the recipe does not ship.
- User-created files are never overwritten. A new recipe file colliding with
  yours is skipped and recorded as a deviation; the render commands print a
  notice while deviations exist.
- Interrupted installs leave nothing behind; interrupted updates converge by
  re-running `vaka get` (journaled two-phase commit).
- `vaka get` never adopts an existing non-recipe directory and never runs
  docker.

### `vaka search` / `vaka recipes`

```bash
vaka search [term]                  # search names, descriptions, tags
vaka recipes list                   # full catalogs (newest version each)
vaka recipes info <name>[@version]  # catalog metadata of one recipe
```

Catalog commands read published indexes or locally generated Git preview
indexes. Published indexes use ETag revalidation; a cache younger than 15
minutes is served without network. Git preview indexes change only on an
explicit registry refresh. Catalog commands never scan the local filesystem.
The policy block they display is advisory — `vaka get` always recomputes it
locally.

### `vaka registry`

```bash
vaka registry list                     # configured registries + cache age
vaka registry add <name> <index-url>   # add a published registry
vaka registry add-git <name> <git-url> --ref <branch>
vaka registry remove <name>            # remove config + cache (alias: rm)
vaka registry refresh [name]           # refresh index(es) or Git preview(s)
```

`add`, `add-git`, and `remove` edit `registries.yaml`; removal also deletes that
registry's cached index and preview artifacts. `refresh` force-revalidates
published indexes and advances Git preview refs (or just the named registry).
The path is shown by `list`; an absent file means the official published
registry only:

```yaml
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
registries:
  - name: official
    url: https://infrasecture.github.io/vaka-registry/index.yaml
  - name: preview
    git:
      url: https://github.com/infrasecture/vaka-registry.git
      ref: fix/codex-candidate
```

Registry names match `[a-z0-9-]+`. Published index URLs must be `https://`
(`file://` is allowed for local/air-gapped registries). An unqualified recipe
name resolves only when it is unique across published registries; otherwise
vaka lists the qualified candidates (`registry/name`).

**Testing an unpublished recipe branch**

```bash
vaka registry add-git preview \
  https://github.com/infrasecture/vaka-registry.git \
  --ref fix/codex-candidate
vaka get preview/codex@0.2.3 codex-preview

# After more commits are pushed to the branch:
vaka registry refresh preview
cd codex-preview && vaka get
```

Git registries are development preview channels, not release authorities:

- `add-git` resolves the ref immediately to one full commit ID, validates all
  top-level recipes, and builds a local index plus digest-addressed artifacts.
  A shorthand ref names a branch; use `refs/tags/...` or another full
  `refs/...` name when needed.
- Only files tracked by that commit are packaged. Vaka reads tree/blob objects
  directly and does not use `git archive`, so `.gitattributes` export rules and
  user `tar.*` settings cannot change the artifact. It does not check out or
  recursively scan a worktree, execute repository hooks, or initialize
  submodules. Ignored and untracked files cannot enter a preview artifact.
- Normal `get`, search, info, list, and completion never contact Git. The
  configured branch advances only through `registry refresh`, which resolves
  it to a new commit and atomically replaces the generated index.
- Preview recipes must always be qualified (`preview/codex`). They cannot
  hijack or make an ordinary unqualified release reference ambiguous.
- The preview index records both the artifact digest and source commit. The
  lock records the commit that supplied installed bytes. Editing a recipe on
  the branch may intentionally change its digest without changing its
  candidate SemVer; after refresh, `vaka get` applies that update. An unrelated
  commit leaves the content-derived recipe digest unchanged.
- A failed refresh preserves the last complete snapshot and returns an error.
  Existing installs remain usable against that cached snapshot. Cached
  artifacts have an aggregate 512 MiB limit; old unreferenced artifacts remain
  for a 24-hour reader grace period and are reclaimed by later refreshes.
- Install previews into a disposable, separate target such as
  `codex-preview`. Its lock remains bound to the `preview` registry alias; an
  official release should be installed from the official registry rather than
  silently changing the preview's provenance.

Git preview refresh requires the `git` executable. HTTPS, SSH, scp-style SSH,
and absolute `file://` repository URLs are accepted; plain HTTP, `git://`, and
custom remote helpers are rejected. Configure private-repository credentials
through the normal Git credential helper or SSH agent. Vaka disables Git
credential terminal prompts so an unattended credential lookup fails instead
of hanging; SSH host-key behavior remains controlled by the user's SSH setup.

## `vaka completion`

Generate a completion script for Bash, Zsh, Fish, or PowerShell:

```bash
# Bash
source <(vaka completion bash)

# Zsh
source <(vaka completion zsh)

# Fish
vaka completion fish | source

# PowerShell
vaka completion powershell | Out-String | Invoke-Expression
```

Save the generated script in the shell's completion directory to load it in
future sessions. Bash scripts embed the path of the `vaka` executable that
generated them; regenerate a saved script if the executable moves.

Vaka completes its native command tree. `vaka get` and `vaka recipes info`
complete recipe names from the cached registry indexes (qualified as
`registry/name` when more than one published registry is configured, and always
qualified for Git previews). `vaka registry remove` and `vaka registry refresh`
complete configured registry names. Completion reads only the local cache,
never the network. Compose-backed commands and shorthands intentionally provide
no vaka-generated argument candidates and disable filename fallback; use Docker
Compose documentation for their flags and arguments.

## Vaka Wrapper Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--vaka-file=<path>` | `vaka.yaml` | Policy file for injection/proxy paths. |
| `--vaka-init-present` | off | Skip the runtime image mount; assume compatible helper binaries already exist at `/opt/vaka/sbin/` in service images. |

Vaka wrapper flags must appear before the command. Value-taking vaka flags require `=` form.

Correct:

```bash
vaka --vaka-file=policies/prod.yaml up
vaka --vaka-file=policies/prod.yaml compose -f compose.yml up
```

Incorrect:

```bash
vaka up --vaka-file policies/prod.yaml
vaka compose --vaka-file=policies/prod.yaml up
```

Docker top-level globals such as `--context`, `-c`, `--host`, `-H`, `--config`, TLS flags, `--debug`, and `--log-level` are rejected; use Docker environment or context configuration instead.
