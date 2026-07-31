#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

assert_contains() {
    local path="$1"
    local pattern="$2"
    grep -Fq -- "${pattern}" "${path}" || fail "${path} does not contain: ${pattern}"
}

tmp="$(mktemp -d)"
cleanup() { rm -rf -- "${tmp}"; }
trap cleanup EXIT
mkdir -p "${tmp}/bin" "${tmp}/registry"

commit=0123456789abcdef0123456789abcdef01234567
short="${commit:0:12}"
amd_id="sha256:$(printf 'a%.0s' {1..64})"
arm_id="sha256:$(printf 'b%.0s' {1..64})"
amd_digest="sha256:$(printf 'd%.0s' {1..64})"
arm_digest="sha256:$(printf 'e%.0s' {1..64})"
runtime_inputs="$(printf '1%.0s' {1..64})"
nft_inputs="$(printf '2%.0s' {1..64})"
init_sha="$(printf '3%.0s' {1..64})"
nft_sha="$(printf '4%.0s' {1..64})"

cat >"${tmp}/bin/git" <<'FAKEGIT'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
    rev-parse) printf '%s\n' "${FAKE_GIT_COMMIT}" ;;
    status) ;;
    *) printf 'unexpected fake git invocation: %s\n' "$*" >&2; exit 90 ;;
esac
FAKEGIT

cat >"${tmp}/bin/docker" <<'FAKEDOCKER'
#!/usr/bin/env bash
set -euo pipefail

registry="${FAKE_REGISTRY_DIR}"
log="${FAKE_DOCKER_LOG}"
amd_id="${FAKE_AMD_ID}"
arm_id="${FAKE_ARM_ID}"
amd_digest="${FAKE_AMD_DIGEST}"
arm_digest="${FAKE_ARM_DIGEST}"

arch_for_ref() {
    case "$1" in
        *-amd64) printf 'amd64\n' ;;
        *-arm64) printf 'arm64\n' ;;
        *) printf 'none\n' ;;
    esac
}

if [[ "$1 $2" == "image inspect" ]]; then
    ref="$3"
    format="${5:-}"
    arch="$(arch_for_ref "${ref}")"
    [[ "${arch}" != none ]] || exit 1
    if [[ -f "${registry}/pulled-${arch}" && "${FAKE_REMOTE_MISMATCH_ARCH:-}" == "${arch}" && "${format}" == '{{.Id}}' ]]; then
        printf 'sha256:%064d\n' 0
        exit 0
    fi
    case "${format}" in
        '{{.Id}}') [[ "${arch}" == amd64 ]] && printf '%s\n' "${amd_id}" || printf '%s\n' "${arm_id}" ;;
        '{{.Os}}/{{.Architecture}}') printf 'linux/%s\n' "${arch}" ;;
        *runtime.version*) printf '%s\n' "${FAKE_RUNTIME_VERSION}" ;;
        *runtime.inputs-sha256*) printf '%s\n' "${FAKE_RUNTIME_INPUTS}" ;;
        *runtime.vaka-init-sha256*) printf '%s\n' "${FAKE_INIT_SHA}" ;;
        *nftables.version*) printf '%s\n' "${FAKE_NFT_VERSION}" ;;
        *nftables.inputs-sha256*) printf '%s\n' "${FAKE_NFT_INPUTS}" ;;
        *nftables.binary-sha256*) printf '%s\n' "${FAKE_NFT_SHA}" ;;
        *) printf 'unexpected image inspect format: %s\n' "${format}" >&2; exit 91 ;;
    esac
    exit 0
fi

if [[ "$1" == pull ]]; then
    ref="${4}"
    arch="$(arch_for_ref "${ref}")"
    [[ -f "${registry}/arch-${arch}" ]] || exit 1
    touch "${registry}/pulled-${arch}"
    exit 0
fi

if [[ "$1" == tag ]]; then
    arch="$(arch_for_ref "$3")"
    rm -f -- "${registry}/pulled-${arch}"
    exit 0
fi

if [[ "$1" == push ]]; then
    ref="$2"
    arch="$(arch_for_ref "${ref}")"
    printf 'push %s\n' "${ref}" >>"${log}"
    touch "${registry}/arch-${arch}"
    exit 0
fi

if [[ "$1 $2 $3" == "buildx imagetools inspect" ]]; then
    shift 3
    format=""
    if [[ "${1:-}" == --format ]]; then
        format="$2"
        shift 2
    fi
    ref="$1"
    arch="$(arch_for_ref "${ref}")"
    if [[ "${arch}" != none ]]; then
        [[ -f "${registry}/arch-${arch}" ]] || { echo 'manifest unknown' >&2; exit 1; }
        case "${format}" in
            '') printf 'Name: %s\n' "${ref}" ;;
            '{{.Manifest.Digest}}') [[ "${arch}" == amd64 ]] && printf '%s\n' "${amd_digest}" || printf '%s\n' "${arm_digest}" ;;
            '{{.Manifest.MediaType}}')
                if [[ "${FAKE_INDEX_ARCH:-}" == "${arch}" ]]; then
                    printf 'application/vnd.oci.image.index.v1+json\n'
                else
                    printf 'application/vnd.oci.image.manifest.v1+json\n'
                fi
                ;;
            *) printf 'unexpected arch manifest format: %s\n' "${format}" >&2; exit 92 ;;
        esac
        exit 0
    fi
    if [[ "${ref}" == *:latest ]]; then
        [[ -f "${registry}/latest" ]] || { echo 'manifest unknown' >&2; exit 1; }
        exit 0
    fi
    [[ -f "${registry}/version" ]] || { echo 'manifest unknown' >&2; exit 1; }
    if [[ -z "${format}" ]]; then
        printf 'Name: %s\n' "${ref}"
    elif [[ "${FAKE_BAD_MANIFEST:-}" == 1 ]]; then
        printf 'linux/amd64 %s\nlinux/arm64 sha256:%064d\n' "${amd_digest}" 9
    elif [[ "${FAKE_EXTRA_MANIFEST:-}" == 1 ]]; then
        printf 'linux/amd64 %s\nlinux/arm64 %s\nunknown/unknown sha256:%064d\n' "${amd_digest}" "${arm_digest}" 8
    else
        printf 'linux/amd64 %s\nlinux/arm64 %s\n' "${amd_digest}" "${arm_digest}"
    fi
    exit 0
fi

if [[ "$1 $2 $3" == "buildx imagetools create" ]]; then
    tag="$5"
    printf 'create %s\n' "${tag}" >>"${log}"
    if [[ "${tag}" == *:latest ]]; then
        touch "${registry}/latest"
    else
        touch "${registry}/version"
    fi
    exit 0
fi

printf 'unexpected fake docker invocation: %s\n' "$*" >&2
exit 99
FAKEDOCKER
chmod +x "${tmp}/bin/git" "${tmp}/bin/docker"

state="${tmp}/prepared-state"
printf '{"schemaVersion":1}\n' >"${tmp}/component-manifest.json"
component_sha="$(sha256sum "${tmp}/component-manifest.json" | awk '{print $1}')"
cat >"${state}" <<STATE
FORMAT=1
GIT_COMMIT=${commit}
GIT_SHORT=${short}
GIT_DIRTY=false
CHANNEL=stable
CLI_VERSION=v0.2.0
RUNTIME_BASE_VERSION=v0.1.0
RUNTIME_EFFECTIVE_VERSION=v0.1.0
RUNTIME_TAG=runtime-v0.1.0
RUNTIME_INPUTS_SHA256=${runtime_inputs}
NFTABLES_VERSION=1.1.6
NFT_INPUTS_SHA256=${nft_inputs}
INIT_IMAGE=example/vaka-init
ARCHS=amd64,arm64
RUNTIME_IMAGE_AMD64=${amd_id}
RUNTIME_IMAGE_ARM64=${arm_id}
VAKA_INIT_BINARY_AMD64_SHA256=${init_sha}
VAKA_INIT_BINARY_ARM64_SHA256=${init_sha}
NFT_BINARY_AMD64_SHA256=${nft_sha}
NFT_BINARY_ARM64_SHA256=${nft_sha}
COMPONENT_MANIFEST_SHA256=${component_sha}
STATE

export PATH="${tmp}/bin:${PATH}"
export FAKE_GIT_COMMIT="${commit}"
export FAKE_REGISTRY_DIR="${tmp}/registry"
export FAKE_DOCKER_LOG="${tmp}/docker.log"
export FAKE_AMD_ID="${amd_id}"
export FAKE_ARM_ID="${arm_id}"
export FAKE_AMD_DIGEST="${amd_digest}"
export FAKE_ARM_DIGEST="${arm_digest}"
export FAKE_RUNTIME_VERSION=v0.1.0
export FAKE_RUNTIME_INPUTS="${runtime_inputs}"
export FAKE_NFT_VERSION=1.1.6
export FAKE_NFT_INPUTS="${nft_inputs}"
export FAKE_INIT_SHA="${init_sha}"
export FAKE_NFT_SHA="${nft_sha}"

preflight_out="${tmp}/preflight.out"
"${REPO_ROOT}/scripts/release-runtime.sh" preflight "${state}" >"${preflight_out}"
assert_contains "${preflight_out}" "registry was not modified"
[[ ! -e "${tmp}/docker.log" ]] || fail "preflight mutated the registry"

publish_out="${tmp}/publish.out"
"${REPO_ROOT}/scripts/release-runtime.sh" publish "${state}" >"${publish_out}"
assert_contains "${tmp}/docker.log" "push example/vaka-init:runtime-v0.1.0-amd64"
assert_contains "${tmp}/docker.log" "push example/vaka-init:runtime-v0.1.0-arm64"
assert_contains "${tmp}/docker.log" "create example/vaka-init:runtime-v0.1.0"
assert_contains "${tmp}/docker.log" "create example/vaka-init:latest"

: >"${tmp}/docker.log"
"${REPO_ROOT}/scripts/release-runtime.sh" publish "${state}" >/dev/null
if grep -Eq '^push |runtime-v0\.1\.0$' "${tmp}/docker.log"; then
    fail "identical publication retry rewrote an immutable tag"
fi
assert_contains "${tmp}/docker.log" "create example/vaka-init:latest"

rm -f -- "${tmp}/registry/version" "${tmp}/registry/latest"
touch "${tmp}/registry/arch-amd64" "${tmp}/registry/arch-arm64"
: >"${tmp}/docker.log"
if FAKE_REMOTE_MISMATCH_ARCH=arm64 "${REPO_ROOT}/scripts/release-runtime.sh" publish "${state}" >"${tmp}/mismatch.out" 2>&1; then
    fail "conflicting immutable architecture image was accepted"
fi
assert_contains "${tmp}/mismatch.out" "refusing to replace immutable runtime tag"
[[ ! -s "${tmp}/docker.log" ]] || fail "conflict path mutated the registry"

touch "${tmp}/registry/version"
if FAKE_BAD_MANIFEST=1 "${REPO_ROOT}/scripts/release-runtime.sh" preflight "${state}" >"${tmp}/manifest.out" 2>&1; then
    fail "mismatched immutable manifest was accepted"
fi
assert_contains "${tmp}/manifest.out" "does not exactly match its architecture tags"

if FAKE_EXTRA_MANIFEST=1 "${REPO_ROOT}/scripts/release-runtime.sh" preflight "${state}" >"${tmp}/extra.out" 2>&1; then
    fail "immutable manifest with an extra child was accepted"
fi
assert_contains "${tmp}/extra.out" "does not exactly match its architecture tags"

rm -f -- "${tmp}/registry/version"
if FAKE_INDEX_ARCH=arm64 "${REPO_ROOT}/scripts/release-runtime.sh" preflight "${state}" >"${tmp}/index.out" 2>&1; then
    fail "multi-platform architecture staging tag was accepted"
fi
assert_contains "${tmp}/index.out" "is not a single-image manifest"

echo "PASS: runtime release preflight and publication"
