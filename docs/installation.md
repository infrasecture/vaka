# Installation

This page covers installing the `vaka` CLI. The CLI runs on the host. The helper runtime (`vaka-init` plus `nft`) runs inside Linux containers and is pulled automatically on first use unless you use the baked-in helper mode.

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

The Homebrew formula installs both `vaka` and the local `vaka-init` helper binary used by the CLI package.

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

Package installs place files at:

- `/usr/local/bin/vaka`
- `/opt/vaka/sbin/vaka-init`
- `/opt/vaka/sbin/nft`

The host CLI is `/usr/local/bin/vaka`. The `/opt/vaka/sbin` binaries are helper binaries used for baked-in or package-managed environments.

## Build From Source

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

## First-Run Helper Image

Normal use pulls `emsi/vaka-init:<vaka-version>` when needed. Run:

```bash
vaka doctor
```

To let vaka pull the helper image automatically when missing:

```bash
vaka doctor --fix
```

Unstamped development builds report `version=dev`; those cannot auto-pull a published `emsi/vaka-init:dev` image.

## Air-Gapped Or Baked-In Helper Mode

If containers cannot pull the helper image, copy helper binaries into your service image:

```dockerfile
FROM emsi/vaka-init:v0.0.2 AS vaka
FROM ubuntu:24.04
COPY --from=vaka /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
COPY --from=vaka /opt/vaka/sbin/nft       /opt/vaka/sbin/nft
```

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

Services with that label use the baked-in `/opt/vaka/sbin/vaka-init` and `/opt/vaka/sbin/nft`; other services use the injected helper container.
