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
| Compose full render | `vaka compose up`, `vaka compose run`, `vaka compose create` | Validate policy, generate the full Compose override, inject secrets and entrypoint changes, then call Compose. |
| Compose reference | Other `vaka compose` commands such as `logs`, `exec`, `ps`, `pull`, `down`, `stop`, `kill`, `rm` | Call Compose with a minimal `__vaka-init` overlay so helper resources remain visible. |
| Compose metadata | `vaka compose version`, `vaka compose ls` | Proxied to Compose without any overlay; they work outside project directories. |
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

Compose commands vaka does not know about are forwarded with the reference
overlay, so new docker compose subcommands keep working.

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

Reference commands include the `__vaka-init` helper unless you pass
`--vaka-init-present` before the command.

Use `vaka down --volumes` or `vaka up -V` after upgrading to refresh anonymous helper volumes and avoid `vakaVersion` mismatches.

## `vaka validate`

```bash
vaka validate [-f vaka.yaml] [--compose docker-compose.yaml]
```

Parses and validates `vaka.yaml`. Repeat `--compose` for multiple compose files.

## `vaka doctor`

```bash
vaka doctor [--fix]
```

Checks Docker CLI availability, daemon reachability, Compose v2 availability, Linux-container backend, helper image availability, and Docker context information.

`--fix` currently pulls the required `emsi/vaka-init:<vaka-version>` helper image when missing. Development builds with `version=dev` cannot be fixed this way.

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

Prints the generated Compose override used by `vaka compose up`, `run`, and
`create`.

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

Vaka completes its native command tree. Compose-backed commands and
shorthands intentionally provide no vaka-generated argument candidates and
disable filename fallback; use Docker Compose documentation for their flags
and arguments.

## Vaka Wrapper Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--vaka-file=<path>` | `vaka.yaml` | Policy file for injection/proxy paths. |
| `--vaka-init-present` | off | Skip automatic helper injection; assume helper binaries already exist at `/opt/vaka/sbin/` in service images. |

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

## Breaking Changes In 0.x

The compose namespace restructure changed the following pre-1.0 behavior:

- Compose global flags moved under `compose`: `vaka -f x.yml up` →
  `vaka compose -f x.yml up`.
- `vaka create` was demoted: use `vaka compose create`.
- Unknown top-level commands now error instead of being forwarded to
  docker compose; Compose verbs outside the shorthand set need
  `vaka compose <verb>`.
- `show-compose` takes compose inputs as flags after the command:
  `vaka -f x.yml show-compose` → `vaka show-compose -f x.yml`.
