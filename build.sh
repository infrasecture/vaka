#!/usr/bin/env bash
# build.sh — multi-arch Docker-based build for vaka. No local Go toolchain required.
#
# Requires: docker (with buildx)
#
# Usage:
#   ./build.sh                  build native host targets (fast local loop)
#   ./build.sh --release        build full release matrix without publishing
#   ./build.sh --release --cli-version v0.2.0 --packages
#                               build an unpublished, non-dev release locally
#   ./build.sh --preflight-prepared
#                               check the prepared runtime against the registry
#   ./build.sh --publish-prepared
#                               publish an already-prepared runtime after preflight
#   ./build.sh --packages       also produce CLI-only Linux packages via nfpm
#   ./build.sh --rebuild-nft    force rebuild of the internal nft build artifact
#   ./build.sh --rebuild-cli    force rebuild of host CLI binaries
#   ./build.sh --rebuild-runtime
#                               force rebuild of vaka-init and runtime images
#   ./build.sh --rebuild-go     compatibility alias for both Go rebuild flags
#   ARCHS="amd64" ./build.sh    restrict to one architecture
#   CLI_TARGETS="darwin/amd64 darwin/arm64" ARCHS="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')" ./build.sh
#                               build Darwin CLI binaries only, keep runtime on native arch
#   ARCHS="amd64 arm64" CLI_TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64" ./build.sh
#                               build complete matrix explicitly (same targets as --release defaults)
#
# Image tagging model:
#   vaka-internal/nft-static:<input-sha>-<arch> is a local build cache only.
#   emsi/vaka-init:runtime-<version>-<arch> is prepared locally and is the only
#   component image published. The unsuffixed runtime tag is a native local alias
#   during builds and a multi-platform manifest in the registry after release.
#
# Environment overrides:
#   ARCHS          space-separated Go arch names     (default: native host arch; --release: amd64 arm64)
#   CLI_TARGETS    space-separated GOOS/GOARCH pairs for vaka CLI
#                  (default: native host target; --release: linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)
#   GOLANG_IMAGE   pinned builder image              (default: Go 1.25.12 Alpine digest)
#   INIT_IMAGE     vaka-init image name              (default: emsi/vaka-init)
#   NFPM_IMAGE     pinned nfpm packager image         (default: nfpm v2.47.0 digest)
#
# Output layout in ./dist/:
#   vaka-<os>-<arch>         — vaka host CLI (native host target by default)
#   vaka-init-linux-<arch>   — vaka-init container binary, one per requested arch
#   nft-linux-<arch>         — static nft binary, one per requested arch
#
# Host packages contain only /usr/local/bin/vaka. Runtime binaries remain build
# outputs used to assemble the independently versioned runtime image.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${SCRIPT_DIR:?failed to resolve SCRIPT_DIR}"
cd "$SCRIPT_DIR"
source "${SCRIPT_DIR}/scripts/lib/release-versioning.sh"

# ── Flags ─────────────────────────────────────────────────────────────────────
BUILD_PACKAGES=false
REBUILD_NFT=false
REBUILD_CLI=false
REBUILD_RUNTIME=false
RELEASE_MODE=false
CLI_VERSION_ARG=""
RUNTIME_VERSION_ARG=""
PREPARED_ACTION=""

usage() {
    cat <<'EOF'
Usage: ./build.sh [OPTIONS]

  --release                  Build the amd64/arm64 release matrix; never publish.
  --cli-version VERSION      Stamp an explicit stable vX.Y.Z or nightly Git ID.
  --runtime-version VERSION  Override the effective runtime identity (nightlies).
  --packages                 Build CLI-only Linux packages.
  --rebuild-nft              Rebuild the internal nft artifact.
  --rebuild-cli              Rebuild all selected CLI binaries.
  --rebuild-runtime          Rebuild vaka-init and runtime images.
  --rebuild-go               Alias for --rebuild-cli --rebuild-runtime.
  --preflight-prepared       Validate dist/.vaka-release-state and the registry.
  --publish-prepared         Publish only the already-prepared runtime images.
  -h, --help                 Show this help.

Publishing is intentionally separate from building. Legacy --push and
--manifest are rejected so a build cannot mutate the registry accidentally.
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --packages) BUILD_PACKAGES=true; shift ;;
        --rebuild-nft) REBUILD_NFT=true; shift ;;
        --rebuild-cli) REBUILD_CLI=true; shift ;;
        --rebuild-runtime) REBUILD_RUNTIME=true; shift ;;
        --rebuild-go) REBUILD_CLI=true; REBUILD_RUNTIME=true; shift ;;
        --release) RELEASE_MODE=true; shift ;;
        --cli-version)
            [[ $# -ge 2 ]] || { echo "ERROR: --cli-version requires a value" >&2; exit 1; }
            CLI_VERSION_ARG="$2"
            shift 2
            ;;
        --cli-version=*) CLI_VERSION_ARG="${1#*=}"; shift ;;
        --runtime-version)
            [[ $# -ge 2 ]] || { echo "ERROR: --runtime-version requires a value" >&2; exit 1; }
            RUNTIME_VERSION_ARG="$2"
            shift 2
            ;;
        --runtime-version=*) RUNTIME_VERSION_ARG="${1#*=}"; shift ;;
        --preflight-prepared) PREPARED_ACTION=preflight; shift ;;
        --publish-prepared) PREPARED_ACTION=publish; shift ;;
        --push|--manifest)
            echo "ERROR: $1 was removed; build first, then use --preflight-prepared or --publish-prepared" >&2
            exit 1
            ;;
        -h|--help) usage; exit 0 ;;
        *) echo "ERROR: unknown argument: $1" >&2; usage >&2; exit 1 ;;
    esac
done

if [[ -n "${PREPARED_ACTION}" ]]; then
    if [[ "${BUILD_PACKAGES}" == true || "${REBUILD_NFT}" == true || "${REBUILD_CLI}" == true ||
          "${REBUILD_RUNTIME}" == true || "${RELEASE_MODE}" == true || -n "${CLI_VERSION_ARG}" ||
          -n "${RUNTIME_VERSION_ARG}" ]]; then
        echo "ERROR: prepared publication actions cannot be combined with build options" >&2
        exit 1
    fi
    exec "${SCRIPT_DIR}/scripts/release-runtime.sh" "${PREPARED_ACTION}"
fi

# ── Version ───────────────────────────────────────────────────────────────────
cli_version_env="${CLI_VERSION:-}"
legacy_version_env="${VERSION:-}"
if [[ -n "${CLI_VERSION_ARG}" && -n "${cli_version_env}" && "${CLI_VERSION_ARG}" != "${cli_version_env}" ]]; then
    echo "ERROR: --cli-version conflicts with CLI_VERSION" >&2
    exit 1
fi
if [[ -n "${CLI_VERSION_ARG}" ]]; then
    CLI_VERSION="${CLI_VERSION_ARG}"
elif [[ -n "${cli_version_env}" ]]; then
    CLI_VERSION="${cli_version_env}"
elif [[ -n "${legacy_version_env}" ]]; then
    CLI_VERSION="${legacy_version_env}"
else
    CLI_VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo "dev")"
fi

explicit_cli_version=false
if [[ -n "${CLI_VERSION_ARG}" || -n "${cli_version_env}" || -n "${legacy_version_env}" ]]; then
    explicit_cli_version=true
fi
if [[ "${explicit_cli_version}" == true ]]; then
    vaka_require_release_cli_version "${CLI_VERSION}"
fi
PKG_VERSION="${CLI_VERSION#v}"
RUNTIME_VERSION_FILE="${SCRIPT_DIR}/internal/runtimebundle/VERSION"
if [[ ! -f "${RUNTIME_VERSION_FILE}" ]]; then
    echo "ERROR: runtime bundle version file not found: ${RUNTIME_VERSION_FILE}" >&2
    exit 1
fi
RUNTIME_BASE_VERSION="$(<"${RUNTIME_VERSION_FILE}")"
vaka_require_stable_version "runtime bundle version" "${RUNTIME_BASE_VERSION}"

if vaka_is_stable_version "${CLI_VERSION}"; then
    RELEASE_CHANNEL=stable
elif vaka_is_nightly_cli_version "${CLI_VERSION}"; then
    RELEASE_CHANNEL=nightly
else
    RELEASE_CHANNEL=development
fi

if [[ -n "${RUNTIME_VERSION_ARG}" && -n "${RUNTIME_VERSION_OVERRIDE:-}" &&
      "${RUNTIME_VERSION_ARG}" != "${RUNTIME_VERSION_OVERRIDE}" ]]; then
    echo "ERROR: --runtime-version conflicts with RUNTIME_VERSION_OVERRIDE" >&2
    exit 1
fi
if [[ -n "${RUNTIME_VERSION_ARG}" ]]; then
    RUNTIME_VERSION="${RUNTIME_VERSION_ARG}"
elif [[ -n "${RUNTIME_VERSION_OVERRIDE:-}" ]]; then
    RUNTIME_VERSION="${RUNTIME_VERSION_OVERRIDE}"
elif [[ "${RELEASE_CHANNEL}" == nightly ]]; then
    RUNTIME_VERSION="$(vaka_nightly_runtime_version "${RUNTIME_BASE_VERSION}" "${CLI_VERSION}")"
else
    RUNTIME_VERSION="${RUNTIME_BASE_VERSION}"
fi
vaka_is_effective_runtime_version "${RUNTIME_VERSION}" || {
    echo "ERROR: invalid effective runtime version: ${RUNTIME_VERSION}" >&2
    exit 1
}
case "${RELEASE_CHANNEL}" in
    stable)
        [[ "${RUNTIME_VERSION}" == "${RUNTIME_BASE_VERSION}" ]] || {
            echo "ERROR: a stable CLI release must use committed runtime ${RUNTIME_BASE_VERSION}" >&2
            exit 1
        }
        ;;
    nightly)
        expected_runtime="$(vaka_nightly_runtime_version "${RUNTIME_BASE_VERSION}" "${CLI_VERSION}")"
        [[ "${RUNTIME_VERSION}" == "${expected_runtime}" ]] || {
            echo "ERROR: nightly runtime must be ${expected_runtime}" >&2
            exit 1
        }
        ;;
    development)
        [[ "${RUNTIME_VERSION}" == "${RUNTIME_BASE_VERSION}" ]] || {
            echo "ERROR: development builds use committed runtime ${RUNTIME_BASE_VERSION}" >&2
            exit 1
        }
        ;;
esac
RUNTIME_TAG="runtime-${RUNTIME_VERSION}"
# Keep runtime image identity stable across release hosts and CLI-only commits.
# 1980-01-01 is accepted by tar implementations used throughout the build.
RUNTIME_SOURCE_DATE_EPOCH=315532800

# ── Native architecture ───────────────────────────────────────────────────────
NATIVE_ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
HOST_OS="$(uname -s)"

# ── nft Dockerfile location ───────────────────────────────────────────────────
NFT_DIR="${SCRIPT_DIR}/nft"

if [[ ! -f "${NFT_DIR}/Dockerfile" ]]; then
    echo "ERROR: nft/Dockerfile not found at ${NFT_DIR}" >&2
    exit 1
fi

NFTABLES_VERSION="$(awk -F= '/^ARG NFTABLES_VERSION=/{print $2; exit}' "${NFT_DIR}/Dockerfile")"
: "${NFTABLES_VERSION:?could not detect NFTABLES_VERSION from nft/Dockerfile}"

# ── Configurable variables ────────────────────────────────────────────────────
if [[ "${RELEASE_MODE}" == "true" ]]; then
    default_archs="amd64 arm64"
    default_cli_targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"
else
    default_archs="${NATIVE_ARCH}"
    if [[ "${HOST_OS}" == "Darwin" ]]; then
        default_cli_targets="darwin/${NATIVE_ARCH}"
    else
        # Linux and other non-Darwin hosts default to Linux CLI outputs.
        default_cli_targets="linux/${NATIVE_ARCH}"
    fi
fi

ARCHS="${ARCHS:-${default_archs}}"
CLI_TARGETS="${CLI_TARGETS:-${default_cli_targets}}"
GOLANG_IMAGE="${GOLANG_IMAGE:-golang:1.25.12-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587}"
VERIFY_IMAGE="${VERIFY_IMAGE:-alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d}"
NFT_INTERNAL_IMAGE="${NFT_INTERNAL_IMAGE:-vaka-internal/nft-static}"
INIT_IMAGE="${INIT_IMAGE:-emsi/vaka-init}"
NFPM_IMAGE="${NFPM_IMAGE:-ghcr.io/goreleaser/nfpm:v2.47.0@sha256:a662cb167d7b6d3a83920c83d76b12d02b8ac5dd2c13e5c62c15270b23f6df0c}"

declare -A seen_archs=()
for ARCH in ${ARCHS}; do
    case "${ARCH}" in
        amd64|arm64) ;;
        *) echo "ERROR: unsupported runtime architecture: ${ARCH}" >&2; exit 1 ;;
    esac
    [[ -z "${seen_archs[${ARCH}]:-}" ]] || { echo "ERROR: duplicate runtime architecture: ${ARCH}" >&2; exit 1; }
    seen_archs["${ARCH}"]=1
done
[[ "${#seen_archs[@]}" -gt 0 ]] || { echo "ERROR: ARCHS must not be empty" >&2; exit 1; }

for target in ${CLI_TARGETS}; do
    [[ "${target}" =~ ^(linux|darwin)/(amd64|arm64)$ ]] || {
        echo "ERROR: unsupported CLI target: ${target}" >&2
        exit 1
    }
done
[[ -n "${CLI_TARGETS//[[:space:]]/}" ]] || { echo "ERROR: CLI_TARGETS must not be empty" >&2; exit 1; }

hash_build_inputs() {
    local namespace="$1"
    local values="$2"
    shift 2
    (
        printf '%s\0%s\0' "${namespace}" "${values}"
        cd "${SCRIPT_DIR}"
        local path
        for path in "$@"; do
            [[ -f "${path}" ]] || {
                printf 'ERROR: build fingerprint input missing: %s\n' "${path}" >&2
                exit 1
            }
            printf '%s\0' "${path}"
            cat -- "${path}"
            printf '\0'
        done
    ) | vaka_sha256_stream
}

mapfile -d '' cli_source_files < <(
    find cmd/vaka pkg internal -type f \( -name '*.go' -o -name '*.tmpl' -o -name VERSION \) \
        ! -name '*_test.go' -print0 | LC_ALL=C sort -z
)
mapfile -d '' runtime_source_files < <(
    find cmd/vaka-init pkg/nft pkg/policy internal/runtimebundle -type f \
        \( -name '*.go' -o -name '*.tmpl' -o -name VERSION \) \
        ! -name '*_test.go' -print0 | LC_ALL=C sort -z
)

NFT_INPUTS_SHA256="$(vaka_nft_inputs_sha256 "${SCRIPT_DIR}" "${NFTABLES_VERSION}")"
CLI_INPUTS_SHA256="$(hash_build_inputs cli \
    "cli=${CLI_VERSION};runtime=${RUNTIME_VERSION};builder=${GOLANG_IMAGE}" \
    go.mod go.sum "${cli_source_files[@]}")"
RUNTIME_INPUTS_SHA256="$(hash_build_inputs runtime \
    "runtime=${RUNTIME_VERSION};nft=${NFT_INPUTS_SHA256};builder=${GOLANG_IMAGE}" \
    go.mod go.sum docker/init/Dockerfile nft/Dockerfile "${runtime_source_files[@]}")"
NFT_INTERNAL_TAG_PREFIX="${NFT_INTERNAL_IMAGE}:${NFT_INPUTS_SHA256}"

mkdir -p dist

echo "==> vaka ${CLI_VERSION}; runtime bundle ${RUNTIME_VERSION} (runtime archs: ${ARCHS}; CLI targets: ${CLI_TARGETS})"
echo ""

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

    # Non-Linux hosts (e.g. macOS Docker Desktop) do not expose Linux
    # binfmt handlers under /proc on the host. Let buildx handle emulation.
    if [[ "${HOST_OS}" != "Linux" ]]; then
        return 0
    fi

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

# ── Phase 1: internal nft artifacts — one per arch ───────────────────────────
# Uses docker buildx build --platform to set correct OCI platform metadata.
# C compilation for a foreign arch (e.g. arm64 on amd64) requires QEMU binfmt.
# The QEMU check is skipped when the image is already present (cache hit).
#
# The image is a local, content-addressed build cache. It is never part of the
# release namespace and is never pushed.
for ARCH in $ARCHS; do
    arch_nft_tag="${NFT_INTERNAL_TAG_PREFIX}-${ARCH}"
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
    echo ""
done

# ── Phase 2: Extract nft binaries — one per arch ─────────────────────────────
# docker create on a non-native arch image works because no code is executed;
# docker cp just reads the layer filesystem.
echo "==> Extracting nft binaries..."
declare -A nft_binary_sha256=()
for ARCH in $ARCHS; do
    arch_nft_tag="${NFT_INTERNAL_TAG_PREFIX}-${ARCH}"
    printf '    dist/nft-linux-%-10s' "${ARCH}"
    nft_cid="$(docker create --platform "linux/${ARCH}" "${arch_nft_tag}" /opt/nftables/bin/nft)"
    cleanup_nft_cid() { docker rm -f -- "${nft_cid}" >/dev/null 2>&1 || true; }
    trap cleanup_nft_cid EXIT
    docker cp "${nft_cid}:/opt/nftables/bin/nft" "dist/nft-linux-${ARCH}"
    docker rm -f -- "${nft_cid}" >/dev/null 2>&1 || true
    trap - EXIT
    nft_binary_sha256["${ARCH}"]="$(vaka_sha256_file "dist/nft-linux-${ARCH}")"
    echo "OK"
done
echo ""

# ── Phase 3: Go binaries — independently content-keyed ────────────────────────
artifact_cache_hit() {
    local output="$1"
    local stamp="$2"
    local expected_inputs="$3"
    local stamped_inputs stamped_output actual_output
    [[ -f "${output}" && -f "${stamp}" ]] || return 1
    stamped_inputs="$(awk -F= '$1 == "INPUTS" { print substr($0, 8); exit }' "${stamp}")"
    stamped_output="$(awk -F= '$1 == "OUTPUT" { print substr($0, 8); exit }' "${stamp}")"
    [[ "${stamped_inputs}" == "${expected_inputs}" && "${stamped_output}" =~ ^[0-9a-f]{64}$ ]] || return 1
    actual_output="$(vaka_sha256_file "${output}")"
    [[ "${actual_output}" == "${stamped_output}" ]]
}

write_artifact_stamp() {
    local output="$1"
    local stamp="$2"
    local inputs="$3"
    local tmp_stamp="${stamp}.tmp.$$"
    printf 'INPUTS=%s\nOUTPUT=%s\n' "${inputs}" "$(vaka_sha256_file "${output}")" >"${tmp_stamp}"
    mv -f -- "${tmp_stamp}" "${stamp}"
}

echo "==> Building vaka CLI binaries as needed..."
for target in ${CLI_TARGETS}; do
    GOOS="${target%%/*}"
    GOARCH="${target##*/}"
    OUT="dist/vaka-${GOOS}-${GOARCH}"
    stamp="dist/.vaka-cli-${GOOS}-${GOARCH}.stamp"
    if [[ "${REBUILD_CLI}" == false ]] && artifact_cache_hit "${OUT}" "${stamp}" "${CLI_INPUTS_SHA256}"; then
        printf '    %-36s cached\n' "${OUT}"
        continue
    fi
    printf '    %-36s' "${OUT}"
    docker run --rm \
        --volume "${SCRIPT_DIR}:/src:ro" \
        --volume "${SCRIPT_DIR}/dist:/dist" \
        --volume "vaka-gomodcache:/go/pkg/mod" \
        --volume "vaka-gobuildcache:/root/.cache/go/build" \
        --workdir /src \
        --env CGO_ENABLED=0 \
        --env GOOS="${GOOS}" \
        --env GOARCH="${GOARCH}" \
        --env GOWORK=off \
        "${GOLANG_IMAGE}" \
        go build \
            -buildvcs=false \
            -trimpath \
            -tags "netgo,osusergo" \
            -ldflags="-s -w -extldflags=-static -X main.version=${CLI_VERSION} -X vaka.dev/vaka/internal/runtimebundle.buildVersion=${RUNTIME_VERSION}" \
            -o "/dist/vaka-${GOOS}-${GOARCH}" \
            ./cmd/vaka
    write_artifact_stamp "${OUT}" "${stamp}" "${CLI_INPUTS_SHA256}"
    echo "OK"
done

echo "==> Building vaka-init binaries as needed..."
for ARCH in $ARCHS; do
    OUT="dist/vaka-init-linux-${ARCH}"
    stamp="dist/.vaka-init-linux-${ARCH}.stamp"
    if [[ "${REBUILD_RUNTIME}" == false ]] && artifact_cache_hit "${OUT}" "${stamp}" "${RUNTIME_INPUTS_SHA256}"; then
        printf '    %-36s cached\n' "${OUT}"
        continue
    fi
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
            -buildvcs=false \
            -trimpath \
            -tags "netgo,osusergo" \
            -ldflags="-s -w -extldflags=-static -X vaka.dev/vaka/internal/runtimebundle.buildVersion=${RUNTIME_VERSION}" \
            -o "/dist/vaka-init-linux-${ARCH}" \
            ./cmd/vaka-init
    write_artifact_stamp "${OUT}" "${stamp}" "${RUNTIME_INPUTS_SHA256}"
    echo "OK"
done
echo ""

# ── Verify binary outputs and formats ─────────────────────────────────────────
echo "==> Verifying binary outputs and formats..."
# Fail fast if expected binaries are missing.
for target in ${CLI_TARGETS}; do
    GOOS="${target%%/*}"
    GOARCH="${target##*/}"
    out="dist/vaka-${GOOS}-${GOARCH}"
    if [[ ! -f "${out}" ]]; then
        echo "ERROR: missing expected CLI binary: ${out}" >&2
        exit 1
    fi
done
for ARCH in $ARCHS; do
    out="dist/vaka-init-linux-${ARCH}"
    if [[ ! -f "${out}" ]]; then
        echo "ERROR: missing expected runtime binary: ${out}" >&2
        exit 1
    fi
done

docker run --rm \
    --volume "${SCRIPT_DIR}/dist:/check:ro" \
    "${VERIFY_IMAGE}" \
    sh -c '
        apk add --no-cache --quiet file
        ok=true

        # Linux binaries must be statically linked ELF.
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

        # Darwin binaries should be Mach-O executables.
        for f in /check/vaka-darwin-*; do
            [ -f "$f" ] || continue
            name="${f##*/}"
            result=$(file "$f")
            if echo "$result" | grep -Eqi "Mach-O .* executable"; then
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
runtime_image_cache_hit() {
    local ref="$1"
    local arch="$2"
    local init_sha nft_sha
    init_sha="$(vaka_sha256_file "dist/vaka-init-linux-${arch}")"
    nft_sha="${nft_binary_sha256[${arch}]}"
    [[ "${REBUILD_RUNTIME}" == false ]] || return 1
    docker image inspect "${ref}" >/dev/null 2>&1 || return 1
    [[ "$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.runtime.version"}}')" == "${RUNTIME_VERSION}" ]] || return 1
    [[ "$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.runtime.inputs-sha256"}}')" == "${RUNTIME_INPUTS_SHA256}" ]] || return 1
    [[ "$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.runtime.vaka-init-sha256"}}')" == "${init_sha}" ]] || return 1
    [[ "$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.nftables.version"}}')" == "${NFTABLES_VERSION}" ]] || return 1
    [[ "$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.nftables.inputs-sha256"}}')" == "${NFT_INPUTS_SHA256}" ]] || return 1
    [[ "$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.nftables.binary-sha256"}}')" == "${nft_sha}" ]]
}

# The native-arch unsuffixed alias lets the freshly built CLI run locally
# without a registry round trip. Only release-runtime.sh creates registry tags.
for ARCH in $ARCHS; do
    arch_init_tag="${INIT_IMAGE}:${RUNTIME_TAG}-${ARCH}"
    if runtime_image_cache_hit "${arch_init_tag}" "${ARCH}"; then
        echo "==> Skipping runtime image build for ${ARCH} (${arch_init_tag} matches inputs)"
    else
        echo "==> Building ${arch_init_tag} (platform linux/${ARCH})..."
        ctx="$(mktemp -d)"
        cleanup_ctx() { rm -rf -- "${ctx}"; }
        trap cleanup_ctx EXIT
        runtime_root="${ctx}/rootfs"
        mkdir -p "${runtime_root}/opt/vaka/sbin"
        cp "dist/vaka-init-linux-${ARCH}" "${runtime_root}/opt/vaka/sbin/vaka-init"
        cp "dist/nft-linux-${ARCH}"       "${runtime_root}/opt/vaka/sbin/nft"
        find "${runtime_root}" -exec env TZ=UTC touch -t 198001010000 {} +

        docker buildx build \
            --no-cache \
            --platform "linux/${ARCH}" \
            --output "type=docker,rewrite-timestamp=true" \
            --file docker/init/Dockerfile \
            --build-arg "RUNTIME_VERSION=${RUNTIME_VERSION}" \
            --build-arg "NFTABLES_VERSION=${NFTABLES_VERSION}" \
            --build-arg "RUNTIME_INPUTS_SHA256=${RUNTIME_INPUTS_SHA256}" \
            --build-arg "NFT_INPUTS_SHA256=${NFT_INPUTS_SHA256}" \
            --build-arg "VAKA_INIT_SHA256=$(vaka_sha256_file "dist/vaka-init-linux-${ARCH}")" \
            --build-arg "NFT_BINARY_SHA256=${nft_binary_sha256[${ARCH}]}" \
            --build-arg "SOURCE_DATE_EPOCH=${RUNTIME_SOURCE_DATE_EPOCH}" \
            --tag "${arch_init_tag}" \
            "${ctx}"

        rm -rf -- "${ctx}"
        trap - EXIT
    fi
    if [[ "${ARCH}" == "${NATIVE_ARCH}" ]]; then
        docker tag "${arch_init_tag}" "${INIT_IMAGE}:${RUNTIME_TAG}"
        echo "    Tagged native-arch alias: ${INIT_IMAGE}:${RUNTIME_TAG}"
    fi
    echo ""
done

# ── Phase 5: Verify every prepared runtime image ──────────────────────────────
for verify_arch in ${ARCHS}; do
    verify_tag="${INIT_IMAGE}:${RUNTIME_TAG}-${verify_arch}"
    echo "==> Verifying ${verify_tag}..."
    cid="$(docker create --platform "linux/${verify_arch}" "${verify_tag}" /opt/vaka/sbin/vaka-init)"
    cleanup_cid() { docker rm -f -- "${cid}" >/dev/null 2>&1 || true; }
    trap cleanup_cid EXIT

    image_listing="$(docker export "${cid}" | tar -tv 2>/dev/null)"
    for expected in opt/vaka/sbin/nft opt/vaka/sbin/vaka-init; do
        printf '    /%-39s' "${expected}"
        # docker export produces tar entries with an optional './' prefix; strip it
        # before matching so both './opt/...' and 'opt/...' formats are handled.
        entry="$(printf '%s\n' "${image_listing}" | awk -v path="${expected}" '$NF == path || $NF == "./" path { print; exit }')"
        if [[ -n "${entry}" ]]; then
            permissions="${entry%% *}"
            if [[ "${permissions}" != "-r-xr-xr-x" ]]; then
                echo "BAD MODE (${permissions})"
                echo "ERROR: /${expected} must have mode 0555" >&2
                exit 1
            fi
            echo "OK (0555)"
        else
            echo "MISSING"
            echo "ERROR: ${expected} not found in image" >&2
            echo "--- image contents ---" >&2
            docker export "${cid}" | tar -t 2>/dev/null | sed 's|^\./||' | grep -v '/$' >&2 || true
            exit 1
        fi
    done

    printf '    %-40s' "runtime version label"
    image_runtime_version="$(docker image inspect "${verify_tag}" --format '{{index .Config.Labels "agent.vaka.runtime.version"}}')"
    if [[ "${image_runtime_version}" != "${RUNTIME_VERSION}" ]]; then
        echo "MISMATCH (${image_runtime_version})"
        exit 1
    fi
    echo "OK (${RUNTIME_VERSION})"

    printf '    %-40s' "runtime inputs label"
    image_runtime_inputs="$(docker image inspect "${verify_tag}" --format '{{index .Config.Labels "agent.vaka.runtime.inputs-sha256"}}')"
    [[ "${image_runtime_inputs}" == "${RUNTIME_INPUTS_SHA256}" ]] || { echo "MISMATCH (${image_runtime_inputs})"; exit 1; }
    echo "OK (${RUNTIME_INPUTS_SHA256})"

    printf '    %-40s' "nft inputs label"
    image_nft_inputs="$(docker image inspect "${verify_tag}" --format '{{index .Config.Labels "agent.vaka.nftables.inputs-sha256"}}')"
    [[ "${image_nft_inputs}" == "${NFT_INPUTS_SHA256}" ]] || { echo "MISMATCH (${image_nft_inputs})"; exit 1; }
    echo "OK (${NFT_INPUTS_SHA256})"

    printf '    %-40s' "vaka-init binary label"
    image_init_sha="$(docker image inspect "${verify_tag}" --format '{{index .Config.Labels "agent.vaka.runtime.vaka-init-sha256"}}')"
    expected_init_sha="$(vaka_sha256_file "dist/vaka-init-linux-${verify_arch}")"
    [[ "${image_init_sha}" == "${expected_init_sha}" ]] || { echo "MISMATCH (${image_init_sha})"; exit 1; }
    echo "OK (${image_init_sha})"

    printf '    %-40s' "nft binary label"
    image_nft_sha="$(docker image inspect "${verify_tag}" --format '{{index .Config.Labels "agent.vaka.nftables.binary-sha256"}}')"
    [[ "${image_nft_sha}" == "${nft_binary_sha256[${verify_arch}]}" ]] || { echo "MISMATCH (${image_nft_sha})"; exit 1; }
    echo "OK (${image_nft_sha})"

    printf '    %-40s' "legacy image volume"
    image_volumes="$(docker image inspect "${verify_tag}" --format '{{json .Config.Volumes}}')"
    if [[ "${image_volumes}" != "null" && "${image_volumes}" != "{}" ]]; then
        echo "PRESENT (${image_volumes})"
        echo "ERROR: runtime image must not declare VOLUME /opt/vaka" >&2
        exit 1
    fi
    echo "absent"

    if [[ "${verify_arch}" == "${NATIVE_ARCH}" ]]; then
        printf '    %-40s' "vaka-init --version"
        reported_runtime_version="$(docker run --rm --platform "linux/${verify_arch}" "${verify_tag}" /opt/vaka/sbin/vaka-init --version)"
        if [[ "${reported_runtime_version}" != "${RUNTIME_VERSION}" ]]; then
            echo "MISMATCH (${reported_runtime_version})"
            exit 1
        fi
        echo "OK (${reported_runtime_version})"
    fi

    docker rm -f -- "${cid}" >/dev/null 2>&1 || true
    trap - EXIT
    echo ""
done

# ── Phase 6: Linux packages (.deb / .rpm / .pkg.tar.zst) ─────────────────────
if [[ "${BUILD_PACKAGES}" == "true" ]]; then
    echo "==> Building Linux packages (using ${NFPM_IMAGE})..."

    for ARCH in $ARCHS; do
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
license: LGPL-2.1-only
contents:
  - src: /src/dist/vaka-linux-${ARCH}
    dst: /usr/local/bin/vaka
    file_info:
      mode: 0755
NFPM

        for PKG_TYPE in deb rpm archlinux; do
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

# ── Phase 7: Record exact prepared component identities ───────────────────────
json_quote() {
    local value="$1"
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    value="${value//$'\n'/\\n}"
    printf '"%s"' "${value}"
}

git_commit="$(git rev-parse --verify HEAD)"
git_short="$(git rev-parse --short=12 HEAD)"
git_dirty=false
[[ -z "$(git status --porcelain)" ]] || git_dirty=true

if [[ -n "${seen_archs[amd64]:-}" && -n "${seen_archs[arm64]:-}" ]]; then
    archs_csv=amd64,arm64
elif [[ -n "${seen_archs[amd64]:-}" ]]; then
    archs_csv=amd64
else
    archs_csv=arm64
fi
cli_targets_csv="$(tr ' ' ',' <<<"${CLI_TARGETS}" | sed -E 's/,+/,/g; s/^,//; s/,$//')"

mapfile -d '' package_artifacts < <(
    find dist -maxdepth 1 -type f \
        \( -name "vaka_${PKG_VERSION}_*.deb" -o -name "vaka-${PKG_VERSION}-*.rpm" \
           -o -name "vaka-${PKG_VERSION}-*.pkg.tar.*" -o -name "vaka_${PKG_VERSION}_*.pkg.tar.*" \) \
        -print0 | LC_ALL=C sort -z
)

component_manifest="dist/component-manifest.json"
component_tmp="${component_manifest}.tmp.$$"
{
    printf '{\n  "schemaVersion": 1,\n'
    printf '  "source": {"gitCommit": '; json_quote "${git_commit}"
    printf ', "dirty": %s},\n' "${git_dirty}"
    printf '  "cli": {"version": '; json_quote "${CLI_VERSION}"
    printf ', "channel": '; json_quote "${RELEASE_CHANNEL}"
    printf ', "inputsSha256": '; json_quote "${CLI_INPUTS_SHA256}"
    printf ', "targets": ['
    first=true
    for target in ${CLI_TARGETS}; do
        GOOS="${target%%/*}"
        GOARCH="${target##*/}"
        path="dist/vaka-${GOOS}-${GOARCH}"
        [[ "${first}" == true ]] || printf ','
        printf '\n      {"platform": '; json_quote "${target}"
        printf ', "path": '; json_quote "${path}"
        printf ', "sha256": '; json_quote "$(vaka_sha256_file "${path}")"
        printf '}'
        first=false
    done
    printf '\n    ]},\n'
    printf '  "runtime": {"baseVersion": '; json_quote "${RUNTIME_BASE_VERSION}"
    printf ', "effectiveVersion": '; json_quote "${RUNTIME_VERSION}"
    printf ', "inputsSha256": '; json_quote "${RUNTIME_INPUTS_SHA256}"
    printf ', "imageRepository": '; json_quote "${INIT_IMAGE}"
    printf ', "nft": {"version": '; json_quote "${NFTABLES_VERSION}"
    printf ', "inputsSha256": '; json_quote "${NFT_INPUTS_SHA256}"
    printf '}, "images": ['
    first=true
    for ARCH in ${ARCHS}; do
        ref="${INIT_IMAGE}:${RUNTIME_TAG}-${ARCH}"
        image_id="$(docker image inspect "${ref}" --format '{{.Id}}')"
        [[ "${first}" == true ]] || printf ','
        printf '\n      {"platform": '; json_quote "linux/${ARCH}"
        printf ', "reference": '; json_quote "${ref}"
        printf ', "imageId": '; json_quote "${image_id}"
        printf ', "vakaInitSha256": '; json_quote "$(vaka_sha256_file "dist/vaka-init-linux-${ARCH}")"
        printf ', "nftSha256": '; json_quote "$(vaka_sha256_file "dist/nft-linux-${ARCH}")"
        printf '}'
        first=false
    done
    printf '\n    ]},\n'
    printf '  "hostPackages": {"contents": ["vaka"], "artifacts": ['
    first=true
    for path in "${package_artifacts[@]}"; do
        [[ "${first}" == true ]] || printf ','
        printf '\n      {"path": '; json_quote "${path}"
        printf ', "sha256": '; json_quote "$(vaka_sha256_file "${path}")"
        printf '}'
        first=false
    done
    printf '\n    ]}\n}\n'
} >"${component_tmp}"
mv -f -- "${component_tmp}" "${component_manifest}"

state_file="dist/.vaka-release-state"
state_tmp="${state_file}.tmp.$$"
{
    printf 'FORMAT=1\n'
    printf 'GIT_COMMIT=%s\n' "${git_commit}"
    printf 'GIT_SHORT=%s\n' "${git_short}"
    printf 'GIT_DIRTY=%s\n' "${git_dirty}"
    printf 'CHANNEL=%s\n' "${RELEASE_CHANNEL}"
    printf 'CLI_VERSION=%s\n' "${CLI_VERSION}"
    printf 'CLI_INPUTS_SHA256=%s\n' "${CLI_INPUTS_SHA256}"
    printf 'CLI_TARGETS=%s\n' "${cli_targets_csv}"
    printf 'RUNTIME_BASE_VERSION=%s\n' "${RUNTIME_BASE_VERSION}"
    printf 'RUNTIME_EFFECTIVE_VERSION=%s\n' "${RUNTIME_VERSION}"
    printf 'RUNTIME_TAG=%s\n' "${RUNTIME_TAG}"
    printf 'RUNTIME_INPUTS_SHA256=%s\n' "${RUNTIME_INPUTS_SHA256}"
    printf 'NFTABLES_VERSION=%s\n' "${NFTABLES_VERSION}"
    printf 'NFT_INPUTS_SHA256=%s\n' "${NFT_INPUTS_SHA256}"
    printf 'INIT_IMAGE=%s\n' "${INIT_IMAGE}"
    printf 'ARCHS=%s\n' "${archs_csv}"
    for ARCH in ${ARCHS}; do
        state_key="RUNTIME_IMAGE_${ARCH^^}"
        state_key="${state_key//-/_}"
        printf '%s=%s\n' "${state_key}" \
            "$(docker image inspect "${INIT_IMAGE}:${RUNTIME_TAG}-${ARCH}" --format '{{.Id}}')"
        printf 'VAKA_INIT_BINARY_%s_SHA256=%s\n' "${ARCH^^}" \
            "$(vaka_sha256_file "dist/vaka-init-linux-${ARCH}")"
        printf 'NFT_BINARY_%s_SHA256=%s\n' "${ARCH^^}" "${nft_binary_sha256[${ARCH}]}"
    done
    printf 'COMPONENT_MANIFEST_SHA256=%s\n' "$(vaka_sha256_file "${component_manifest}")"
} >"${state_tmp}"
mv -f -- "${state_tmp}" "${state_file}"

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
    echo "  ${INIT_IMAGE}:${RUNTIME_TAG}-${ARCH}"
done
if echo " ${ARCHS} " | grep -qF " ${NATIVE_ARCH} "; then
    echo ""
    echo "Native-arch local aliases (unsuffixed, for local 'vaka up'):"
    echo "  ${INIT_IMAGE}:${RUNTIME_TAG}"
fi
echo ""
echo "Prepared metadata:"
echo "  ${component_manifest}"
echo "  ${state_file}"
echo ""
if [[ "${RELEASE_CHANNEL}" == stable || "${RELEASE_CHANNEL}" == nightly ]]; then
    echo "No registry data was changed. Next steps on this same builder host:"
    echo "  ./build.sh --preflight-prepared"
    echo "  ./build.sh --publish-prepared"
fi
