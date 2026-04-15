#!/usr/bin/env bash
# build.sh — Docker-based build for vaka. No local Go toolchain required.
#
# Requires: docker
#
# Usage:
#   ./build.sh                  build binaries + emsi/vaka-init image
#   ./build.sh --packages       also produce .deb and .rpm packages via nfpm
#   ./build.sh --rebuild-nft    force rebuild of emsi/nft-static even if present
#   ./build.sh --rebuild-go     force rebuild of Go binaries even if up to date
#   ARCHS="amd64" ./build.sh    build a single architecture only
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
#   nft-linux-<native-arch>  — static nft binary extracted from emsi/nft-static image
#
# When installed via .deb/.rpm the binaries are named:
#   /usr/local/bin/vaka
#   /usr/local/sbin/vaka-init
#   /usr/local/sbin/nft          (native arch only)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${SCRIPT_DIR:?failed to resolve SCRIPT_DIR}"
cd "$SCRIPT_DIR"

# ── Flags ─────────────────────────────────────────────────────────────────────
BUILD_PACKAGES=false
REBUILD_NFT=false
REBUILD_GO=false

for arg in "$@"; do
    case "$arg" in
        --packages)   BUILD_PACKAGES=true ;;
        --rebuild-nft) REBUILD_NFT=true ;;
        --rebuild-go)  REBUILD_GO=true ;;
        *)
            printf 'Unknown argument: %s\nUsage: %s [--packages] [--rebuild-nft] [--rebuild-go]\n' "$arg" "$0" >&2
            exit 1
            ;;
    esac
done

# ── Version ───────────────────────────────────────────────────────────────────
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo "dev")"
PKG_VERSION="${VERSION#v}"

# ── Native architecture ───────────────────────────────────────────────────────
NATIVE_ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"

# ── nft image location ────────────────────────────────────────────────────────
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

NFT_RELEASE_TAG="${NFT_IMAGE}:${NFTABLES_VERSION}"
NFT_ARCH_TAG="${NFT_IMAGE}:${NFTABLES_VERSION}-${NATIVE_ARCH}"
RELEASE_TAG="${INIT_IMAGE}:${VERSION}"
ARCH_RELEASE_TAG="${INIT_IMAGE}:${VERSION}-${NATIVE_ARCH}"
LATEST_TAG="${INIT_IMAGE}:latest"

mkdir -p dist

echo "==> vaka ${VERSION}"
echo ""

# ── Go module/build cache volumes ────────────────────────────────────────────
docker volume create vaka-gomodcache   >/dev/null
docker volume create vaka-gobuildcache >/dev/null

# ── Phase 1: nft image ────────────────────────────────────────────────────────
# Skip if the versioned tag already exists locally. The nft build downloads and
# compiles C sources — it is slow. --rebuild-nft forces a fresh build.
if [[ "${REBUILD_NFT}" == "false" ]] && \
   docker image inspect "${NFT_RELEASE_TAG}" >/dev/null 2>&1; then
    echo "==> Skipping nft build (${NFT_RELEASE_TAG} already present locally)"
    echo "    Use --rebuild-nft to force a rebuild."
else
    echo "==> Building ${NFT_RELEASE_TAG} (nftables ${NFTABLES_VERSION})..."
    docker build \
        --file "${NFT_DIR}/Dockerfile" \
        --target artifacts \
        --tag "${NFT_RELEASE_TAG}" \
        --tag "${NFT_ARCH_TAG}" \
        --tag "${NFT_IMAGE}:latest" \
        "${NFT_DIR}"
fi
echo ""

# ── Extract nft binary from image into dist/ ──────────────────────────────────
# Always re-extract to ensure dist/nft-linux-<arch> matches the current image,
# even when the build was skipped above.
printf '==> Extracting nft from %s...\n' "${NFT_RELEASE_TAG}"
nft_cid="$(docker create "${NFT_RELEASE_TAG}" /opt/nftables/bin/nft)"
cleanup_nft_cid() { docker rm -f -- "${nft_cid}" >/dev/null 2>&1 || true; }
trap cleanup_nft_cid EXIT
docker cp "${nft_cid}:/opt/nftables/bin/nft" "dist/nft-linux-${NATIVE_ARCH}"
docker rm -f -- "${nft_cid}" >/dev/null 2>&1 || true
trap - EXIT
printf '    dist/nft-linux-%-24s OK\n' "${NATIVE_ARCH}"
echo ""

# ── Phase 2: Go binaries ──────────────────────────────────────────────────────
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
        # Find the oldest output binary; check if any .go is newer
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

# ── Phase 3: vaka-init Docker image ──────────────────────────────────────────
# Pass a minimal build context containing only the two native-arch binaries.
# The Dockerfile is a pure scratch assembly — no Go stage, no external FROM,
# no BuildKit cache fragility.
echo "==> Building ${RELEASE_TAG}..."
ctx="$(mktemp -d)"
cleanup_ctx() { rm -rf -- "${ctx}"; }
trap cleanup_ctx EXIT
cp "dist/vaka-init-linux-${NATIVE_ARCH}" "${ctx}/vaka-init"
cp "dist/nft-linux-${NATIVE_ARCH}"       "${ctx}/nft"

docker build \
    --file docker/init/Dockerfile \
    --build-arg "VERSION=${VERSION}" \
    --build-arg "NFTABLES_VERSION=${NFTABLES_VERSION}" \
    --tag "${RELEASE_TAG}" \
    --tag "${ARCH_RELEASE_TAG}" \
    --tag "${LATEST_TAG}" \
    "${ctx}"

rm -rf -- "${ctx}"
trap - EXIT
echo ""

# ── Phase 4: Verify image ─────────────────────────────────────────────────────
echo "==> Verifying ${RELEASE_TAG}..."
cid="$(docker create "${RELEASE_TAG}" /opt/vaka/bin/vaka-init)"
cleanup_cid() { docker rm -f -- "${cid}" >/dev/null 2>&1 || true; }
trap cleanup_cid EXIT

for expected in opt/vaka/bin/nft opt/vaka/bin/vaka-init; do
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

# ── Linux packages (.deb / .rpm) ──────────────────────────────────────────────
if [[ "${BUILD_PACKAGES}" == "true" ]]; then
    echo "==> Building Linux packages (using ${NFPM_IMAGE})..."

    for ARCH in $ARCHS; do
        cfg_rel="dist/.nfpm-${ARCH}.yaml"
        cfg_abs="${SCRIPT_DIR}/${cfg_rel}"

        nft_entry=""
        if [[ -f "dist/nft-linux-${ARCH}" ]]; then
            nft_entry="  - src: /src/dist/nft-linux-${ARCH}
    dst: /usr/local/sbin/nft
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
    dst: /usr/local/sbin/vaka-init
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

# ── Summary ───────────────────────────────────────────────────────────────────
echo "Build complete."
echo ""
echo "Artifacts:"
while IFS= read -r f; do
    printf '  %-42s %s\n' "$f" "$(du -sh "$f" 2>/dev/null | cut -f1)"
done < <(find dist -maxdepth 1 -not -name '.*' -not -name 'dist' | sort)
echo ""
echo "Docker images:"
echo "  ${NFT_RELEASE_TAG}"
echo "  ${NFT_ARCH_TAG}"
echo "  ${NFT_IMAGE}:latest"
echo "  ${RELEASE_TAG}"
echo "  ${ARCH_RELEASE_TAG}"
echo "  ${LATEST_TAG}"
echo ""
echo "To release:"
echo "  git tag v${PKG_VERSION}"
echo "  git push origin v${PKG_VERSION}"
echo "  docker push ${NFT_RELEASE_TAG}"
echo "  docker push ${NFT_ARCH_TAG}"
echo "  docker push ${RELEASE_TAG}"
echo "  docker push ${ARCH_RELEASE_TAG}"
echo "  docker push ${LATEST_TAG}"
