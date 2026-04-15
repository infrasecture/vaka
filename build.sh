#!/usr/bin/env bash
# build.sh — Docker-based build for vaka. No local Go toolchain required.
#
# Requires: docker
#
# Usage:
#   ./build.sh                  build binaries + emsi/vaka-init image
#   ./build.sh --packages       also produce .deb and .rpm packages via nfpm
#   ARCHS="amd64" ./build.sh    build a single architecture only
#
# Environment overrides:
#   ARCHS          space-separated Go arch names     (default: amd64 arm64)
#   GOLANG_IMAGE   builder image                     (default: golang:1.25-alpine)
#   INIT_IMAGE     vaka-init image name              (default: emsi/vaka-init)
#   NFPM_IMAGE     nfpm packager image               (default: ghcr.io/goreleaser/nfpm:latest)
#
# Output layout in ./dist/:
#   vaka-linux-<arch>        — vaka host CLI, one per requested arch
#   vaka-init-linux-<arch>   — vaka-init container binary, one per requested arch
#   nft-linux-<native-arch>  — static nft binary extracted from emsi/vaka-init image
#
# When installed via .deb/.rpm the binaries are named:
#   /usr/local/bin/vaka
#   /usr/local/sbin/vaka-init
#   /usr/local/sbin/nft          (native arch only)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${SCRIPT_DIR:?failed to resolve SCRIPT_DIR}"
cd "$SCRIPT_DIR"

# ── Version ───────────────────────────────────────────────────────────────────
# Derived from the most recent git tag (e.g. v0.1.0).
# Falls back to "dev" on untagged or shallow clones.
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo "dev")"
# Strip leading 'v' for package metadata fields (0.1.0, not v0.1.0).
PKG_VERSION="${VERSION#v}"

# ── Configurable variables ────────────────────────────────────────────────────
ARCHS="${ARCHS:-amd64 arm64}"
GOLANG_IMAGE="${GOLANG_IMAGE:-golang:1.25-alpine}"
INIT_IMAGE="${INIT_IMAGE:-emsi/vaka-init}"
NFPM_IMAGE="${NFPM_IMAGE:-ghcr.io/goreleaser/nfpm:latest}"
BUILD_PACKAGES=false

for arg in "$@"; do
    case "$arg" in
        --packages) BUILD_PACKAGES=true ;;
        *)
            printf 'Unknown argument: %s\nUsage: %s [--packages]\n' "$arg" "$0" >&2
            exit 1
            ;;
    esac
done

RELEASE_TAG="${INIT_IMAGE}:${VERSION}"
LATEST_TAG="${INIT_IMAGE}:latest"

mkdir -p dist

echo "==> vaka ${VERSION}"
echo ""

# ── CLI + init binaries ───────────────────────────────────────────────────────
# Both binaries are built with CGO_ENABLED=0 + netgo/osusergo tags, producing
# fully static executables with no shared library dependencies.
echo "==> Building vaka and vaka-init binaries..."
for ARCH in $ARCHS; do
    for TARGET in vaka vaka-init; do
        OUT="dist/${TARGET}-linux-${ARCH}"
        PKG_PATH="./cmd/${TARGET}"
        printf '    %-36s' "${OUT}"
        docker run --rm \
            --volume "${SCRIPT_DIR}:/src:ro" \
            --volume "${SCRIPT_DIR}/dist:/dist" \
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
echo ""

# ── Verify static linking ─────────────────────────────────────────────────────
# Run 'file' inside Alpine so this check works on any host OS (macOS, etc.).
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

# ── Docker image (emsi/vaka-init) ─────────────────────────────────────────────
# The image contains two static binaries built for the native host architecture:
#   /opt/vaka/bin/vaka-init   (from this repo)
#   /opt/vaka/bin/nft         (from emsi/nft-static)
echo "==> Building ${RELEASE_TAG}..."
docker build \
    --file docker/init/Dockerfile \
    --build-arg "VERSION=${VERSION}" \
    --tag "${RELEASE_TAG}" \
    --tag "${LATEST_TAG}" \
    .
echo ""

# ── Verify image + extract nft ────────────────────────────────────────────────
# Detect the native architecture so we can label the extracted nft correctly.
NATIVE_ARCH="$(docker info --format '{{.Architecture}}' \
    | sed 's/x86_64/amd64/; s/aarch64/arm64/')"

echo "==> Verifying ${RELEASE_TAG} and extracting nft..."
cid="$(docker create "${RELEASE_TAG}" /opt/vaka/bin/vaka-init)"
cleanup_cid() { docker rm -f -- "${cid}" >/dev/null 2>&1 || true; }
trap cleanup_cid EXIT

for expected in opt/vaka/bin/nft opt/vaka/bin/vaka-init; do
    printf '    /%-39s' "${expected}"
    if docker export "${cid}" | tar -t 2>/dev/null | grep -q "^${expected}$"; then
        echo "OK"
    else
        echo "MISSING"
        echo "ERROR: ${expected} not found in image" >&2
        exit 1
    fi
done

# Extract nft — a static C binary built for the native arch inside the image.
# Cross-arch nft binaries require docker buildx and are not produced here.
printf '    %-40s' "dist/nft-linux-${NATIVE_ARCH}"
docker cp "${cid}:/opt/vaka/bin/nft" "dist/nft-linux-${NATIVE_ARCH}"
echo "OK (extracted from image)"

docker rm -f -- "${cid}" >/dev/null 2>&1 || true
trap - EXIT
echo ""

# ── Linux packages (.deb / .rpm) ──────────────────────────────────────────────
# Installed names (no arch suffix):
#   /usr/local/bin/vaka
#   /usr/local/sbin/vaka-init
#   /usr/local/sbin/nft          (included only when built for the native arch)
if [[ "${BUILD_PACKAGES}" == "true" ]]; then
    echo "==> Building Linux packages (using ${NFPM_IMAGE})..."

    for ARCH in $ARCHS; do
        cfg_rel="dist/.nfpm-${ARCH}.yaml"
        cfg_abs="${SCRIPT_DIR}/${cfg_rel}"

        # Include nft only when it was extracted for this architecture.
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
echo "  ${RELEASE_TAG}"
echo "  ${LATEST_TAG}"
echo ""
echo "To release:"
echo "  git tag v${PKG_VERSION}"
echo "  git push origin v${PKG_VERSION}"
echo "  docker push ${RELEASE_TAG}"
echo "  docker push ${LATEST_TAG}"
