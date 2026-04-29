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

Create `vaka.yaml` next to `docker-compose.yaml`:

```yaml
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  agent:
    network:
      egress:
        defaultAction: reject
        block_metadata: drop
        accept:
          - dns: {}
          - proto: tcp
            to:
              - api.openai.com
              - api.anthropic.com
              - api.github.com
              - github.com
            ports: [443]
```

Replace `agent` with the real Compose service name.

## 3. Validate

```bash
vaka validate --compose docker-compose.yaml
```

This checks the policy schema, service names, and unsupported `network_mode: host` services.

## 4. Check The Host

```bash
vaka doctor
```

If the helper image is missing, let vaka pull it:

```bash
vaka doctor --fix
```

## 5. Start The Stack

```bash
vaka up
```

Use regular Compose flags as usual:

```bash
vaka up --build -d
vaka -f compose.prod.yaml up -d
```

Use `--vaka-file=<path>` before the subcommand when the policy file is not named `vaka.yaml`:

```bash
vaka --vaka-file=policies/prod.yaml -f compose.prod.yaml up -d
```

## 6. Operate The Stack

```bash
vaka ps
vaka logs -f agent
vaka exec agent sh
vaka down
```

Reference commands are proxied through Docker Compose with a minimal vaka overlay so the helper resources stay visible.

## Preview Generated Output

Preview the nftables rules for one service:

```bash
vaka show-nft --compose docker-compose.yaml agent
```

Preview the generated Compose override:

```bash
vaka show-compose
vaka show-compose -o /tmp/vaka-override.yaml
```

`show-compose` intentionally does not print the per-service encoded policy values.

## Build-Only Services

If a Compose service has `build:` but no `image:`, vaka may not be able to inspect image defaults before the build. Either add an `image:` name or provide runtime metadata explicitly:

```yaml
services:
  app:
    build: .
    image: app:local
```

or:

```yaml
services:
  app:
    build: .
    user: "1000:1000"
    entrypoint: ["/usr/local/bin/app"]
```

Without image inspection or explicit metadata, `vaka up` fails before containers start.
