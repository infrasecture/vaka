#!/usr/bin/env bash
# build.sh — multi-arch Docker-based build for vaka. No local Go toolchain required.
#
# Requires: docker (with buildx)
#
# Usage:
#   ./build.sh                  build for all ARCHS locally (arch-specific tags)
#   ./build.sh --push           build, push arch tags, create manifest lists
#   ./build.sh --manifest       create manifest lists from already-pushed arch images
#   ./build.sh --packages       also produce .deb and .rpm packages via nfpm
#   ./build.sh --rebuild-nft    force rebuild of emsi/nft-static images
#   ./build.sh --rebuild-go     force rebuild of Go binaries even if up to date
#   ARCHS="amd64" ./build.sh    restrict to one architecture
#
# Multi-arch publishing — single host (QEMU handles foreign-arch nft C build):
#   sudo apt-get install -y qemu-user-static   # Debian/Ubuntu
#   # or: docker run --rm --privileged tonistiigi/binfmt --install all
#   ./build.sh --push
#
# Multi-arch publishing — separate native hosts (no QEMU needed):
#   ARCHS=amd64 ./build.sh --push   # on amd64 host
#   ARCHS=arm64 ./build.sh --push   # on arm64 host
#   ./build.sh --manifest            # on any host after both are pushed
#
# Image tagging model:
#   Arch-specific (local + push staging):
#     emsi/nft-static:1.1.6-amd64,  emsi/nft-static:1.1.6-arm64
#     emsi/vaka-init:v0.1.0-amd64,  emsi/vaka-init:v0.1.0-arm64
#
#   Native-arch local alias (unsuffixed, created during every local build):
#     emsi/nft-static:1.1.6    → points at the native-arch image only
#     emsi/vaka-init:v0.1.0    → points at the native-arch image only
#   The alias makes the unsuffixed ref resolvable locally without a registry
#   round-trip. vaka's CLI constructs emsi/vaka-init:<version> at runtime
#   from its baked-in version string; this alias is what lets dirty dev
#   builds run locally without pushing.
#
#   Manifest lists (registry only, created by --push or --manifest):
#     emsi/nft-static:1.1.6    → auto-selects amd64 or arm64 at pull time
#     emsi/nft-static:latest   → auto-selects amd64 or arm64 at pull time
#     emsi/vaka-init:v0.1.0    → auto-selects amd64 or arm64 at pull time
#     emsi/vaka-init:latest    → auto-selects amd64 or arm64 at pull time
#
# Environment overrides:
#   ARCHS          space-separated Go arch names     (default: amd64 arm64)
#   GOLANG_IMAGE   builder image                     (default: golang:1.25-alpine)
#   INIT_IMAGE     vaka-init image name              (default: emsi/vaka-init)
#   NFT_IMAGE      nft-static image name             (default: emsi/nft-static)
#   NFPM_IMAGE     nfpm packager image               (default: ghcr.io/goreleaser/nfpm:latest)
#
# Output layout in ./dist/:
#   vaka-linux-<arch>        — vaka host CLI, one per requested arch
#   vaka-init-linux-<arch>   — vaka-init container binary, one per requested arch
#   nft-linux-<arch>         — static nft binary, one per requested arch
#
# When installed via .deb/.rpm the binaries are named:
#   /usr/local/bin/vaka
#   /opt/vaka/sbin/vaka-init
#   /opt/vaka/sbin/nft

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${SCRIPT_DIR:?failed to resolve SCRIPT_DIR}"
cd "$SCRIPT_DIR"

# ── Flags ─────────────────────────────────────────────────────────────────────
BUILD_PACKAGES=false
REBUILD_NFT=false
REBUILD_GO=false
DO_PUSH=false
DO_MANIFEST_ONLY=false

for arg in "$@"; do
    case "$arg" in
        --packages)    BUILD_PACKAGES=true ;;
        --rebuild-nft) REBUILD_NFT=true ;;
        --rebuild-go)  REBUILD_GO=true ;;
        --push)        DO_PUSH=true ;;
        --manifest)    DO_MANIFEST_ONLY=true ;;
        *)
            printf 'Unknown argument: %s\nUsage: %s [--push] [--manifest] [--packages] [--rebuild-nft] [--rebuild-go]\n' "$arg" "$0" >&2
            exit 1
            ;;
    esac
done

if [[ "${DO_PUSH}" == "true" && "${DO_MANIFEST_ONLY}" == "true" ]]; then
    echo "ERROR: --push and --manifest are mutually exclusive" >&2
    exit 1
fi

# ── Version ───────────────────────────────────────────────────────────────────
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}"
PKG_VERSION="${VERSION#v}"

if [[ "${DO_PUSH}" == "true" || "${DO_MANIFEST_ONLY}" == "true" ]] && \
   [[ "${VERSION}" == *"-dirty" ]]; then
    printf 'ERROR: Cannot push with a dirty working tree (version: %s).\n' "${VERSION}" >&2
    printf 'Commit or stash your changes first.\n' >&2
    exit 1
fi

# ── Native architecture ───────────────────────────────────────────────────────
NATIVE_ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"

# ── nft Dockerfile location ───────────────────────────────────────────────────
GIT_COMMON_DIR="$(cd "$(git rev-parse --git-common-dir)" && pwd)"
MAIN_REPO_ROOT="$(dirname "${GIT_COMMON_DIR}")"
NFT_DIR="${MAIN_REPO_ROOT}/nft"

if [[ ! -f "${NFT_DIR}/Dockerfile" ]]; then
    echo "ERROR: nft/Dockerfile not found at ${NFT_DIR}" >&2
    exit 1
fi

NFTABLES_VERSION="$(awk -F= '/^ARG NFTABLES_VERSION=/{print $2; exit}' "${NFT_DIR}/Dockerfile")"
: "${NFTABLES_VERSION:?could not detect NFTABLES_VERSION from nft/Dockerfile}"

# ── Configurable variables ────────────────────────────────────────────────────
ARCHS="${ARCHS:-amd64 arm64}"
GOLANG_IMAGE="${GOLANG_IMAGE:-golang:1.25-alpine}"
NFT_IMAGE="${NFT_IMAGE:-emsi/nft-static}"
INIT_IMAGE="${INIT_IMAGE:-emsi/vaka-init}"
NFPM_IMAGE="${NFPM_IMAGE:-ghcr.io/goreleaser/nfpm:latest}"

# Tag scheme:
#   Arch-specific (local + push staging): emsi/nft-static:1.1.6-amd64
#   Manifest lists (registry only):       emsi/nft-static:1.1.6
# Arch tags are computed inside loops; manifest tags are assembled at push time.

mkdir -p dist

echo "==> vaka ${VERSION} (archs: ${ARCHS})"
echo ""

# ─────────────────────────────────────────────────────────────────────────────
# --manifest only: assemble manifest lists from already-pushed arch images.
# Run this after pushing from separate native hosts (no build performed).
# ─────────────────────────────────────────────────────────────────────────────
if [[ "${DO_MANIFEST_ONLY}" == "true" ]]; then
    echo "==> Creating manifest lists (archs: ${ARCHS})..."

    nft_sources=()
    init_sources=()
    for ARCH in $ARCHS; do
        nft_sources+=("${NFT_IMAGE}:${NFTABLES_VERSION}-${ARCH}")
        init_sources+=("${INIT_IMAGE}:${VERSION}-${ARCH}")
    done

    printf '    %s\n' "${NFT_IMAGE}:${NFTABLES_VERSION}"
    docker buildx imagetools create \
        --tag "${NFT_IMAGE}:${NFTABLES_VERSION}" \
        --tag "${NFT_IMAGE}:latest" \
        "${nft_sources[@]}"

    printf '    %s\n' "${INIT_IMAGE}:${VERSION}"
    docker buildx imagetools create \
        --tag "${INIT_IMAGE}:${VERSION}" \
        --tag "${INIT_IMAGE}:latest" \
        "${init_sources[@]}"

    echo ""
    echo "Manifest lists created in registry:"
    printf '  %s   (%s)\n' "${NFT_IMAGE}:${NFTABLES_VERSION}" "${ARCHS}"
    printf '  %s  (%s)\n'  "${NFT_IMAGE}:latest"              "${ARCHS}"
    printf '  %s   (%s)\n' "${INIT_IMAGE}:${VERSION}"         "${ARCHS}"
    printf '  %s  (%s)\n'  "${INIT_IMAGE}:latest"             "${ARCHS}"
    exit 0
fi

# ── Go module/build cache volumes ────────────────────────────────────────────
docker volume create vaka-gomodcache   >/dev/null
docker volume create vaka-gobuildcache >/dev/null

# ── QEMU check helper ─────────────────────────────────────────────────────────
# Exits with a clear error if ARCH requires QEMU for C compilation (nft image)
# and the relevant binfmt handler is not registered.
# Not needed for vaka-init images (FROM scratch + COPY — no RUN instructions).
require_qemu_for_arch() {
    local arch="$1"
    [[ "${arch}" == "${NATIVE_ARCH}" ]] && return 0

    # Map Go arch names to the qemu-binfmt names used in /proc/sys/fs/binfmt_misc/
    local qemu_arch
    case "${arch}" in
        arm64)   qemu_arch="aarch64" ;;
        amd64)   qemu_arch="x86_64" ;;
        arm)     qemu_arch="arm" ;;
        s390x)   qemu_arch="s390x" ;;
        ppc64le) qemu_arch="ppc64le" ;;
        386)     qemu_arch="i386" ;;
        *)       qemu_arch="${arch}" ;;
    esac

    if [[ -f "/proc/sys/fs/binfmt_misc/qemu-${qemu_arch}" ]]; then
        return 0
    fi

    printf '\nERROR: Building the nft image for linux/%s on a %s host requires QEMU binfmt.\n' \
        "${arch}" "${NATIVE_ARCH}" >&2
    printf '\n' >&2
    printf '  Register QEMU binfmt handlers — choose one (one-time, persists until reboot):\n' >&2
    printf '    sudo apt-get install -y qemu-user-static          # Debian/Ubuntu\n' >&2
    printf '    docker run --rm --privileged tonistiigi/binfmt --install all  # any host with Docker\n' >&2
    printf '\n' >&2
    printf '  Or build natively on a %s host instead:\n' "${arch}" >&2
    printf '    ARCHS=%s ./build.sh\n' "${arch}" >&2
    exit 1
}

# ── Phase 1: nft images — one per arch ───────────────────────────────────────
# Uses docker buildx build --platform to set correct OCI platform metadata.
# C compilation for a foreign arch (e.g. arm64 on amd64) requires QEMU binfmt.
# The QEMU check is skipped when the image is already present (cache hit).
#
# Native-arch alias: after building the native-arch image, also tag it as the
# unsuffixed tag (emsi/nft-static:VERSION). Consumers of the image by its
# public tag then work locally without the manifest list that only
# --push/--manifest creates in the registry.
for ARCH in $ARCHS; do
    arch_nft_tag="${NFT_IMAGE}:${NFTABLES_VERSION}-${ARCH}"
    if [[ "${REBUILD_NFT}" == "false" ]] && \
       docker image inspect "${arch_nft_tag}" >/dev/null 2>&1; then
        echo "==> Skipping nft build for ${ARCH} (${arch_nft_tag} already present locally)"
        echo "    Use --rebuild-nft to force a rebuild."
    else
        require_qemu_for_arch "${ARCH}"
        echo "==> Building ${arch_nft_tag} (nftables ${NFTABLES_VERSION}, platform linux/${ARCH})..."
        docker buildx build \
            --platform "linux/${ARCH}" \
            --load \
            --file "${NFT_DIR}/Dockerfile" \
            --target artifacts \
            --tag "${arch_nft_tag}" \
            "${NFT_DIR}"
    fi
    if [[ "${ARCH}" == "${NATIVE_ARCH}" ]]; then
        docker tag "${arch_nft_tag}" "${NFT_IMAGE}:${NFTABLES_VERSION}"
        echo "    Tagged native-arch alias: ${NFT_IMAGE}:${NFTABLES_VERSION}"
    fi
    echo ""
done

# ── Phase 2: Extract nft binaries — one per arch ─────────────────────────────
# docker create on a non-native arch image works because no code is executed;
# docker cp just reads the layer filesystem.
echo "==> Extracting nft binaries..."
for ARCH in $ARCHS; do
    arch_nft_tag="${NFT_IMAGE}:${NFTABLES_VERSION}-${ARCH}"
    printf '    dist/nft-linux-%-10s' "${ARCH}"
    nft_cid="$(docker create --platform "linux/${ARCH}" "${arch_nft_tag}" /opt/nftables/bin/nft)"
    cleanup_nft_cid() { docker rm -f -- "${nft_cid}" >/dev/null 2>&1 || true; }
    trap cleanup_nft_cid EXIT
    docker cp "${nft_cid}:/opt/nftables/bin/nft" "dist/nft-linux-${ARCH}"
    docker rm -f -- "${nft_cid}" >/dev/null 2>&1 || true
    trap - EXIT
    echo "OK"
done
echo ""

# ── Phase 3: Go binaries — skip if up to date ────────────────────────────────
# Skip if all output files exist AND no .go source is newer than the oldest one.
# --rebuild-go forces a fresh build.
need_go_build=false
if [[ "${REBUILD_GO}" == "true" ]]; then
    need_go_build=true
else
    for ARCH in $ARCHS; do
        for TARGET in vaka vaka-init; do
            out="dist/${TARGET}-linux-${ARCH}"
            if [[ ! -f "${out}" ]]; then
                need_go_build=true
                break 2
            fi
        done
    done
    if [[ "${need_go_build}" == "false" ]]; then
        oldest_out=""
        for ARCH in $ARCHS; do
            for TARGET in vaka vaka-init; do
                out="dist/${TARGET}-linux-${ARCH}"
                if [[ -z "${oldest_out}" ]] || \
                   [[ "${out}" -ot "${oldest_out}" ]]; then
                    oldest_out="${out}"
                fi
            done
        done
        if find cmd pkg -name '*.go' -newer "${oldest_out}" | grep -q .; then
            need_go_build=true
        fi
    fi
fi

if [[ "${need_go_build}" == "false" ]]; then
    echo "==> Go binaries up to date, skipping build"
    echo "    Use --rebuild-go to force a rebuild."
else
    echo "==> Building Go binaries..."
    for ARCH in $ARCHS; do
        for TARGET in vaka vaka-init; do
            OUT="dist/${TARGET}-linux-${ARCH}"
            PKG_PATH="./cmd/${TARGET}"
            printf '    %-36s' "${OUT}"
            docker run --rm \
                --volume "${SCRIPT_DIR}:/src:ro" \
                --volume "${SCRIPT_DIR}/dist:/dist" \
                --volume "vaka-gomodcache:/go/pkg/mod" \
                --volume "vaka-gobuildcache:/root/.cache/go/build" \
                --workdir /src \
                --env CGO_ENABLED=0 \
                --env GOOS=linux \
                --env GOARCH="${ARCH}" \
                --env GOWORK=off \
                "${GOLANG_IMAGE}" \
                go build \
                    -trimpath \
                    -tags "netgo,osusergo" \
                    -ldflags="-s -w -extldflags=-static -X main.version=${VERSION}" \
                    -o "/dist/${TARGET}-linux-${ARCH}" \
                    "${PKG_PATH}"
            echo "OK"
        done
    done
fi
echo ""

# ── Verify static linking ─────────────────────────────────────────────────────
echo "==> Verifying static linking..."
docker run --rm \
    --volume "${SCRIPT_DIR}/dist:/check:ro" \
    alpine:3.21 \
    sh -c '
        apk add --no-cache --quiet file
        ok=true
        for f in /check/vaka-linux-* /check/vaka-init-linux-*; do
            [ -f "$f" ] || continue
            name="${f##*/}"
            result=$(file "$f")
            if echo "$result" | grep -qi "statically linked"; then
                printf "    %-36s OK\n" "$name"
            else
                printf "    %-36s FAIL\n" "$name"
                echo "$result" >&2
                ok=false
            fi
        done
        $ok || exit 1
    '
echo ""

# ── Phase 4: vaka-init images — one per arch ─────────────────────────────────
# FROM scratch + COPY does not need QEMU; --platform only sets OCI metadata.
# Each arch gets its own minimal build context with the matching binaries.
#
# Native-arch alias: after building the native-arch image, also tag it as the
# unsuffixed tag (emsi/vaka-init:VERSION). The `vaka` CLI constructs that ref
# at runtime from its baked-in version string; without the alias, local dev
# builds (especially --dirty) would have no tag to resolve to.
for ARCH in $ARCHS; do
    arch_init_tag="${INIT_IMAGE}:${VERSION}-${ARCH}"
    echo "==> Building ${arch_init_tag} (platform linux/${ARCH})..."
    ctx="$(mktemp -d)"
    cleanup_ctx() { rm -rf -- "${ctx}"; }
    trap cleanup_ctx EXIT
    cp "dist/vaka-init-linux-${ARCH}" "${ctx}/vaka-init"
    cp "dist/nft-linux-${ARCH}"       "${ctx}/nft"

    docker buildx build \
        --platform "linux/${ARCH}" \
        --load \
        --file docker/init/Dockerfile \
        --build-arg "VERSION=${VERSION}" \
        --build-arg "NFTABLES_VERSION=${NFTABLES_VERSION}" \
        --tag "${arch_init_tag}" \
        "${ctx}"

    rm -rf -- "${ctx}"
    trap - EXIT
    if [[ "${ARCH}" == "${NATIVE_ARCH}" ]]; then
        docker tag "${arch_init_tag}" "${INIT_IMAGE}:${VERSION}"
        echo "    Tagged native-arch alias: ${INIT_IMAGE}:${VERSION}"
    fi
    echo ""
done

# ── Phase 5: Verify native-arch image ────────────────────────────────────────
# Only verify when the native arch was actually built this run.
# (e.g. ARCHS=arm64 on an amd64 host skips this — the amd64 image wasn't built)
verify_arch="${NATIVE_ARCH}"
if ! echo " ${ARCHS} " | grep -qF " ${verify_arch} "; then
    # Fall back to verifying whatever single arch was requested, if only one
    arch_count=$(echo "${ARCHS}" | wc -w)
    if [[ "${arch_count}" -eq 1 ]]; then
        verify_arch="${ARCHS}"
    else
        echo "==> Skipping image verification (native arch ${NATIVE_ARCH} not in requested ARCHS: ${ARCHS})"
        echo ""
        verify_arch=""
    fi
fi

if [[ -n "${verify_arch}" ]]; then
    verify_tag="${INIT_IMAGE}:${VERSION}-${verify_arch}"
    echo "==> Verifying ${verify_tag}..."
    cid="$(docker create --platform "linux/${verify_arch}" "${verify_tag}" /opt/vaka/sbin/vaka-init)"
    cleanup_cid() { docker rm -f -- "${cid}" >/dev/null 2>&1 || true; }
    trap cleanup_cid EXIT

    for expected in opt/vaka/sbin/nft opt/vaka/sbin/vaka-init; do
        printf '    /%-39s' "${expected}"
        # docker export produces tar entries with an optional './' prefix; strip it
        # before matching so both './opt/...' and 'opt/...' formats are handled.
        if docker export "${cid}" | tar -t 2>/dev/null | sed 's|^\./||' | grep -q "^${expected}$"; then
            echo "OK"
        else
            echo "MISSING"
            echo "ERROR: ${expected} not found in image" >&2
            echo "--- image contents ---" >&2
            docker export "${cid}" | tar -t 2>/dev/null | sed 's|^\./||' | grep -v '/$' >&2 || true
            exit 1
        fi
    done

    docker rm -f -- "${cid}" >/dev/null 2>&1 || true
    trap - EXIT
    echo ""
fi

# ── Phase 6: Linux packages (.deb / .rpm) ────────────────────────────────────
if [[ "${BUILD_PACKAGES}" == "true" ]]; then
    echo "==> Building Linux packages (using ${NFPM_IMAGE})..."

    for ARCH in $ARCHS; do
        cfg_rel="dist/.nfpm-${ARCH}.yaml"
        cfg_abs="${SCRIPT_DIR}/${cfg_rel}"

        nft_entry=""
        if [[ -f "dist/nft-linux-${ARCH}" ]]; then
            nft_entry="  - src: /src/dist/nft-linux-${ARCH}
    dst: /opt/vaka/sbin/nft
    file_info:
      mode: 0755"
        fi

        cat > "${cfg_abs}" <<NFPM
name: vaka
arch: ${ARCH}
platform: linux
version: "${PKG_VERSION}"
maintainer: Mariusz Woloszyn
description: |
  vaka is a secure container layer that enforces an egress firewall inside
  Docker containers. Run 'vaka up' instead of 'docker compose up' to
  restrict each service's outbound network access to a declared allowlist.
homepage: https://github.com/infrasecture/vaka
license: MIT
contents:
  - src: /src/dist/vaka-linux-${ARCH}
    dst: /usr/local/bin/vaka
    file_info:
      mode: 0755
  - src: /src/dist/vaka-init-linux-${ARCH}
    dst: /opt/vaka/sbin/vaka-init
    file_info:
      mode: 0755
${nft_entry}
NFPM

        for PKG_TYPE in deb rpm; do
            printf '    %-6s (%s)  ' "${PKG_TYPE}" "${ARCH}"
            docker run --rm \
                --volume "${SCRIPT_DIR}:/src:ro" \
                --volume "${SCRIPT_DIR}/dist:/dist" \
                "${NFPM_IMAGE}" \
                package \
                    --config "/src/${cfg_rel}" \
                    --packager "${PKG_TYPE}" \
                    --target /dist/
            echo "OK"
        done
    done
    echo ""
fi

# ── Phase 7: Push arch images + create manifest lists (--push only) ───────────
if [[ "${DO_PUSH}" == "true" ]]; then
    echo "==> Pushing arch-specific images..."
    nft_sources=()
    init_sources=()

    for ARCH in $ARCHS; do
        arch_nft_tag="${NFT_IMAGE}:${NFTABLES_VERSION}-${ARCH}"
        arch_init_tag="${INIT_IMAGE}:${VERSION}-${ARCH}"

        printf '    %s  ' "${arch_nft_tag}"
        docker push "${arch_nft_tag}"
        echo "OK"

        printf '    %s  ' "${arch_init_tag}"
        docker push "${arch_init_tag}"
        echo "OK"

        nft_sources+=("${arch_nft_tag}")
        init_sources+=("${arch_init_tag}")
    done
    echo ""

    echo "==> Creating manifest lists..."
    printf '    %s\n' "${NFT_IMAGE}:${NFTABLES_VERSION}"
    docker buildx imagetools create \
        --tag "${NFT_IMAGE}:${NFTABLES_VERSION}" \
        --tag "${NFT_IMAGE}:latest" \
        "${nft_sources[@]}"

    printf '    %s\n' "${INIT_IMAGE}:${VERSION}"
    docker buildx imagetools create \
        --tag "${INIT_IMAGE}:${VERSION}" \
        --tag "${INIT_IMAGE}:latest" \
        "${init_sources[@]}"

    echo ""
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo "Build complete."
echo ""
echo "Artifacts in dist/:"
while IFS= read -r f; do
    printf '  %-42s %s\n' "$f" "$(du -sh "$f" 2>/dev/null | cut -f1)"
done < <(find dist -maxdepth 1 -not -name '.*' -not -name 'dist' | sort)
echo ""
echo "Local images (arch-specific staging tags):"
for ARCH in $ARCHS; do
    echo "  ${NFT_IMAGE}:${NFTABLES_VERSION}-${ARCH}"
    echo "  ${INIT_IMAGE}:${VERSION}-${ARCH}"
done
if echo " ${ARCHS} " | grep -qF " ${NATIVE_ARCH} "; then
    echo ""
    echo "Native-arch local aliases (unsuffixed, for local 'vaka up'):"
    echo "  ${NFT_IMAGE}:${NFTABLES_VERSION}"
    echo "  ${INIT_IMAGE}:${VERSION}"
fi
echo ""

if [[ "${DO_PUSH}" == "true" ]]; then
    echo "Registry manifest lists (multi-arch, auto-selected by 'docker pull'):"
    echo "  ${NFT_IMAGE}:${NFTABLES_VERSION}"
    echo "  ${NFT_IMAGE}:latest"
    echo "  ${INIT_IMAGE}:${VERSION}"
    echo "  ${INIT_IMAGE}:latest"
else
    echo "To publish (single host with QEMU):"
    echo "  sudo apt-get install -y qemu-user-static   # Debian/Ubuntu"
    echo "  # or: docker run --rm --privileged tonistiigi/binfmt --install all"
    echo "  ./build.sh --push"
    echo ""
    echo "To publish (native hosts, no QEMU needed):"
    echo "  ARCHS=amd64 ./build.sh --push   # on amd64 host"
    echo "  ARCHS=arm64 ./build.sh --push   # on arm64 host"
    echo "  ./build.sh --manifest            # on any host after both are pushed"
fi
