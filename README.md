# vaka

> Declarative egress firewalling for Docker Compose services.

[![License: LGPL v2.1](https://img.shields.io/badge/License-LGPL_v2.1-blue.svg)](LICENSE)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8.svg)](go.mod)
[![Status: alpha](https://img.shields.io/badge/status-alpha-orange.svg)](#status)
[![Latest release](https://img.shields.io/github/v/release/infrasecture/vaka?include_prereleases&sort=semver)](https://github.com/infrasecture/vaka/releases)

vaka lets you run `vaka up` instead of `docker compose up` and enforce a per-service outbound network policy before your application starts.

You keep your existing `docker-compose.yaml`. You add a small `vaka.yaml` that says which hosts, ports, DNS servers, and metadata endpoints each service may reach. vaka loads nftables rules inside each container's own Linux network namespace, then hands control to the original entrypoint.

No image rebuilds. No compose-file edits. No generated policy files on the host.

## Contents

- [Why](#why)
- [Install](#install)
- [Quickstart](#quickstart)
- [Mental Model](#mental-model)
- [Requirements](#requirements)
- [Limits](#limits)
- [Examples](#examples)
- [Documentation](#documentation)
- [Status](#status)
- [License](#license)

## Why

AI agents, build containers, vendor tools, and CI jobs often run with real credentials and broad filesystem access. If one of those processes is prompt-injected, misconfigured, or compromised, it can try to send secrets to an unexpected endpoint.

vaka reduces that blast radius. A service that only needs OpenAI, Anthropic, GitHub, and your package registry should not be able to POST data to an arbitrary webhook. With vaka, that connection is rejected by the kernel from inside the container's network namespace.

## Install

On macOS, use Homebrew:

```bash
brew tap infrasecture/tap
brew install vaka

# or track nightly builds
brew install vaka-nightly
```

On Linux, install a `.deb`, `.rpm`, or Arch package from the [releases page](https://github.com/infrasecture/vaka/releases). Full install options are in [docs/installation.md](docs/installation.md).

## Quickstart

Create `vaka.yaml` next to your `docker-compose.yaml`:

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

The service name under `services:` must match a service in your Compose file.

Check the setup:

```bash
vaka doctor
vaka validate --compose docker-compose.yaml
```

Start the stack:

```bash
vaka up
```

Use normal Compose commands through vaka:

```bash
vaka logs -f agent
vaka exec agent sh
vaka down
```

For a slower walkthrough, see [docs/quickstart.md](docs/quickstart.md).

## Mental Model

Think of vaka as Docker Compose plus one extra startup step:

1. Compose still defines the containers.
2. `vaka.yaml` defines outbound network policy.
3. vaka injects a tiny `vaka-init` helper at container startup.
4. `vaka-init` loads nftables rules inside the container.
5. The original app starts under that policy.

If the firewall cannot be installed, the app does not start.

## Requirements

- Docker Engine or Docker Desktop with Docker Compose v2.
- Linux containers. Docker Desktop on macOS is supported because containers run inside Docker's Linux VM.
- A Compose project and a matching `vaka.yaml`.
- Network access to pull the `emsi/vaka-init:<version>` helper image on first use, unless you bake the helper binaries into your image.

## Limits

- vaka controls outbound traffic only. It does not manage published ports or inbound access.
- `network_mode: host` is not supported because there is no per-container network namespace to isolate.
- Hostnames are resolved when the container starts. Restart long-running services if allowed endpoints move.
- vaka is not a VM or a hostile-code sandbox. It reduces network blast radius; it does not defend against kernel escapes or code that already has access to sensitive files inside the mounted workspace.
- Some nftables features require reasonably modern Linux kernels. Very old pre-5.x kernels may fail to load the generated ruleset.

## Examples

The first complete example is [examples/codex](examples/codex): Codex runs in one container, LiteLLM runs as a local gateway, and vaka prevents Codex from reaching the internet directly.

That example demonstrates the recommended sidecar pattern for agent containers:

- The agent can reach only local sidecars it needs.
- Internet-facing access is placed on a narrower gateway service.
- Each service gets its own egress policy.

More examples will live under [examples/](examples/). See [docs/examples.md](docs/examples.md) for the catalogue and adaptation notes.

## Documentation

- [Installation](docs/installation.md)
- [Quickstart](docs/quickstart.md)
- [Examples](docs/examples.md)
- [Policy reference](docs/policy.md)
- [CLI reference](docs/cli.md)
- [Security model](docs/security.md)
- [How it works](docs/how-it-works.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Maintainers](docs/maintainers.md)
- [Historical implementation notes](docs/archive/README.md)

## Status

vaka is **alpha**. The CLI surface, `vaka.yaml` schema (`agent.vaka/v1alpha1`), and build outputs may change between 0.x releases. The core path is already clear: load nftables rules before the application process starts.

- Issues and feature requests: <https://github.com/infrasecture/vaka/issues>
- Source: <https://github.com/infrasecture/vaka>

## License

vaka is licensed under the GNU Lesser General Public License v2.1. See [LICENSE](LICENSE) for the full text.
