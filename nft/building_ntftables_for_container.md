# Building nftables for cross-container reuse

This document describes the build process and portability decisions for producing a reusable `nft` artifact image.

## Goal

Produce an `nft` binary that can be copied into many Linux container base images (Alpine, Ubuntu, Fedora, etc.) via multi-stage Docker builds.

## Versions and provenance

Pinned inputs:

- `nftables` `1.1.6`
- `libnftnl` `1.3.1`
- `libmnl` `1.0.5`

Verification policy in `Dockerfile`:

1. Download official tarballs from Netfilter upstream endpoints.
2. Verify pinned SHA-256 checksums.
3. Verify detached OpenPGP signatures (`.sig`) for each tarball.
4. Pin signer identity by checking Netfilter Core Team key fingerprint:
   - `8C5F7146A1757A65E2422A94D70D1A666ACF2B21`
5. For `libmnl-1.0.5`, also import and pin the historical Netfilter key used for that release:
   - `37D964ACC04981C75500FB9BD55D978A8A1420E4`

References:

- https://www.netfilter.org/projects/nftables/downloads.html
- https://www.netfilter.org/projects/libnftnl/downloads.html
- https://www.netfilter.org/projects/libmnl/downloads.html
- https://www.netfilter.org/files/coreteam-gpg-key-0xD70D1A666ACF2B21.txt
- https://www.netfilter.org/files/coreteam-gpg-key-0xD55D978A8A1420E4.txt

## Dependency chain and why `libmnl` is included

Upstream build metadata for `nftables-1.1.6` requires:

- `libnftnl >= 1.3.1`
- `libmnl >= 1.0.4`

And `libnftnl` itself also depends on `libmnl`.

So even if only `libnftnl` was originally requested, building `libmnl` from source is required for a fully pinned and reproducible source build.

Note on signatures: Netfilter rotated signing keys over time; current `nftables/libnftnl` releases validate with key `8C5F...2B21`, while `libmnl-1.0.5` validates with key `37D9...20E4`.

## Image naming and tags

Publish under:

- `emsi/nft-static`

Tagging policy:

- release tag: `emsi/nft-static:<nftables-version>`
- moving tag: `emsi/nft-static:latest`

For current pinned sources, the release tag is:

- `emsi/nft-static:1.1.6`

## Portability findings: dynamic vs static

### Why one dynamically linked binary is not universal across base images

A dynamically linked Linux ELF binary hardcodes an interpreter path (dynamic loader), such as:

- glibc systems: `/lib64/ld-linux-x86-64.so.2` (or architecture variant)
- musl systems: `/lib/ld-musl-<arch>.so.1`

That means a binary built against one libc family does not generally run on images using another libc family unless you also transplant a compatible loader + libc runtime stack.

### Chosen approach

Build a **fully static** `nft` binary in an Alpine/musl builder stage:

- avoids runtime dependency on host/container libc
- gives the best practical "copy and run" behavior across distro families
- keeps downstream runtime images small and simple

Implementation note: `nftables` uses libtool; for the final `nft` link step, static linking is enforced with `make LDFLAGS="-all-static" src/nft`.

### Feature profile selected for portability

To reduce dependency surface and avoid optional runtime libraries:

- `--with-mini-gmp`
- `--without-cli` (no readline/editline/linenoise)
- `--without-json` (no jansson)
- `--without-xtables`
- `--disable-man-doc`

These settings produce a minimal, non-interactive `nft` CLI suitable for scripted/container use.

## Build and test locally

Use the included script:

```bash
./build_and_test.sh
```

What it does:

- reads `NFTABLES_VERSION` from `Dockerfile`
- builds `--target artifacts`
- tags image as `emsi/nft-static:<version>` and `emsi/nft-static:latest`
- validates:
  - exported binary is statically linked
  - `nft --version` matches the pinned `nftables` version

## Artifacts exported by the build

The Docker build exports:

- `/opt/nftables/bin/nft` (portable static binary)
- `/opt/nftables/BUILDINFO` (versions, checksums, signing key fingerprint)

Final stage name in `Dockerfile`: `artifacts`.

## Consumer patterns (multi-stage)

### Alpine runtime image

```dockerfile
FROM your-nft-builder-image:latest AS nftbuild
FROM alpine:3.21
COPY --from=nftbuild /opt/nftables/bin/nft /usr/local/sbin/nft
ENTRYPOINT ["/usr/local/sbin/nft"]
```

### Ubuntu runtime image

```dockerfile
FROM your-nft-builder-image:latest AS nftbuild
FROM ubuntu:24.04
COPY --from=nftbuild /opt/nftables/bin/nft /usr/local/sbin/nft
ENTRYPOINT ["/usr/local/sbin/nft"]
```

### Fedora runtime image

```dockerfile
FROM your-nft-builder-image:latest AS nftbuild
FROM fedora:42
COPY --from=nftbuild /opt/nftables/bin/nft /usr/local/sbin/nft
ENTRYPOINT ["/usr/local/sbin/nft"]
```

## Operational notes

- `nft` controls kernel netfilter state; container runtime privileges still matter.
- Typical requirement: `CAP_NET_ADMIN` (and sometimes host network namespace, depending on usage).
- This build solves binary portability, not kernel capability/permission constraints.
