# Maintainers

This page is for project maintainers and release work. User installation and operation docs live elsewhere.

## Build Requirements

- Docker with buildx.
- `git`.
- `gh` for releases.
- QEMU/binfmt only when building foreign-arch nft images on Linux.

The normal build path does not require a local Go toolchain; Go builds run in `golang:1.25-alpine`.

## Local Build

```bash
./build.sh
```

Default behavior builds native host artifacts:

- `ARCHS` defaults to host architecture.
- `CLI_TARGETS` defaults to host OS/architecture.

Build the full release matrix:

```bash
./build.sh --release
```

Build packages:

```bash
./build.sh --packages
```

Build and push release images:

```bash
./build.sh --release --packages --push
```

## Build Outputs

Raw binaries in `dist/`:

- `vaka-linux-amd64`
- `vaka-linux-arm64`
- `vaka-darwin-amd64`
- `vaka-darwin-arm64`
- `vaka-init-linux-amd64`
- `vaka-init-linux-arm64`
- `nft-linux-amd64`
- `nft-linux-arm64`

Linux packages:

- `.deb`
- `.rpm`
- `.pkg.tar.*`

Package install paths:

- `/usr/local/bin/vaka`
- `/opt/vaka/sbin/vaka-init`
- `/opt/vaka/sbin/nft`

## Release Script

Stable release:

```bash
git tag v0.0.2
git push origin v0.0.2
./release.sh
```

Nightly release:

```bash
./release.sh --nightly
```

`release.sh`:

- requires a clean working tree,
- initializes and updates the `homebrew-tap` submodule,
- runs `./build.sh --release --packages --push`,
- creates release checksums,
- publishes a GitHub release,
- updates the stable or nightly Homebrew formula,
- pushes the tap,
- commits and pushes the submodule pointer bump when needed.

GitHub release assets include Linux packages, macOS raw binaries, Homebrew bundles, and `SHA256SUMS`. Raw Linux CLI binaries, raw `vaka-init` binaries, and raw `nft` binaries are build outputs but are not uploaded as release assets.

## Homebrew Tap

The tap lives in the `homebrew-tap` submodule.

User-facing install command:

```bash
brew tap infrasecture/tap
brew install vaka
```

Nightly:

```bash
brew install vaka-nightly
```

The formula installs both:

- `vaka`
- `vaka-init`

## Multi-Arch Publishing

Single host with QEMU:

```bash
sudo apt-get install -y qemu-user-static
./build.sh --release --push
```

Separate native hosts:

```bash
ARCHS=amd64 ./build.sh --push
ARCHS=arm64 ./build.sh --push
./build.sh --release --manifest
```

Published manifest tags:

- `emsi/nft-static:<nftables-version>`
- `emsi/nft-static:latest`
- `emsi/vaka-init:<vaka-version>`
- `emsi/vaka-init:latest`

## Tests

```bash
go test ./...
```

Dockerized:

```bash
docker run --rm \
  -v "$(pwd):/src:ro" -w /src \
  -e GOWORK=off \
  golang:1.25-alpine \
  go test ./...
```
