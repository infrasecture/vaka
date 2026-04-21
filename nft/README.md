# emsi/nft-static

Portable static `nft` CLI image built from verified upstream Netfilter sources.

The image is designed to be used as a build artifact source in multi-stage Dockerfiles:

- copy `/opt/nftables/bin/nft` into your target image
- use the same binary across Alpine, Ubuntu, Fedora, and other Linux container bases

## What this image provides

- `nftables` CLI pinned to `1.1.6`
- static binary output (`/opt/nftables/bin/nft`)
- reproducible source build with checksum + signature verification
- build metadata file at `/opt/nftables/BUILDINFO`

## Versions and dependencies

Pinned source versions:

- `nftables` `1.1.6`
- `libnftnl` `1.3.1`
- `libmnl` `1.0.5`

Dependency chain:

- `nftables -> libnftnl -> libmnl`

`libmnl` is included intentionally so the full build chain is pinned and reproducible.

## Security and provenance

The build verifies each source tarball in two ways:

1. pinned SHA-256 digest
2. detached OpenPGP signature (`.sig`)

Pinned signer fingerprints:

- current Netfilter Core Team key (used for current `nftables`/`libnftnl`):
  - `8C5F7146A1757A65E2422A94D70D1A666ACF2B21`
- historical Netfilter Core Team key (used for `libmnl-1.0.5`):
  - `37D964ACC04981C75500FB9BD55D978A8A1420E4`

Upstream references:

- https://www.netfilter.org/projects/nftables/downloads.html
- https://www.netfilter.org/projects/libnftnl/downloads.html
- https://www.netfilter.org/projects/libmnl/downloads.html
- https://www.netfilter.org/files/coreteam-gpg-key-0xD70D1A666ACF2B21.txt
- https://www.netfilter.org/files/coreteam-gpg-key-0xD55D978A8A1420E4.txt

## Why static

A dynamically linked Linux binary depends on the runtime loader and libc ABI of the target image. In practice this breaks cross-libc portability (glibc vs musl).

This project builds `nft` as fully static, so downstream images do not need compatible runtime libc/loader stacks just to run the CLI.

Implementation detail: because `nftables` links through libtool, static linking is enforced with:

```bash
make LDFLAGS="-all-static" src/nft
```

## Build and test

Run:

```bash
./build_and_test.sh
```

The script:

- reads `NFTABLES_VERSION` from [Dockerfile](Dockerfile)
- builds `--target artifacts`
- tags:
  - `emsi/nft-static:<nftables-version>`
  - `emsi/nft-static:latest`
- validates:
  - binary is statically linked
  - `nft --version` matches the pinned nftables version

## Publish

For the current pinned version:

```bash
docker push emsi/nft-static:1.1.6
docker push emsi/nft-static:latest
```

## Use in other images (multi-stage)

Example:

```dockerfile
FROM emsi/nft-static:1.1.6 AS nftbuild
FROM ubuntu:24.04
COPY --from=nftbuild /opt/nftables/bin/nft /opt/vaka/sbin/nft
ENTRYPOINT ["/opt/vaka/sbin/nft"]
```

Also works with Alpine/Fedora-style targets using the same copy path.

## Runtime notes

- `nft` manages kernel netfilter state, so runtime privileges still apply.
- Typical requirement: `CAP_NET_ADMIN`.
- If you need host firewall control, networking/namespace setup must match your deployment model.
