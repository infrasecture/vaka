# Installation

This page covers installing the `vaka` CLI. The CLI runs on the host. The helper runtime (`vaka-init` plus `nft`) runs inside Linux containers and is pulled automatically on first use.

Vaka requires Docker Engine 28.0.0 or newer, an effective Docker client API of
1.48 or newer, and Docker Compose 2.35.0 or newer. These versions provide the
read-only Compose image mounts used to deliver the runtime on both rootful and
rootless Docker. If `DOCKER_API_VERSION` is set, it must not pin the client
below 1.48.

Docker Engine 29.0.x and 29.1.x have an upstream image-mount path-length bug.
With those Engine versions, use Docker Compose 2.35.0 through 5.0.x, or
preferably upgrade the Engine to 29.2.0 or newer. Compose 5.1.0 and newer expand
compact image-ID prefixes before container creation and therefore cannot avoid
the affected Engine behavior. `vaka doctor` detects and rejects this specific
combination before running Compose.

## Linux Packages

Linux release assets are distributed as Debian, RPM, and Arch Linux packages.
The version below matches the current
[latest release](https://github.com/infrasecture/vaka/releases/latest). Choose
the block for your distribution; each one is self-contained.

### Debian Or Ubuntu

```bash
VAKA_VERSION=0.3.3
vaka_arch="$(dpkg --print-architecture)"
case "${vaka_arch}" in
  amd64|arm64) ;;
  *) echo "Unsupported architecture: ${vaka_arch}" >&2; exit 1 ;;
esac
curl -fLO "https://github.com/infrasecture/vaka/releases/download/v${VAKA_VERSION}/vaka_${VAKA_VERSION}_${vaka_arch}.deb"
sudo dpkg -i "vaka_${VAKA_VERSION}_${vaka_arch}.deb"
```

### Fedora, RHEL, Or CentOS

```bash
VAKA_VERSION=0.3.3
vaka_arch="$(uname -m)"
case "${vaka_arch}" in
  x86_64|aarch64) ;;
  *) echo "Unsupported architecture: ${vaka_arch}" >&2; exit 1 ;;
esac
curl -fLO "https://github.com/infrasecture/vaka/releases/download/v${VAKA_VERSION}/vaka-${VAKA_VERSION}-1.${vaka_arch}.rpm"
sudo rpm -i "vaka-${VAKA_VERSION}-1.${vaka_arch}.rpm"
```

### Arch Linux

```bash
VAKA_VERSION=0.3.3
vaka_arch="$(uname -m)"
case "${vaka_arch}" in
  x86_64|aarch64) ;;
  *) echo "Unsupported architecture: ${vaka_arch}" >&2; exit 1 ;;
esac
curl -fLO "https://github.com/infrasecture/vaka/releases/download/v${VAKA_VERSION}/vaka-${VAKA_VERSION}-1-${vaka_arch}.pkg.tar.zst"
sudo pacman -U "vaka-${VAKA_VERSION}-1-${vaka_arch}.pkg.tar.zst"
```

Packages install the host CLI at `/usr/local/bin/vaka`. They do not install
`vaka-init` or `nft` on the host; those are delivered together by the
independently versioned runtime image.

## Linux Build From Source

The build script uses Docker, so a local Go toolchain is not required for normal
builds.

```bash
git clone https://github.com/infrasecture/vaka.git
cd vaka
./build.sh

case "$(uname -m)" in
  x86_64) vaka_arch=amd64 ;;
  aarch64|arm64) vaka_arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
sudo install -m 0755 "dist/vaka-linux-${vaka_arch}" /usr/local/bin/vaka
```

Build packages with:

```bash
./build.sh --packages
```

Local package outputs include version- and format-specific names. List the
artifacts produced by the current build, then install the exact path for your
package manager:

```bash
find dist -maxdepth 1 -type f \
  \( -name '*.deb' -o -name '*.rpm' -o -name '*.pkg.tar.zst' \) \
  -print
```

Build the full release matrix with:

```bash
./build.sh --release
```

That release build produces all four host CLI binaries:

- `dist/vaka-linux-amd64`
- `dist/vaka-linux-arm64`
- `dist/vaka-darwin-amd64`
- `dist/vaka-darwin-arm64`

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

Homebrew is the preferred macOS install path, but stable raw binaries also have
version-independent latest-release URLs:

```bash
case "$(uname -m)" in
  arm64) vaka_arch=arm64 ;;
  x86_64) vaka_arch=amd64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

curl -fsSL \
  "https://github.com/infrasecture/vaka/releases/latest/download/vaka-darwin-${vaka_arch}" \
  -o vaka
sudo install -m 0755 vaka /usr/local/bin/vaka
```

## First-Run Runtime Image

Normal use selects the Docker daemon architecture and pulls the corresponding
`emsi/vaka-init:runtime-vX.Y.Z-amd64` or
`emsi/vaka-init:runtime-vX.Y.Z-arm64` image when needed. The runtime bundle has
its own version and does not change for every Vaka CLI release. Run:

```bash
vaka doctor
```

To let vaka pull or repair the runtime image automatically:

```bash
vaka doctor --fix
```

Development CLI builds use the same committed runtime bundle version and can
therefore use `vaka doctor --fix` normally.

## Air-Gapped Runtime

Managed services require Vaka's verified read-only runtime image mount. The
former baked-helper modes are not safe for later exec processes because files
in a service image can be replaced in the writable container layer. In an
air-gapped environment, transfer and load the exact architecture-specific
`emsi/vaka-init:runtime-vX.Y.Z-<arch>` image into the selected Docker target,
then verify it with `vaka doctor` before starting services.
