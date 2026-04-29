# CLI Reference

`vaka` has native commands and Compose-proxy commands.

## Command Paths

| Path | Commands | Behavior |
|------|----------|----------|
| Native | `validate`, `show-nft`, `doctor`, `show-compose`, `version`, `help`, `completion` | Handled by vaka itself. |
| Full render | `up`, `run`, `create` | Validate policy, generate the full Compose override, inject secrets and entrypoint changes, then call Compose. |
| Reference | Other Compose commands such as `logs`, `exec`, `ps`, `pull`, `volumes`, `down`, `stop`, `kill`, `rm` | Call Compose with a minimal `__vaka-init` overlay so helper resources remain visible. |

## `vaka up`

```bash
vaka [--vaka-file=<path>] [--vaka-init-present] [compose-global-flags...] up [compose-flags...]
```

Starts the stack with policy enforcement. All `docker compose up` flags are passed through.

## `vaka run`

```bash
vaka [--vaka-file=<path>] [--vaka-init-present] [compose-global-flags...] run [compose-flags...] <service> [command...]
```

Runs a one-off service with the same injection path as `up`.

## `vaka create`

```bash
vaka [--vaka-file=<path>] [--vaka-init-present] [compose-global-flags...] create [compose-flags...]
```

Creates containers with the `vaka-init` entrypoint override but does not start application services.

## Teardown And Reference Commands

```bash
vaka down
vaka down --volumes
vaka stop
vaka kill
vaka rm
vaka volumes
vaka logs -f app
vaka exec app sh
vaka ps
```

`down`, `stop`, `kill`, and `rm` include the `__vaka-init` helper unless you pass `--vaka-init-present` before the subcommand.

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
vaka show-nft [-f vaka.yaml] [--compose docker-compose.yaml] <service>
```

Prints the nftables ruleset that would be loaded for a service.

Current behavior: hostnames in `to:` lists are printed as comments instead of being resolved. This keeps the command offline and avoids using the host resolver when the container may resolve differently. The wording and optional resolved preview are tracked in [issue #57](https://github.com/infrasecture/vaka/issues/57).

## `vaka show-compose`

```bash
vaka [--vaka-file=<path>] [--vaka-init-present] [compose-global-flags...] show-compose [--build] [-o override.yaml]
```

Prints the generated Compose override used by `up`, `run`, and `create`.

Notes:

- `--vaka-file` and `--vaka-init-present` must appear before `show-compose`.
- Compose global flags must appear before `show-compose`.
- After `show-compose`, only `--build` and `-o/--output` are accepted.
- Encoded per-service `VAKA_<SERVICE>_CONF` values are not printed.

## `vaka version`

```bash
vaka version
```

Prints the version stamped at build time.

## Vaka Wrapper Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--vaka-file=<path>` | `vaka.yaml` | Policy file for injection/proxy paths. |
| `--vaka-init-present` | off | Skip automatic helper injection; assume helper binaries already exist at `/opt/vaka/sbin/` in service images. |

Vaka wrapper flags must appear before the subcommand. Value-taking vaka flags require `=` form.

Correct:

```bash
vaka --vaka-file=policies/prod.yaml up
```

Incorrect:

```bash
vaka up --vaka-file policies/prod.yaml
```

Compose globals such as `-f`, `-p`, `--profile`, `--env-file`, and `--project-directory` are passed through. Docker top-level globals such as `--context`, `-c`, `--host`, `-H`, `--config`, TLS flags, `--debug`, and `--log-level` are rejected; use Docker environment or context configuration instead.
