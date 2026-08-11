# Quickstart

Vaka supports two starting points:

- Install a published recipe when you want a ready-made Compose project.
- Add `vaka.yaml` when you already have a Compose project.

## Before You Start

Install the Vaka CLI using the [installation guide](installation.md), then
check the Docker target:

```bash
vaka version
vaka doctor
```

If the runtime image is missing or incompatible, let Vaka repair it:

```bash
vaka doctor --fix
```

## Path A: Install And Review A Published Recipe

Search the configured registries and inspect a recipe before downloading it:

```bash
vaka search codex
vaka recipes info codex
```

Create a parent directory and install into a new target:

```bash
mkdir -p "$HOME/vaka-recipes"
vaka get codex "$HOME/vaka-recipes/codex"
```

The first install target must not already exist. `vaka get` verifies the
artifact digest, validates the manifest, Compose model, and Vaka policy, checks
the minimum Vaka version, and records provenance in `.vaka-recipe.lock`. It
never starts containers or runs recipe scripts.

Read the instructions shipped with the installed recipe:

```bash
cd "$HOME/vaka-recipes/codex"
less README.md
```

Run only the command documented by that recipe. Some recipes use `vaka up`;
others provide a launcher that supplies additional configuration.

To update later, run `vaka get` from the installed directory:

```bash
(cd "$HOME/vaka-recipes/codex" && vaka get)
```

The update changes recipe files only. Follow the installed README to apply the
new files to any running stack.

## Path B: Protect Your Compose Project

Create `vaka.yaml` next to your Compose file. Replace `app` with the name of a
service from your Compose project and replace `api.example.com` with the
destination that service actually needs:

```yaml
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
        block_metadata: drop
        accept:
          - dns: {}
          - proto: tcp
            to: [api.example.com]
            ports: [443]
```

Validate the policy and its service names:

```bash
vaka validate --compose docker-compose.yaml
```

If your project uses another filename or several Compose files, repeat
`--compose` for the exact set you want to check.

Preview what Vaka will apply:

```bash
vaka show-nft app
vaka show-compose
```

Start and operate the stack through Vaka:

```bash
vaka up -d
vaka ps
vaka logs --tail 50 app
```

Open an interactive shell when you need one, then exit it before stopping the
stack:

```bash
vaka exec app sh
vaka down
```

Compose command flags follow the command. Compose global flags, such as `-f`,
`--profile`, and `--project-directory`, use the `vaka compose` namespace:

```bash
vaka up --build -d
vaka compose -f compose.prod.yaml --profile worker up -d
```

Vaka flags go before the command and value-taking flags use `=`:

```bash
vaka --vaka-file=policies/prod.yaml compose -f compose.prod.yaml up -d
```

## Next Steps

- [Examples](examples.md) explains the complete recipe lifecycle.
- [Policy reference](policy.md) covers every policy field and rule type.
- [CLI reference](cli.md) covers registry management, update safety, Compose
  dispatch, and Vaka flags.
- [Troubleshooting](troubleshooting.md) covers build-only services, image
  inspection, Docker compatibility, and DNS behavior.
