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
- [Codex Example](#codex-example)
- [Documentation](#documentation)
- [Status](#status)
- [License](#license)

## Why

AI agents, build containers, vendor tools, and CI jobs often run with real credentials and broad filesystem access. If one of those processes is prompt-injected, misconfigured, or compromised, it can try to send secrets to an unexpected endpoint.

vaka reduces that blast radius. A service that only needs OpenAI, Anthropic, GitHub, and your package registry should not be able to POST data to an arbitrary webhook. With vaka, that connection is rejected by the kernel from inside the container's network namespace.

## Install

### Linux

Vaka publishes native `amd64` and `arm64` packages for Debian/Ubuntu (`.deb`),
Fedora/RHEL (`.rpm`), and Arch Linux. Use the copy-and-paste commands in
[Linux installation](docs/installation.md#linux-packages), or download a package
from the [latest release](https://github.com/infrasecture/vaka/releases/latest).

### macOS

Install Vaka with Homebrew:

```bash
brew tap infrasecture/tap
brew install vaka
```

### Verify The Installation

Check Docker and pull or repair Vaka's container runtime:

```bash
vaka version
vaka doctor --fix
```

Raw macOS binaries, source builds, nightly builds, and Docker compatibility are
covered in [Installation](docs/installation.md).

## Quickstart

Install the current Codex recipe and launch its protected workspace:

```bash
mkdir -p "$HOME/vaka-recipes"
vaka get codex "$HOME/vaka-recipes/codex"
cd "$HOME/vaka-recipes/codex"
./myCodex
```

On first launch, press Enter to use the default `work` workspace, then choose
ChatGPT subscription, OpenAI API key, or the experimental Vertex profile.
`myCodex` handles authentication, pulls images as needed, starts the protected
Codex and LiteLLM stack through Vaka, and attaches you to an interactive Codex
session. Codex can reach the model through the gateway, but direct connections
to arbitrary internet hosts are rejected.

To use Codex on an existing project, run the launcher from that project instead:

```bash
cd /path/to/your/project
"$HOME/vaka-recipes/codex/myCodex"
```

The current project directory is the only host workspace mounted read/write into
Codex. See
[First Secure Codex Session](docs/quickstart.md) for what happens during startup
and how to use an existing project.

Vaka can also protect services in an existing Compose project without a recipe.
[Write Your First `vaka.yaml`](docs/vaka-yaml-quickstart.md) covers that separate
workflow.

## Mental Model

Think of vaka as Docker Compose with guarded startup and later-process paths:

1. Compose still defines the containers.
2. `vaka.yaml` defines outbound network policy.
3. vaka mounts a verified, read-only `vaka-init` runtime.
4. At startup, `vaka-init` loads nftables rules, drops Vaka-added capabilities,
   switches to the service's effective Compose/image user, and starts the app.
5. Later Vaka execs and healthchecks re-enter the immutable helper, require the
   nftables table to exist, drop capabilities, select the intended user, and
   only then start their command.

If the firewall cannot be installed, the app does not start.

## Requirements

- Docker Engine 28.0.0 or newer (effective API 1.48+) and Docker Compose 2.35.0 or newer.
- Linux containers. Docker Desktop on macOS is supported because containers run inside Docker's Linux VM.
- A Compose project and matching `vaka.yaml` when protecting your own stack;
  published recipes supply their own Compose and policy files.
- Network access to pull the versioned `emsi/vaka-init:runtime-vX.Y.Z` runtime
  image on first use, or an exact runtime image preloaded into the selected
  Docker target for air-gapped operation.

## Limits

- vaka controls outbound traffic only. It does not manage published ports or inbound access.
- `network_mode: host`, `service:...`, and `container:...` are not supported for
  managed services because they do not provide an independently owned network
  namespace for Vaka to isolate.
- Hostnames are resolved when the container starts. Restart long-running services if allowed endpoints move.
- vaka is not a VM or a hostile-code sandbox. It reduces network blast radius; it does not defend against kernel escapes or code that already has access to sensitive files inside the mounted workspace.
- Some nftables features require reasonably modern Linux kernels. Very old pre-5.x kernels may fail to load the generated ruleset.

## Codex Example

The Codex recipe is a complete example of Vaka protecting an AI coding agent.
It runs Codex in one container and LiteLLM in a second container. Your project is
mounted only into Codex; model credentials are supplied only to LiteLLM. Vaka
lets Codex call the gateway on the private Compose network and lets the gateway
call the upstream endpoints allowed by the selected profile, while rejecting
Codex's other outbound traffic.

[Codex Recipe: A Restricted Agent Workspace](docs/examples.md) shows the
architecture, authentication choices, workspace and credential boundaries,
common commands, updates, and what the isolation does and does not protect.

## Documentation

- [Installation](docs/installation.md)
- [Run Codex with Vaka](docs/quickstart.md)
- [Understand the Codex recipe](docs/examples.md)
- [Write your first `vaka.yaml`](docs/vaka-yaml-quickstart.md)
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
