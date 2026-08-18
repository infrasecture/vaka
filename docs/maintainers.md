# Maintainers

This page defines Vaka's build identities and release procedure. User-facing
installation and operation are documented elsewhere.

## Build Requirements

- Bash 4 or newer.
- Docker with buildx.
- `git`.
- QEMU/binfmt when the Linux builder compiles the non-native `nft` target.
- `gh` only for the publication phase.

The build does not require a host Go toolchain. Go, nFPM, Alpine, and the
runtime assembly inputs use image references pinned by digest. The public
release build should run on one builder host because prepared state records
local runtime image IDs which publication verifies on that same Docker target.

## Component Model

Vaka has exactly two release identities:

1. **CLI version.** Stable releases use an explicit canonical `vX.Y.Z`. There
   is no CLI version file; `build.sh --cli-version` stamps the selected value
   into the binary. Nightlies use the 12-character lowercase Git commit ID.
2. **Runtime bundle version.** The stable base is committed in
   `internal/runtimebundle/VERSION`. It covers `vaka-init`, the bundled `nft`
   executable, the internal `/opt/vaka` image layout, the container-visible
   `/vaka` mount contract and modes, and the generated policy contract consumed
   inside containers.

There is deliberately no separate `vaka-init` version. It cannot be selected
independently from the generated policy and bundled `nft`, so another public
version would create invalid combinations without useful flexibility.

`nft` is even narrower: it is an internal build dependency of the runtime
bundle. `build.sh` derives a SHA-256 fingerprint from `nft/Dockerfile` and uses
the local cache tag `vaka-internal/nft-static:<fingerprint>-<arch>`. That image
is never pushed, included in a manifest, put in a host package, or attached to
a GitHub release. Its version and fingerprint are recorded in runtime labels
and `dist/component-manifest.json`.

### When To Bump The CLI

Choose the next CLI `vX.Y.Z` for every stable Vaka release, including releases
containing only host orchestration, parser, Compose, registry, documentation,
or packaging changes. Pass it to the release command; do not edit source to
record it.

### When To Bump The Runtime

Bump `internal/runtimebundle/VERSION` in the same commit whenever shipped
runtime content or its compatibility contract changes, including:

- `cmd/vaka-init` behavior or runtime dependencies;
- `pkg/nft`, `pkg/policy`, or generated policy fields/semantics used by
  `vaka-init`;
- the bundled nftables version, source verification, build base, or output;
- the runtime Go toolchain pin when it changes the selected runtime inputs;
- `docker/init/Dockerfile`, runtime paths, file modes, labels, or image-mount
  contract;
- a security fix whose enforcement depends on changed in-container bytes.

Use SemVer to communicate the runtime change:

- **Patch:** compatible implementation, dependency, bundled `nft`, or security
  fix.
- **Minor:** backward-compatible runtime capability or policy-contract
  extension; while the runtime is `0.x`, also use a minor bump for an
  incompatible contract or layout change.
- **Major:** incompatible behavior, policy contract, or filesystem layout after
  the runtime reaches `1.0.0`.

Do not bump the runtime for host-only CLI behavior, Compose dispatch, recipe
registry work, documentation, packaging, or tests. Test-only Go files are
excluded from shipped-component fingerprints.

When nftables itself changes, update its version and verified hashes in
`nft/Dockerfile` and bump the runtime version. Do not maintain a separate Vaka
`nft` version or manually copy a cache fingerprint; the build derives it.

Stable CLI releases use the committed runtime version exactly. Nightlies derive
an effective runtime identity automatically:

```text
<stable-runtime-base>-nightly.<12-character-git-id>
```

For example, base `v0.1.0` at commit `0123456789ab` becomes
`v0.1.0-nightly.0123456789ab`. Do not edit `VERSION` for a nightly. This keeps
the nightly CLI, generated policy, runtime image, and `vaka-init` exact even
when several nightly releases share the same stable base. Nightlies never
update `emsi/vaka-init:latest`.

## Local Builds

For the normal development loop, build the native host targets:

```bash
./build.sh
```

The following controls can expand or force parts of that build:

```bash
./build.sh --release
./build.sh --packages
./build.sh --rebuild-cli
./build.sh --rebuild-runtime
./build.sh --rebuild-nft
```

`--release` selects the complete Linux/macOS CLI and Linux runtime architecture
matrix. `--packages` adds CLI-only Debian, RPM, and Arch packages. The rebuild
controls are independent; `--rebuild-go` remains an alias for rebuilding the
CLI and `vaka-init`. Normal cache reuse validates both the input fingerprint
and output binary hash, so a file with the expected name is not sufficient.

Linux and Homebrew packages contain only the host `vaka` CLI. `vaka-init` and
`nft` remain raw internal build outputs used to assemble the runtime image.
Linux packages install only `/usr/local/bin/vaka`.

## Testing A Versioned Release Build

There are two non-publishing workflows. Choose according to whether the test is
about the built artifacts or the complete release-preparation procedure.

Export the intended, unreleased CLI version once, replacing the placeholder.
Every stable-release command below reuses it:

```bash
export RELEASE_VERSION=vX.Y.Z
: "${RELEASE_VERSION:?export RELEASE_VERSION to the intended vX.Y.Z}"
```

### Build And Inspect Artifacts

On an ordinary development checkout, build version-stamped artifacts with:

```bash
./build.sh --release --packages --cli-version "${RELEASE_VERSION}"
```

This builds the full architecture matrix, local runtime images, and Linux
packages. The resulting CLI reports the selected version, making this path
suitable for local installation, packaging, and behavior tests. It does not
require a clean checkout and does not run the release vulnerability scan, full
release test gate, image-mount smoke test, registry preflight, or release-asset
preparation.

The build records the source commit and dirty state in its prepared metadata.
The build itself never publishes images, Git tags, GitHub releases, or Homebrew
changes. Legacy `--push` and `--manifest` options are rejected.

### Exercise Complete Release Preparation

On a host satisfying all release-builder requirements, qualify the exact
release candidate without publishing it:

```bash
./release.sh --version "${RELEASE_VERSION}" --prepare-only
```

This path requires a clean checkout. In addition to building the exact release
matrix and packages, it runs `govulncheck`, the full Go test suite, the real
image-mount smoke test, and the read-only runtime registry preflight. It also
creates the Homebrew archives, checksums, release-asset list, and bound release
gates needed by the publication phase.

Preparation reads the runtime registry but does not modify it and does not
create Git tags, GitHub releases, or Homebrew changes. Stop after this command
when the purpose is only to test the release procedure. A candidate intended
for later publication must remain on the same builder host and Docker target.

## Prepared Metadata

Every successful build writes:

- `dist/component-manifest.json`: CLI version and artifact hashes, stable and
  effective runtime versions, runtime image IDs, source fingerprints, bundled
  nftables metadata, and host package contents.
- `dist/.vaka-release-state`: strict machine-readable state consumed by runtime
  preflight/publication.

Release preparation additionally writes an exact artifact list, `SHA256SUMS`,
and release-gate state binding the Git commit, build state, checksums, Go tests,
vulnerability scan, image-mount smoke, and registry preflight. Publication
fails if any bound file changes.

## Stable Release

Push the clean release commit to an origin branch first. Do not create the Git
tag manually; the publish phase creates it only after immutable runtime
publication succeeds.

Run preparation on the release builder:

```bash
./release.sh --version "${RELEASE_VERSION}" --prepare-only
```

Preparation:

1. requires a clean source tree;
2. runs the pinned `govulncheck` gate and full Go test suite;
3. builds the amd64/arm64 Linux runtime and Linux/macOS CLI matrix;
4. builds CLI-only Debian, RPM, Arch, and Homebrew artifacts;
5. runs the real read-only image-mount smoke test;
6. performs a read-only registry preflight;
7. records bound prepared state.

It does not push images, create manifests or tags, call GitHub, or update the
Homebrew tap. Registry reads are required for the immutability preflight.

After reviewing the prepared output, publish on the same builder and Docker
target:

```bash
./release.sh --version "${RELEASE_VERSION}" --publish-prepared
```

The publish phase validates all prepared state again, preflights external Git,
GitHub, and Homebrew prerequisites, repeats runtime registry preflight, then:

1. pushes only missing immutable runtime architecture tags;
2. creates the immutable multi-platform runtime manifest when absent;
3. verifies its exact platform/digest children;
4. updates runtime `:latest` for a stable release;
5. creates and pushes the CLI Git tag;
6. creates or repairs the GitHub release assets;
7. updates and pushes the Homebrew formula and submodule pointer.

Preparation and publication may also run in one invocation:

```bash
./release.sh --version "${RELEASE_VERSION}"
```

Keeping the two commands is recommended for a deliberate review point.

## Nightly Release

Nightlies use the same phases and derive both the CLI Git ID and effective
runtime identity from `HEAD`:

```bash
./release.sh --nightly --prepare-only
./release.sh --nightly --publish-prepared
```

The GitHub release is marked as a prerelease, the nightly Homebrew formula is
updated, and runtime `:latest` remains unchanged.

## Runtime Immutability

The release registry contains only:

- `emsi/vaka-init:runtime-<effective-version>-amd64`;
- `emsi/vaka-init:runtime-<effective-version>-arm64`;
- `emsi/vaka-init:runtime-<effective-version>` as the exact multi-platform
  manifest;
- `emsi/vaka-init:latest` as a stable-only convenience tag.

Before any push, `scripts/release-runtime.sh` checks every existing architecture
tag by pulling it and comparing its image config ID with the prepared local ID.
If the version manifest already exists, its platform/digest children must
exactly equal the architecture tags. Unknown registry errors fail closed.

A missing architecture tag may be pushed. An existing different tag or
manifest is never replaced. If stable runtime bytes changed under an existing
version, bump `internal/runtimebundle/VERSION`; do not repair the tag in place.
Publication retries are safe: matching immutable data is reused, a prior
partial architecture push can be completed, and only mutable `:latest` is
rewritten.

The Vaka CLI reads the selected Docker daemon architecture and resolves the
matching immutable architecture tag, then validates its label and verifies that
its exact local `sha256:` identity is directly inspectable. This distinction is
required by containerd-backed image stores, where a multi-platform image's
platform child can exist as content without being a directly resolvable image
record. Vaka normally passes the complete identity to Compose; Engine 29.0 and
29.1 receive an immutable 40-hex-character compatibility prefix instead.
Vaka retains the complete ID in metadata and never relies on `:latest` for
execution.

### Runtime-Only Registry Maintenance

The runtime registry can be checked or updated independently from a complete
Vaka release:

```bash
./build.sh --preflight-runtime
./build.sh --publish-runtime
```

Both commands consume the exact runtime images and metadata from the preceding
build on the same clean checkout and Docker target. `--preflight-runtime` is
read-only. `--publish-runtime` repeats that preflight and publishes only the
immutable runtime architecture tags, runtime manifest, and stable `:latest`
tag where applicable.

These are advanced runtime-component operations. They do not create or publish
a Vaka CLI tag, GitHub release, release assets, or Homebrew update, and are not
a substitute for `release.sh --publish-prepared` when publishing a complete
release. The distinct `runtime` and `prepared` names make that scope explicit.

## Verification

Run unit and shell contract tests:

```bash
go test ./...
for test_script in scripts/tests/*_test.sh; do "$test_script"; done
```

Run the real image-mount smoke after a native build:

```bash
./build.sh
./scripts/smoke-image-mount.sh
```

The smoke first verifies that a service image containing an image-level
`/vaka` symlink is rejected without leaving a service container or rootfs
probe. It then starts a service through Vaka and verifies that `/vaka` is an
image mount whose source resolves to the exact local runtime image ID, is
read-only, exposes both executables with execute permission, and reports the
selected stable or nightly runtime identity.

Before a runtime-affecting release, also run against the lower supported
matrix: Engine 28 and Compose 2.35.0. Select that Docker target and assert the
exact versions so a newer local installation cannot be mistaken for minimum
coverage:

```bash
DOCKER_HOST=<engine-28-endpoint> \
DOCKER_CONFIG=<compose-2.35-config-directory> \
VAKA_SMOKE_EXPECT_ENGINE_VERSION=<selected-engine-28-version> \
VAKA_SMOKE_EXPECT_COMPOSE_VERSION=2.35.0 \
./scripts/smoke-image-mount.sh
```

Also exercise Engine 29.1.x with Compose 5.0.x. That pair covers the compact
image-ID mount source required by the Engine 29.0/29.1 filesystem-name bug.
Engine 29.2 fixes the daemon bug; Compose 5.1+ expands image-volume sources back
to full IDs, so Vaka deliberately rejects Compose 5.1+ when paired with Engine
29.0 or 29.1.
