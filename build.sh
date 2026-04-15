#!/usr/bin/env bash
# build.sh — Docker-based build for vaka. No local Go toolchain required.
#
# Requires: docker (with Compose v2 for the integration path)
#
# Usage:
#   ./build.sh                  build CLI binaries + emsi/vaka-init image
#   ./build.sh --packages       also produce .deb and .rpm packages via nfpm
#   ARCHS="amd64" ./build.sh    build a single architecture only
#
# Environment overrides:
#   ARCHS          space-separated list of Go arch names (default: amd64 arm64)
#   GOLANG_IMAGE   builder image              (default: golang:1.25-alpine)
#   INIT_IMAGE     vaka-init image name       (default: emsi/vaka-init)
#   NFPM_IMAGE     nfpm packager image        (default: ghcr.io/goreleaser/nfpm:v2)
#
# Output: ./dist/

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
NFPM_IMAGE="${NFPM_IMAGE:-ghcr.io/goreleaser/nfpm:v2}"
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

# ── CLI binaries ──────────────────────────────────────────────────────────────
# Built inside a golang container — no local Go toolchain needed.
# CGO_ENABLED=0 + netgo/osusergo tags produce a fully static binary that
# runs on any Linux distribution without a matching libc.
echo "==> Building vaka CLI..."
for ARCH in $ARCHS; do
    OUT="dist/vaka-linux-${ARCH}"
    printf '    %-32s' "${OUT}"
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
            -o "/dist/vaka-linux-${ARCH}" \
            ./cmd/vaka
    echo "OK"
done
echo ""

# ── Verify static linking ─────────────────────────────────────────────────────
echo "==> Verifying static linking..."
for ARCH in $ARCHS; do
    OUT="dist/vaka-linux-${ARCH}"
    printf '    %-32s' "${OUT}"
    # Run 'file' inside an alpine container so this check works on any host OS.
    result=$(docker run --rm \
        --volume "${SCRIPT_DIR}/dist:/check:ro" \
        alpine:3.21 \
        sh -c "apk add --no-cache --quiet file && file /check/vaka-linux-${ARCH}")
    if ! echo "${result}" | grep -qi "statically linked"; then
        echo "FAIL"
        echo "      ${result}" >&2
        echo "ERROR: binary is not statically linked" >&2
        exit 1
    fi
    echo "OK (statically linked)"
done
echo ""

# ── Docker image (emsi/vaka-init) ─────────────────────────────────────────────
# The image contains two static binaries:
#   /opt/vaka/bin/vaka-init  (built from this repo)
#   /opt/vaka/bin/nft        (from emsi/nft-static)
echo "==> Building ${RELEASE_TAG}..."
docker build \
    --file docker/init/Dockerfile \
    --build-arg "VERSION=${VERSION}" \
    --tag "${RELEASE_TAG}" \
    --tag "${LATEST_TAG}" \
    .
echo ""

# ── Verify Docker image ───────────────────────────────────────────────────────
echo "==> Verifying ${RELEASE_TAG}..."
cid="$(docker create "${RELEASE_TAG}")"
cleanup_cid() { docker rm -f -- "${cid}" >/dev/null 2>&1 || true; }
trap cleanup_cid EXIT

contents="$(docker export "${cid}" | tar -t 2>/dev/null)"
for expected in opt/vaka/bin/nft opt/vaka/bin/vaka-init; do
    printf '    %-40s' "/${expected}"
    if echo "${contents}" | grep -q "^${expected}$"; then
        echo "OK"
    else
        echo "MISSING"
        echo "ERROR: ${expected} not found in image" >&2
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
        # Write a per-arch nfpm config.
        # nfpm accepts Go-style arch names (amd64, arm64) and translates to
        # deb (amd64, arm64) and rpm (x86_64, aarch64) naming automatically.
        cfg_rel="dist/.nfpm-${ARCH}.yaml"
        cfg_abs="${SCRIPT_DIR}/${cfg_rel}"
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
    printf '  %-40s %s\n' "$f" "$(du -sh "$f" 2>/dev/null | cut -f1)"
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
