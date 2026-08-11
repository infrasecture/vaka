# Codex Recipe

The runnable Codex and LiteLLM example is maintained in the official
[vaka recipe registry](https://github.com/infrasecture/vaka-registry/tree/main/codex).
This directory is intentionally documentation-only so a second executable copy
cannot drift behind the published recipe.

## Inspect And Install

Use the catalog to discover the current version, digest, minimum Vaka version,
environment inputs, and advisory risk flags, then install into a new directory:

```bash
vaka search codex
vaka recipes info codex
vaka get codex codex-recipe
```

`vaka get` verifies and validates the recipe but never starts Docker. Run the
installed recipe through its own launcher from the project you want Codex to
work on:

```bash
cd /path/to/project
/path/to/codex-recipe/myCodex
```

Do not replace the launcher with bare `vaka up` or `docker compose`. `myCodex`
selects the authentication profile, supplies host identity and per-project
state, and routes Compose through Vaka.

Running the launcher from the installed recipe directory is also supported; it
creates or reuses a confined named workspace rather than mounting recipe files
and credentials into Codex:

```bash
cd /path/to/codex-recipe
./myCodex
```

## Update

An installed recipe contains a `.vaka-recipe.lock`, so it can be updated safely
in place:

```bash
cd /path/to/codex-recipe
vaka get
```

Then apply updated stack files from each project that uses the recipe:

```bash
cd /path/to/project
/path/to/codex-recipe/myCodex up
```

The old runnable copy that once lived in this directory matched
`codex@0.1.0`. It had no recipe manifest or lock and cannot be adopted or
updated in place. Install the current catalog version into a fresh target as
shown above.

## Historical Security Migration

`codex@0.3.1` corrected exposure of the LiteLLM administrator credential to the
Codex container in versions through `0.3.0`. If you have containers or an
installed recipe from an affected version, follow the
[0.3.1 migration instructions](https://github.com/infrasecture/vaka-registry/blob/codex-0.3.1/codex/README.md#required-upgrade-from-030-or-earlier).
The current catalog version is always reported by `vaka recipes info codex`.

See the [examples guide](../../docs/examples.md) for the complete registry
workflow and [CLI reference](../../docs/cli.md#vaka-get) for install and update
semantics.
