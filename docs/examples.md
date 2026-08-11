# Examples

Maintained, runnable stacks are published as versioned recipes. Use Vaka's
catalog and `get` commands instead of copying an executable snapshot from this
repository.

## Official Recipe Catalogue

```bash
vaka search agent
vaka recipes list
vaka recipes info codex
```

`vaka recipes info` is the current source for a recipe's published version,
digest, minimum Vaka version, environment inputs, version history, policy
summary, and advisory risk flags. Catalog commands inspect registry state; they
do not inspect local recipe installations or run Docker.

## Codex With LiteLLM Gateway

The official Codex recipe runs Codex in one container and LiteLLM in a separate
local gateway container. The agent can reach only the gateway, while the gateway
has its own provider-specific egress policy.

### Install And Run

Install the current catalog version into a new `codex` directory:

```bash
vaka get codex
cd codex
./myCodex
```

`vaka get` verifies the artifact digest, validates the staged recipe, recomputes
its policy and risk summary, and writes a `.vaka-recipe.lock`. It changes recipe
files only and never starts Docker.

Codex must be launched with the installed recipe's `myCodex`, not bare
`vaka up` or `docker compose`. The launcher supplies host identity, authentication
profile selection, per-project state, and the profile-specific Compose and Vaka
files. When invoked from the recipe directory it creates or reuses a confined
named workspace.

For an existing project, invoke the installed launcher from that project's
directory:

```bash
cd /path/to/project
/path/to/codex/myCodex
```

First-run setup offers ChatGPT subscription login, an OpenAI API key, and the
available experimental profiles. The installed recipe README is authoritative
for profile requirements and operational commands.

### Check Status And Update

The catalog, installed files, and running stack have separate status surfaces:

```bash
vaka recipes info codex             # current published catalog metadata
(cd /path/to/codex && vaka get)      # check/update the installed recipe

cd /path/to/project
/path/to/codex/myCodex info          # resolved project and state
/path/to/codex/myCodex auth status   # selected authentication profile
/path/to/codex/myCodex ps            # running services
```

A bare `vaka get` inside an installed recipe uses its lock to resolve the same
registry and recipe. Updates preserve untracked state, refuse to overwrite
locally modified tracked files, and recover by rerunning `vaka get` after an
interruption. Follow the installed README after an update; for Codex, run
`/path/to/codex/myCodex up` from the same project directory so Compose applies
the updated stack files.

### `codex@0.3.1` Security Migration

Recipe versions through `0.3.0` exposed the LiteLLM administrator credential to
the Codex container. Release `0.3.1` introduced route-restricted agent
authentication, rotation of the affected key, and a fail-closed guard for
legacy containers.

After updating an affected installation, follow its README. When instructed,
run `myCodex down` from the same project directory, then rerun `myCodex`. Use
`vaka recipes info codex` rather than this historical notice to determine the
current catalog version.

The former runnable `examples/codex` copy matched `codex@0.1.0` and was not a
managed recipe. Its [compatibility pointer](../examples/codex) explains how to
install into a fresh target.

## Test An Unpublished Recipe

Git preview registries let maintainers test a branch without treating it as an
official release:

```bash
vaka registry add-git preview \
  https://github.com/infrasecture/vaka-registry.git \
  --ref main
vaka recipes info preview/codex
vaka get preview/codex codex-preview

# After the configured ref advances:
vaka registry refresh preview
(cd codex-preview && vaka get)
```

Preview recipe names are always registry-qualified. An explicit refresh pins
the configured ref to a new commit and atomically replaces the local preview
catalog. Replace `main` with your candidate branch when testing unpublished
work, and install previews into disposable targets separate from official
recipes.

## Recommended Agent Pattern

For agent containers, prefer a sidecar or gateway pattern:

- The agent container is blocked by default.
- The agent can reach only local services it needs.
- Internet-facing access lives in a narrower gateway service.
- Each gateway has its own explicit egress allowlist.

This is usually safer than allowing the agent container to reach model
providers, package registries, GitHub, arbitrary documentation sites, and
internal systems directly.

## Adapting Existing Compose Agent Stacks

Vaka can usually be added to an existing Compose stack without changing the
Compose file:

1. Identify the service that runs the agent loop.
2. List the external endpoints it actually needs.
3. Add DNS plus those endpoints to `vaka.yaml`.
4. Run `vaka validate --compose docker-compose.yaml`.
5. Start with `vaka up` instead of `docker compose up`.

Inspect an installed recipe when you need a maintained policy reference. Adapt
the policy to your own service names and endpoints rather than copying the
recipe's launcher or Compose contract piecemeal.

Common candidates include self-hosted coding agents, browser/tool sandboxes,
model gateways, package-cache sidecars, and MCP gateway services.

Examples of stacks where the same pattern can apply:

- [OpenHands](https://github.com/OpenHands/OpenHands)
- [OpenClaw](https://github.com/openclaw/openclaw)
- [SwarmClaw](https://github.com/swarmclawai/swarmclaw)
- [Docker Compose for Agents](https://github.com/docker/compose-for-agents)

Treat the links as integration targets, not as tested official Vaka recipes.

## Other Useful Patterns

The same policy model applies outside coding agents:

- Vendor or SaaS connector containers that should call only the vendor's published endpoints.
- CI and build containers that should reach package registries and artifact stores, not production services.
- Dev and staging services that should not accidentally connect to production systems.
- Data-processing jobs that should egress only to approved warehouses, logs, or object stores.
- Suspicious binary analysis where the process should have no network access or only a narrow allowlist.
- Plugin or extension containers that need their own explicit egress contract.
