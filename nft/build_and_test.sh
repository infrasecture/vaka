#!/usr/bin/env bash

set -euo pipefail

NFT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${NFT_DIR}/.." && pwd)"
source "${REPO_ROOT}/scripts/lib/release-versioning.sh"

NFTABLES_VERSION="$(awk -F= '/^ARG NFTABLES_VERSION=/{print $2; exit}' "${NFT_DIR}/Dockerfile")"
: "${NFTABLES_VERSION:?failed to detect NFTABLES_VERSION from nft/Dockerfile}"

case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "Unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
esac

fingerprint="$(vaka_nft_inputs_sha256 "${REPO_ROOT}" "${NFTABLES_VERSION}")"
image="${NFT_INTERNAL_IMAGE:-vaka-internal/nft-static}:${fingerprint}-${arch}"

echo "Building internal test artifact ${image} ..."
docker buildx build \
    --platform "linux/${arch}" \
    --load \
    --target artifacts \
    --tag "${image}" \
    "${NFT_DIR}"

work_tmp_dir="$(mktemp -d "${NFT_DIR}/.tmp.nft-static.XXXXXX")"
tmp_bin="${work_tmp_dir}/nft"
container_id=""

cleanup() {
    if [[ -n "${container_id:-}" ]]; then
        docker rm -f -- "${container_id}" >/dev/null 2>&1 || true
    fi
    case "${work_tmp_dir}" in
        "${NFT_DIR}"/.tmp.nft-static.*) rm -rf -- "${work_tmp_dir:?}" ;;
        *) echo "Refusing cleanup: ${work_tmp_dir}" >&2 ;;
    esac
}
trap cleanup EXIT

container_id="$(docker create --entrypoint /opt/nftables/bin/nft "${image}" --version)"
docker cp "${container_id}:/opt/nftables/bin/nft" "${tmp_bin}"

if ! file "${tmp_bin}" | grep -qi "statically linked"; then
    echo "Built nft binary is not statically linked" >&2
    file "${tmp_bin}" >&2
    exit 1
fi

version_output="$(docker run --rm --entrypoint /opt/nftables/bin/nft "${image}" --version)"
if [[ "${version_output}" != *"nftables v${NFTABLES_VERSION}"* ]]; then
    echo "Unexpected nft version output: ${version_output}" >&2
    exit 1
fi

echo "PASS: ${version_output} (${image})"
echo "This local image is an internal build artifact and must not be published."
