#!/usr/bin/env bash

# Validate or publish a runtime image assembled by build.sh. This script never
# builds artifacts and never publishes the internal nft build image.

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${SCRIPT_DIR}/lib/release-versioning.sh"

usage() {
    cat <<'EOF'
Usage: scripts/release-runtime.sh <preflight|publish> [STATE_FILE]

STATE_FILE defaults to dist/.vaka-release-state. Both modes require the exact
prepared Git commit, a clean working tree, and the prepared local runtime
images. preflight performs no registry mutation. publish repeats preflight,
pushes only missing immutable architecture tags, creates the immutable version
manifest when absent, and updates :latest only for stable releases.
EOF
}

die() {
    printf 'ERROR: %s\n' "$*" >&2
    exit 1
}

[[ $# -ge 1 && $# -le 2 ]] || { usage >&2; exit 1; }
mode="$1"
state_file="${2:-${REPO_ROOT}/dist/.vaka-release-state}"
case "${mode}" in
    preflight|publish) ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown runtime release mode: ${mode}" ;;
esac

[[ -f "${state_file}" ]] || die "prepared state not found: ${state_file}"
state_file="$(cd "$(dirname "${state_file}")" && pwd)/$(basename "${state_file}")"

state() {
    vaka_state_require "${state_file}" "$1"
}

format="$(state FORMAT)"
git_commit="$(state GIT_COMMIT)"
git_short="$(state GIT_SHORT)"
git_dirty="$(state GIT_DIRTY)"
channel="$(state CHANNEL)"
cli_version="$(state CLI_VERSION)"
runtime_base="$(state RUNTIME_BASE_VERSION)"
runtime_version="$(state RUNTIME_EFFECTIVE_VERSION)"
runtime_tag="$(state RUNTIME_TAG)"
runtime_inputs="$(state RUNTIME_INPUTS_SHA256)"
nft_version="$(state NFTABLES_VERSION)"
nft_inputs="$(state NFT_INPUTS_SHA256)"
init_image="$(state INIT_IMAGE)"
archs_csv="$(state ARCHS)"
component_manifest_sha="$(state COMPONENT_MANIFEST_SHA256)"

[[ "${format}" == "1" ]] || die "unsupported prepared-state format: ${format}"
[[ "${git_commit}" =~ ^[0-9a-f]{40,64}$ ]] || die "invalid GIT_COMMIT in prepared state"
[[ "${git_short}" =~ ^[0-9a-f]{12}$ ]] || die "invalid GIT_SHORT in prepared state"
[[ "${git_commit}" == "${git_short}"* ]] || die "GIT_SHORT does not identify GIT_COMMIT"
[[ "${git_dirty}" == "false" ]] || die "prepared release was built from a dirty working tree"
[[ "${runtime_inputs}" =~ ^[0-9a-f]{64}$ ]] || die "invalid runtime input fingerprint"
[[ "${nft_inputs}" =~ ^[0-9a-f]{64}$ ]] || die "invalid nft input fingerprint"
[[ "${component_manifest_sha}" =~ ^[0-9a-f]{64}$ ]] || die "invalid component manifest fingerprint"
[[ "${nft_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || die "invalid nftables version"
[[ "${init_image}" =~ ^[a-z0-9]+([._-][a-z0-9]+)*([:/][a-z0-9]+([._-][a-z0-9]+)*)*$ ]] || \
    die "invalid runtime image repository: ${init_image}"

case "${channel}" in
    stable)
        vaka_is_stable_version "${cli_version}" || die "stable release has invalid CLI version: ${cli_version}"
        vaka_is_stable_version "${runtime_base}" || die "stable release has invalid runtime base: ${runtime_base}"
        [[ "${runtime_version}" == "${runtime_base}" ]] || \
            die "stable release runtime must equal its committed base version"
        ;;
    nightly)
        vaka_is_nightly_cli_version "${cli_version}" || die "nightly release has invalid CLI version: ${cli_version}"
        [[ "${cli_version}" == "${git_short}" ]] || die "nightly CLI version must equal GIT_SHORT"
        expected_nightly="$(vaka_nightly_runtime_version "${runtime_base}" "${git_short}")"
        [[ "${runtime_version}" == "${expected_nightly}" ]] || \
            die "nightly runtime version must be ${expected_nightly}"
        ;;
    *) die "invalid release channel: ${channel}" ;;
esac
vaka_is_effective_runtime_version "${runtime_version}" || die "invalid effective runtime version: ${runtime_version}"
[[ "${runtime_tag}" == "runtime-${runtime_version}" ]] || die "runtime tag does not match effective runtime version"

IFS=',' read -r -a archs <<<"${archs_csv}"
[[ "${#archs[@]}" -eq 2 && "${archs[0]}" == "amd64" && "${archs[1]}" == "arm64" ]] || \
    die "prepared publication requires ARCHS=amd64,arm64 (got ${archs_csv})"

cd "${REPO_ROOT}"
component_manifest="$(dirname "${state_file}")/component-manifest.json"
[[ -f "${component_manifest}" ]] || die "component manifest is missing next to prepared state"
[[ "$(vaka_sha256_file "${component_manifest}")" == "${component_manifest_sha}" ]] || \
    die "component manifest changed after release preparation"
current_commit="$(git rev-parse --verify HEAD)"
[[ "${current_commit}" == "${git_commit}" ]] || \
    die "prepared state belongs to ${git_commit}, but HEAD is ${current_commit}"
[[ -z "$(git status --porcelain)" ]] || die "working tree changed after release preparation"

declare -A local_ids=()
for arch in "${archs[@]}"; do
    state_key="RUNTIME_IMAGE_${arch^^}"
    state_key="${state_key//-/_}"
    local_id="$(state "${state_key}")"
    [[ "${local_id}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "invalid ${state_key} in prepared state"
    ref="${init_image}:${runtime_tag}-${arch}"
    inspected_id="$(docker image inspect "${ref}" --format '{{.Id}}' 2>/dev/null)" || \
        die "prepared local image is missing: ${ref}"
    [[ "${inspected_id}" == "${local_id}" ]] || \
        die "prepared local image ${ref} changed (${inspected_id}, expected ${local_id})"

    label_version="$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.runtime.version"}}')"
    label_runtime_inputs="$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.runtime.inputs-sha256"}}')"
    label_nft_version="$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.nftables.version"}}')"
    label_nft_inputs="$(docker image inspect "${ref}" --format '{{index .Config.Labels "agent.vaka.nftables.inputs-sha256"}}')"
    [[ "${label_version}" == "${runtime_version}" ]] || die "${ref} has runtime version label ${label_version}"
    [[ "${label_runtime_inputs}" == "${runtime_inputs}" ]] || die "${ref} has the wrong runtime input fingerprint"
    [[ "${label_nft_version}" == "${nft_version}" ]] || die "${ref} has nftables version label ${label_nft_version}"
    [[ "${label_nft_inputs}" == "${nft_inputs}" ]] || die "${ref} has the wrong nft input fingerprint"
    local_ids["${arch}"]="${local_id}"
done

registry_inspect() {
    local ref="$1"
    local output
    if output="$(docker buildx imagetools inspect "${ref}" 2>&1)"; then
        return 0
    fi
    if grep -Eqi 'not found|manifest unknown|name unknown' <<<"${output}"; then
        return 1
    fi
    printf 'ERROR: cannot inspect registry reference %s:\n%s\n' "${ref}" "${output}" >&2
    exit 1
}

remote_manifest_digest() {
    local ref="$1"
    local digest
    digest="$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "${ref}")" || \
        die "cannot read registry digest for ${ref}"
    [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "registry returned an invalid digest for ${ref}: ${digest}"
    printf '%s\n' "${digest}"
}

verify_remote_arch_image() {
    local arch="$1"
    local ref="${init_image}:${runtime_tag}-${arch}"
    local local_id="${local_ids[${arch}]}"
    local remote_id

    if ! docker pull --platform "linux/${arch}" "${ref}" >/dev/null; then
        docker tag "${local_id}" "${ref}" >/dev/null 2>&1 || true
        die "cannot pull existing immutable runtime tag ${ref}"
    fi
    if ! remote_id="$(docker image inspect "${ref}" --format '{{.Id}}')"; then
        docker tag "${local_id}" "${ref}" >/dev/null 2>&1 || true
        die "cannot inspect pulled immutable runtime tag ${ref}"
    fi
    docker tag "${local_id}" "${ref}" >/dev/null
    [[ "${remote_id}" == "${local_id}" ]] || {
        printf 'ERROR: refusing to replace immutable runtime tag %s\n' "${ref}" >&2
        printf '       registry: %s\n       prepared: %s\n' "${remote_id}" "${local_id}" >&2
        printf '       Bump internal/runtimebundle/VERSION for stable releases.\n' >&2
        exit 1
    }
}

manifest_children() {
    local ref="$1"
    docker buildx imagetools inspect --format \
        '{{range .Manifest.Manifests}}{{if eq .Platform.OS "linux"}}{{.Platform.OS}}/{{.Platform.Architecture}} {{.Digest}}{{println}}{{end}}{{end}}' \
        "${ref}"
}

preflight_registry() {
    local version_ref="${init_image}:${runtime_tag}"
    local arch ref digest
    local -a missing=()
    local expected actual

    expected="$(mktemp)"
    actual="$(mktemp)"
    trap 'rm -f -- "${expected}" "${actual}"' RETURN

    for arch in "${archs[@]}"; do
        ref="${version_ref}-${arch}"
        if registry_inspect "${ref}"; then
            verify_remote_arch_image "${arch}"
            digest="$(remote_manifest_digest "${ref}")"
            printf 'linux/%s %s\n' "${arch}" "${digest}" >>"${expected}"
        else
            missing+=("${arch}")
        fi
    done

    if registry_inspect "${version_ref}"; then
        [[ "${#missing[@]}" -eq 0 ]] || \
            die "immutable manifest ${version_ref} exists but architecture tags are missing: ${missing[*]}"
        if ! manifest_children "${version_ref}" >"${actual}"; then
            die "cannot inspect children of immutable manifest ${version_ref}"
        fi
        LC_ALL=C sort -o "${expected}" "${expected}"
        LC_ALL=C sort -o "${actual}" "${actual}"
        if ! diff -u "${expected}" "${actual}"; then
            die "immutable manifest ${version_ref} does not exactly match its architecture tags"
        fi
    fi

    if [[ "${#missing[@]}" -eq 0 ]]; then
        echo "    Registry preflight: immutable runtime content matches."
    else
        echo "    Registry preflight: missing immutable architecture tags: ${missing[*]}"
    fi
}

echo "==> Runtime registry preflight (${init_image}:${runtime_tag})"
preflight_registry

if [[ "${mode}" == "preflight" ]]; then
    echo "Runtime publication preflight passed; registry was not modified."
    exit 0
fi

version_ref="${init_image}:${runtime_tag}"
for arch in "${archs[@]}"; do
    ref="${version_ref}-${arch}"
    if registry_inspect "${ref}"; then
        echo "    ${ref} already exists with identical content"
    else
        echo "==> Pushing immutable runtime tag ${ref}"
        docker push "${ref}"
    fi
done

# Recheck all immutable architecture tags after the only recoverable partial
# mutation point. A failed push can be retried without replacing existing data.
preflight_registry

if registry_inspect "${version_ref}"; then
    echo "    ${version_ref} already exists with the expected children"
else
    sources=()
    for arch in "${archs[@]}"; do
        sources+=("${version_ref}-${arch}")
    done
    echo "==> Creating immutable runtime manifest ${version_ref}"
    docker buildx imagetools create --tag "${version_ref}" "${sources[@]}"
fi

# Verify the final immutable graph before touching the mutable convenience tag.
preflight_registry

if [[ "${channel}" == "stable" ]]; then
    echo "==> Updating stable runtime convenience tag ${init_image}:latest"
    docker buildx imagetools create --tag "${init_image}:latest" "${version_ref}"
else
    echo "    Nightly release: ${init_image}:latest remains unchanged."
fi

echo "Runtime publication complete: ${version_ref}"
