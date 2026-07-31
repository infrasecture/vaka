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

Nightly releases use the 12-character commit SHA as the GitHub release tag and
mark the release as a pre-release. They build and upload the same artifact
classes as stable releases, update `Formula/vaka-nightly.rb`, and push the
Homebrew tap update. The runtime image keeps its independent
`runtime-vX.Y.Z` tag; nightly releases do not update `:latest`.

`release.sh`:

- requires a clean working tree,
- initializes and updates the `homebrew-tap` submodule,
- runs `VERSION=<release-tag> ./build.sh --release --packages --push`,
- sets `PUBLISH_LATEST=false` for nightly releases,
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
- `emsi/vaka-init:runtime-<runtime-version>`
- `emsi/vaka-init:latest`

Architecture staging tags append `-amd64` or `-arm64`, for example
`emsi/vaka-init:runtime-v0.1.0-arm64`.

## CLI And Runtime Versioning

Vaka has two release identities with different reasons to change:

- The **CLI version** is derived from the Git release tag (or commit description
  for development builds) and stamped into `cmd/vaka`. Bump it for normal Vaka
  releases, including host parser, registry, documentation, and orchestration
  changes.
- The **runtime bundle version** is the single value in
  `internal/runtimebundle/VERSION`. It covers `vaka-init`, the bundled `nft`
  executable, the `/opt/vaka` layout and modes, and the injected policy contract
  consumed in the container.

There is deliberately no separate `vaka-init` version. `vaka-init`, `nft`, and
the generated policy contract are delivered and checked as one runtime bundle;
versioning one component separately would add release states without allowing
Vaka to usefully mix and match them.

The runtime uses v-prefixed SemVer, but compatibility is deliberately exact.
The CLI writes `requiredRuntimeVersion` into every generated service policy;
`vaka-init` refuses to run unless it has the same version. A range would allow
the policy generator and in-container enforcer to drift silently.

Use the runtime SemVer components as release communication:

- **Patch:** compatible implementation, dependency, bundled `nft`, or security
  fix that changes runtime image content.
- **Minor:** backward-compatible runtime capability or injected-policy contract
  extension.
- **Major:** incompatible policy contract, behavior, or filesystem layout.

Bump `internal/runtimebundle/VERSION` in the same commit whenever any of these
change:

- `cmd/vaka-init` behavior or dependencies;
- nft generation/execution code used by `vaka-init` or the bundled `nft` binary;
- the Go/runtime build toolchain when it changes the shipped runtime bytes;
- generated policy fields or semantics consumed by `vaka-init`;
- runtime image paths, file modes, or image-mount contract;
- a security fix whose enforcement depends on changed runtime bytes.

Do **not** bump it for host-only CLI parsing, Compose dispatch, recipe-registry,
documentation, or test changes. This prevents routine CLI releases from
creating a new runtime image and recreating every managed service.

For a release:

1. Decide whether the changes affect the runtime bundle using the rules above.
2. If they do, update `internal/runtimebundle/VERSION` and the version shown in
   `docs/installation.md` and `docker/init/Dockerfile` in the same commit. If
   they do not, leave the runtime version unchanged.
3. Create the normal next `vX.Y.Z` Git tag for the CLI release. The CLI has no
   source version file to edit; `release.sh` passes that Git tag into the build.
4. Run the normal release command. It republishes identical runtime tags only
   after verifying their exact image identity and refuses changed bytes under
   an existing architecture tag.

`build.sh` reads the same VERSION file embedded in both host and runtime code,
tags the image as `emsi/vaka-init:runtime-vX.Y.Z`, stamps the OCI/runtime label,
builds a normalized root filesystem without VCS or wall-clock identity, and
verifies:

- the runtime label matches;
- no legacy `VOLUME /opt/vaka` is declared;
- both binaries are mode `0555`;
- `vaka-init --version` reports the expected version.

Runtime tags are immutable. Never publish changed image content under an
existing `runtime-vX.Y.Z` tag; bump the runtime version first. Before pushing,
`build.sh` compares every existing architecture tag with the local image ID and
refuses replacement when they differ. Identical-content retries remain
possible. The multi-platform `runtime-vX.Y.Z` manifest is created only when
absent and is never rewritten; an incomplete published manifest requires a new
runtime version rather than in-place repair. After a version is first
published, the exact image comparison is authoritative: even a source or
toolchain change believed to be semantically neutral needs a version bump if it
changes the image ID. `:latest` is a convenience tag for humans and Dockerfiles
only. The Vaka CLI always resolves the versioned tag, validates its label, and
passes the exact local image ID to Compose.

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

### Runtime Image-Mount Smoke Test

The unit tests verify the generated Compose override, but the image-mount
contract also requires an end-to-end test against a real Docker Engine. Build
the native CLI and runtime image, then run the smoke test against the currently
selected Docker target:

```bash
./build.sh --rebuild-go
./scripts/smoke-image-mount.sh
```

The script runs `vaka doctor`, starts a real service through `vaka compose up`,
and verifies that `/opt/vaka` is an `image` mount sourced from the exact local
`sha256:` runtime image ID. It also verifies that the mount is read-only, both
runtime binaries are executable, and `vaka-init` reports the required runtime
version. It inherits `DOCKER_HOST`, `DOCKER_CONTEXT`, TLS variables, and
`DOCKER_CONFIG`, so the Vaka and inspection commands use the same target.

Before a release that changes the runtime mount or compatibility checks, run
the same test against the lower-bound matrix: an Engine 28 target and Docker
Compose 2.35.0. Select or provision that target, install Compose 2.35.0 in an
isolated `DOCKER_CONFIG`, load the freshly built runtime image into it, and make
the versions part of the assertion:

```bash
DOCKER_HOST=<engine-28-endpoint> \
DOCKER_CONFIG=<compose-2.35-config-directory> \
VAKA_SMOKE_EXPECT_ENGINE_VERSION=<selected-engine-28-version> \
VAKA_SMOKE_EXPECT_COMPOSE_VERSION=2.35.0 \
./scripts/smoke-image-mount.sh
```

The exact-version variables are optional during ordinary development. They are
required for the lower-bound release check so a newer local daemon or Compose
plugin cannot accidentally be reported as minimum-version coverage.
