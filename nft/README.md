# Internal nft Build

This directory builds the static `nft` executable embedded in Vaka's runtime
bundle. It is not a separately released Vaka component.

`build.sh` creates a local, architecture-specific build artifact named:

```text
vaka-internal/nft-static:<input-sha256>-<arch>
```

It extracts `/opt/nftables/bin/nft` from that image and copies the binary into
the versioned `emsi/vaka-init` runtime image. The internal image is never pushed,
given a multi-platform manifest, included in a host package, or attached to a
GitHub release.

## Pinned Inputs

The Dockerfile currently builds:

- nftables `1.1.6`;
- libnftnl `1.3.1`;
- libmnl `1.0.5`.

Each upstream tarball is checked against a pinned SHA-256 digest and detached
OpenPGP signature. The expected signer fingerprints are declared in the
Dockerfile. The Alpine build base is pinned by image digest.

The fingerprint in the local image tag is derived automatically from the
Dockerfile and selected nftables version. It is a cache identity, not a version
maintainers edit or publish.

## Why Static

A dynamically linked Linux executable depends on the target image's loader and
libc ABI. Vaka mounts the same runtime into services using different base
distributions, so `nft` is linked fully statically with:

```bash
make LDFLAGS="-all-static" src/nft
```

## Developer Check

Run the native build and static/version checks with:

```bash
./nft/build_and_test.sh
```

The resulting image remains local and internal. The normal multi-architecture
release build is driven by the repository-level `build.sh`.

Changing nftables, its dependencies, verification inputs, build base, or output
changes the runtime bundle. Update `nft/Dockerfile` and bump
`internal/runtimebundle/VERSION` in the same commit. There is no independent
Vaka nft version.
