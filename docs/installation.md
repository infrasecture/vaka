# Installation

This page covers installing the `vaka` CLI. The CLI runs on the host. The helper runtime (`vaka-init` plus `nft`) runs inside Linux containers and is pulled automatically on first use unless you use the baked-in helper mode.

Vaka requires Docker Engine 28.0.0 or newer, an effective Docker client API of
1.48 or newer, and Docker Compose 2.35.0 or newer. These versions provide the
read-only Compose image mounts used to deliver the runtime on both rootful and
rootless Docker. If `DOCKER_API_VERSION` is set, it must not pin the client
below 1.48.

## Linux Packages

Linux release assets are distributed as Debian, RPM, and Arch Linux packages from the [GitHub releases page](https://github.com/infrasecture/vaka/releases).

```bash
# Debian / Ubuntu
curl -fLO https://github.com/infrasecture/vaka/releases/download/v0.0.2/vaka_0.0.2_amd64.deb
sudo dpkg -i vaka_0.0.2_amd64.deb

# Fedora / RHEL / CentOS
curl -fLO https://github.com/infrasecture/vaka/releases/download/v0.0.2/vaka-0.0.2-1.x86_64.rpm
sudo rpm -i vaka-0.0.2-1.x86_64.rpm

# Arch Linux
curl -fLO https://github.com/infrasecture/vaka/releases/download/v0.0.2/vaka-0.0.2-1-x86_64.pkg.tar.zst
sudo pacman -U vaka-0.0.2-1-x86_64.pkg.tar.zst
```

Packages install the host CLI at `/usr/local/bin/vaka`. They do not install
`vaka-init` or `nft` on the host; those are delivered together by the
independently versioned runtime image.

## Linux Build From Source

The build script uses Docker, so a local Go toolchain is not required for normal builds.

```bash
git clone https://github.com/infrasecture/vaka.git
cd vaka
./build.sh
sudo install -m 0755 dist/vaka-linux-amd64 /usr/local/bin/vaka
```

Use the binary matching your host:

- `dist/vaka-linux-amd64`
- `dist/vaka-linux-arm64`
- `dist/vaka-darwin-amd64`
- `dist/vaka-darwin-arm64`

Build packages with:

```bash
./build.sh --packages
```

Local package outputs appear in `dist/`, for example:

```bash
sudo dpkg -i dist/vaka_0.0.2_amd64.deb
sudo rpm -i dist/vaka-0.0.2-1.x86_64.rpm
sudo pacman -U dist/vaka-0.0.2-1-x86_64.pkg.tar.zst
```

Build the full release matrix with:

```bash
./build.sh --release
```

## macOS With Homebrew

```bash
brew tap infrasecture/tap
brew install vaka
```

For nightly builds:

```bash
brew tap infrasecture/tap
brew install vaka-nightly
```

The Homebrew formula installs only `vaka`. The Linux helper runtime is pulled
as an image by Vaka and never executes on the macOS host.

`vaka` and `vaka-nightly` install the same command names, so only one channel should be linked at a time.

To switch from stable to nightly:

```bash
brew unlink vaka
brew install vaka-nightly
```

If a previous `brew install vaka-nightly` already fetched the formula but failed at the link step, Homebrew may report that it is installed but unlinked. In that case run:

```bash
brew link vaka-nightly
```

To switch back:

```bash
brew unlink vaka-nightly
brew install vaka
```

Docker Desktop must be using Linux containers. That is the normal Docker Desktop mode on macOS.

## macOS Release Binaries

Homebrew is the preferred macOS install path, but raw macOS binaries are also attached to releases:

```bash
# macOS arm64, Apple Silicon
curl -fsSL https://github.com/infrasecture/vaka/releases/download/v0.0.2/vaka-darwin-arm64 -o vaka

# macOS amd64, Intel
curl -fsSL https://github.com/infrasecture/vaka/releases/download/v0.0.2/vaka-darwin-amd64 -o vaka

chmod +x vaka
sudo mv vaka /usr/local/bin/vaka
```

Replace `v0.0.2` with the release you want if you are not installing the latest release.

## First-Run Runtime Image

Normal use pulls `emsi/vaka-init:runtime-vX.Y.Z` when needed. The runtime
bundle has its own version and does not change for every Vaka CLI release. Run:

```bash
vaka doctor
```

To let vaka pull or repair the runtime image automatically:

```bash
vaka doctor --fix
```

Development CLI builds use the same committed runtime bundle version and can
therefore use `vaka doctor --fix` normally.

## Air-Gapped Or Baked-In Helper Mode

If containers cannot use the runtime image, copy its binaries into your service image:

```dockerfile
ARG VAKA_RUNTIME_VERSION=v0.1.0
FROM emsi/vaka-init:runtime-${VAKA_RUNTIME_VERSION} AS vaka
FROM ubuntu:24.04
COPY --from=vaka --chmod=0555 /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
COPY --from=vaka --chmod=0555 /opt/vaka/sbin/nft       /opt/vaka/sbin/nft
```

Use the runtime version required by your Vaka build; `vaka doctor` reports the
corresponding image tag. The injected policy requires an exact runtime-version
match and fails closed if a baked-in helper is stale.

Then pass `--vaka-init-present` before the subcommand:

```bash
vaka --vaka-init-present up
vaka --vaka-init-present down
```

You can also mark individual services in `docker-compose.yaml`:

```yaml
services:
  app:
    labels:
      agent.vaka.init: present
```

Services with that label use the baked-in `/opt/vaka/sbin/vaka-init` and
`/opt/vaka/sbin/nft`; other services receive the runtime through a read-only
image mount.
