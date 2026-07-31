# Quickstart

This guide assumes you already have a working Docker Compose project.

## 1. Install vaka

Linux:

```bash
# Debian / Ubuntu
curl -fLO https://github.com/infrasecture/vaka/releases/download/v0.0.2/vaka_0.0.2_amd64.deb
sudo dpkg -i vaka_0.0.2_amd64.deb
```

See [installation.md](installation.md) for RPM, Arch Linux, source-build, and macOS binary options.

macOS:

```bash
brew tap infrasecture/tap
brew install vaka
```

## 2. Add `vaka.yaml`

For a realistic starting point, use the [Codex + LiteLLM example](../examples/codex). It puts the internet-facing model-provider access on a LiteLLM sidecar and keeps the Codex container restricted to that local sidecar.

Start from [`examples/codex/vaka.yaml`](../examples/codex/vaka.yaml), then adapt service names and allowed endpoints for your Compose project.

Create `vaka.yaml` next to `docker-compose.yaml`. Each key under `services:` must match a Compose service name.

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
