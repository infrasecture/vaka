# vaka

> Declarative egress firewalling for Docker Compose services.

[![License: LGPL v2.1](https://img.shields.io/badge/License-LGPL_v2.1-blue.svg)](LICENSE)
[![Go 1.25](https://img.shields.io/badge/go-1.25-00ADD8.svg)](go.mod)
[![Status: alpha](https://img.shields.io/badge/status-alpha-orange.svg)](#status)
[![Latest release](https://img.shields.io/github/v/release/infrasecture/vaka?include_prereleases&sort=semver)](https://github.com/infrasecture/vaka/releases)

vaka lets you run `vaka up` instead of `docker compose up` and enforce a per-service outbound network policy before your application starts.

You keep your existing `docker-compose.yaml`. You add a small `vaka.yaml` that says which hosts, ports, DNS servers, and metadata endpoints each service may reach. vaka loads nftables rules inside each container's own Linux network namespace, then hands control to the original entrypoint.

Normal use does not require a Vaka-specific image rebuild. Policy stays in a
separate file, and Vaka does not write generated policy files to the host.

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

On Linux, install the package for your distribution and architecture from the
[latest release](https://github.com/infrasecture/vaka/releases/latest). On
macOS, use Homebrew:

```bash
brew tap infrasecture/tap
brew install vaka
```

Then verify the host setup:

```bash
vaka version
vaka doctor
```

Packages, raw macOS binaries, source builds, nightly builds, and compatibility
requirements are covered in [Installation](docs/installation.md).

## Quickstart

Choose the path that matches what you want to do.

### Install And Review A Published Recipe

Search the built-in registry, inspect the recipe, and install it into a new
directory:

```bash
vaka search codex
vaka recipes info codex
mkdir -p "$HOME/vaka-recipes"
vaka get codex "$HOME/vaka-recipes/codex"
```

`vaka get` verifies and validates the recipe but never runs it. Read the
installed instructions before starting anything:

```bash
cd "$HOME/vaka-recipes/codex"
less README.md
```

### Protect Your Compose Project

Create `vaka.yaml` next to your Compose file, with one matching entry for each
service you want Vaka to manage. Then validate and start the project:

```bash
vaka validate --compose docker-compose.yaml
vaka up -d
```

Use the fixed shorthands for common operations:

```bash
vaka ps
vaka logs --tail 50 agent
vaka exec agent sh
vaka down
```

The complete Compose command surface lives under `vaka compose`:

```bash
vaka compose -f compose.prod.yaml up -d
vaka compose pull
```

See [Quickstart](docs/quickstart.md) for a complete policy example and the
recipe update flow.

## Mental Model

Think of vaka as Docker Compose plus one extra startup step:

1. Compose still defines the containers.
2. `vaka.yaml` defines outbound network policy.
3. vaka injects a tiny `vaka-init` helper at container startup.
4. `vaka-init` loads nftables rules inside the container.
5. The original app starts under that policy.

If the firewall cannot be installed, the app does not start.

## Requirements

- Docker Engine 28.0.0 or newer (effective API 1.48+) and Docker Compose 2.35.0 or newer.
- Linux containers. Docker Desktop on macOS is supported because containers run inside Docker's Linux VM.
- A Compose project and matching `vaka.yaml` when protecting your own stack;
  published recipes supply their own Compose and policy files.
- Network access to pull the versioned `emsi/vaka-init:runtime-vX.Y.Z` runtime image on first use, unless you bake the helper binaries into your image.

## Limits

- vaka controls outbound traffic only. It does not manage published ports or inbound access.
- `network_mode: host` is not supported because there is no per-container network namespace to isolate.
- Hostnames are resolved when the container starts. Restart long-running services if allowed endpoints move.
- vaka is not a VM or a hostile-code sandbox. It reduces network blast radius; it does not defend against kernel escapes or code that already has access to sensitive files inside the mounted workspace.
- Some nftables features require reasonably modern Linux kernels. Very old pre-5.x kernels may fail to load the generated ruleset.

## Examples

A gateway is a useful pattern when an agent needs access to a provider but does
not need general internet access:

- The agent can reach only local sidecars it needs.
- Internet-facing access is placed on a narrower gateway service.
- Each service gets its own egress policy.

See [Examples](docs/examples.md) for the current `vaka get` workflow, safe
updates, and this gateway pattern. Runnable recipes come from configured
registries; this repository does not carry a second copy.

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

## Status

vaka is **alpha**. The CLI surface, `vaka.yaml` schema (`agent.vaka/v1alpha1`), and build outputs may change between 0.x releases. The core path is already clear: load nftables rules before the application process starts.

- Issues and feature requests: <https://github.com/infrasecture/vaka/issues>
- Source: <https://github.com/infrasecture/vaka>

## License

vaka is licensed under the GNU Lesser General Public License v2.1. See [LICENSE](LICENSE) for the full text.
