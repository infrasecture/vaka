# Quickstart

This guide assumes you already have a working Docker Compose project.

## 1. Install vaka

Linux:

```bash
# Debian / Ubuntu
VAKA_VERSION=0.3.0
curl -fLO "https://github.com/infrasecture/vaka/releases/download/v${VAKA_VERSION}/vaka_${VAKA_VERSION}_amd64.deb"
sudo dpkg -i "vaka_${VAKA_VERSION}_amd64.deb"
```

See [installation.md](installation.md) for RPM, Arch Linux, source-build, and macOS binary options.

macOS:

```bash
brew tap infrasecture/tap
brew install vaka
```

## Recipe Fast Path

If you want a maintained, ready-to-run stack instead of adapting an existing
Compose project, install the Codex recipe from the official registry:

```bash
vaka recipes info codex
vaka get codex
(cd codex && ./myCodex)
```

`vaka get` verifies and validates the recipe but never starts Docker. Codex uses
its recipe-specific launcher rather than bare `vaka up`; the subshell returns
you to the parent directory when the session ends. See the
[examples guide](examples.md) for running it against an existing project,
updates, authentication profiles, and the security-migration notice.

## 2. Add `vaka.yaml`

For your existing Compose project, create `vaka.yaml` next to
`docker-compose.yaml`. Each key under `services:` must match a Compose service
name; see the [policy reference](policy.md) for the schema.

To inspect a maintained sidecar policy without mixing it into your project,
fetch the recipe into a separate, new directory and review its `vaka.yaml`:

```bash
vaka get codex codex-reference
```

Adapt service names and allowed endpoints to your own stack. Do not copy the
recipe's launcher or Compose files piecemeal; those files form one tested
runtime and credential contract.

## 3. Validate

```bash
vaka validate --compose docker-compose.yaml
```

This checks the policy schema, service names, and unsupported `network_mode: host` services.

## 4. Check The Host

```bash
vaka doctor
```

If the runtime image is missing or incompatible, let vaka repair it:

```bash
vaka doctor --fix
```

## 5. Start The Stack

```bash
vaka up
```

Use regular Compose flags after the command as usual; Compose global flags
such as `-f` require the `vaka compose` form:

```bash
vaka up --build -d
vaka compose -f compose.prod.yaml up -d
```

Use `--vaka-file=<path>` before the command when the policy file is not named `vaka.yaml`:

```bash
vaka --vaka-file=policies/prod.yaml compose -f compose.prod.yaml up -d
```

## 6. Operate The Stack

```bash
vaka ps
vaka logs -f agent
vaka exec agent sh
vaka down
```

Reference commands are proxied through Docker Compose with a metadata-only
Vaka overlay; they do not evaluate policy or create helper resources.

## Preview Generated Output

Preview the nftables rules for one service:

```bash
vaka show-nft agent
```

Preview the generated Compose override:

```bash
vaka show-compose
vaka show-compose -o /tmp/vaka-override.yaml
```

`show-compose` intentionally does not print the per-service encoded policy values.

## Build-Only Services

For build-only service failures, see
[Build-Only Services](troubleshooting.md#build-only-services).
