# Recipes And Examples

Runnable examples are distributed as versioned recipes. This repository
documents the workflow; `vaka get` installs the current recipe from a configured
registry.

## Discover A Recipe

Search recipe names, descriptions, and tags:

```bash
vaka search codex
vaka recipes info codex
```

`vaka recipes info` shows the selected version, digest, minimum Vaka version,
documented environment variables, indexed versions, and the registry's
advisory policy and risk summary. It reads catalog data and does not inspect a
local installation.

## Install The Codex Recipe

The target's parent must exist and the target itself must be new:

```bash
mkdir -p "$HOME/vaka-recipes"
vaka get codex "$HOME/vaka-recipes/codex"
```

During installation, `vaka get`:

- verifies the downloaded artifact against its registry digest;
- extracts it without allowing path escapes or special files;
- validates the recipe identity, minimum Vaka version, Compose model, and
  `vaka.yaml` policy;
- writes `.vaka-recipe.lock` with the installed provenance and file state; and
- publishes the completed directory without replacing an existing path.

After the files are installed, `vaka get` reports a locally computed policy and
risk summary and warns about required environment variables absent from the
current process. It does not load `.env`. A failure to produce that advisory
summary is a warning and does not roll back an otherwise valid installation.

`vaka get` installs files only. It never starts Docker or runs recipe code.

## Review The Installation

Read the instructions and policy that came from the verified artifact:

```bash
cd "$HOME/vaka-recipes/codex"
less README.md
less vaka.yaml
```

Then use the launch command documented in that README. A recipe may use plain
`vaka up` or provide a launcher for additional identity, credentials, Compose
overlays, or state handling. Vaka deliberately does not guess how a recipe
should be run.

## Update An Installed Recipe

Run the same verb without a recipe name from inside the installation:

```bash
(cd "$HOME/vaka-recipes/codex" && vaka get)
```

The lock supplies the registry and recipe name, and an omitted version selects
the highest indexed version. Use the exact-version forms documented under
[`vaka get`](cli.md#vaka-get) when you need a specific `X.Y.Z` release.

Updates are conservative:

- An edited tracked file that the new release still ships blocks the update.
- A locally deleted tracked file that is still shipped is restored.
- A modified tracked file dropped by the new release is kept as an untracked
  deviation.
- Untracked files are left alone. A byte-identical collision is adopted into
  the lock; a different collision is kept and reported as a deviation.
- An interrupted update is journaled and converges when `vaka get` is run
  again.

Updating files does not change running containers. Follow the installed README
to apply the new recipe version.

## A Gateway Pattern For Agents

A local gateway can let an agent reach a provider without giving the agent
general internet access. The pattern is:

- deny the agent's outbound traffic by default;
- allow it to reach only the local services it needs;
- put provider access on a narrower gateway service; and
- give each service its own explicit egress policy.

Use this as an architectural example, not as a file-copy template. To protect
your own Compose project, start with the [Quickstart](quickstart.md) and adapt
the [policy schema](policy.md) to your service names and destinations.

For custom published registries and commit-pinned Git previews, see the
[registry commands](cli.md#vaka-registry).
