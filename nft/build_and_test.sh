#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
: "${ROOT_DIR:?failed to resolve ROOT_DIR}"
cd "$ROOT_DIR"

NFTABLES_VERSION="${NFTABLES_VERSION:-$(awk -F= '/^ARG NFTABLES_VERSION=/{print $2; exit}' Dockerfile)}"
IMAGE_NAME="${IMAGE_NAME:-emsi/nft-static}"
RELEASE_TAG="${IMAGE_NAME}:${NFTABLES_VERSION}"
LATEST_TAG="${IMAGE_NAME}:latest"

if [[ -z "${NFTABLES_VERSION}" ]]; then
  echo "Failed to detect NFTABLES_VERSION from Dockerfile" >&2
  exit 1
fi

echo "Building ${RELEASE_TAG} and ${LATEST_TAG} ..."
docker build --target artifacts -t "${RELEASE_TAG}" -t "${LATEST_TAG}" .

work_tmp_dir="$(mktemp -d "${ROOT_DIR}/.tmp.nft-static.XXXXXX")"
tmp_bin="${work_tmp_dir}/nft"
container_id=""

cleanup() {
  if [[ -n "${container_id:-}" ]]; then
    docker rm -f -- "${container_id}" >/dev/null 2>&1 || true
  fi
  case "$work_tmp_dir" in
    "$ROOT_DIR"/.tmp.nft-static.*) rm -rf -- "${work_tmp_dir:?}" ;;
    *) echo "Refusing cleanup: $work_tmp_dir" >&2 ;;
  esac
}
trap cleanup EXIT

container_id="$(docker create --entrypoint /opt/nftables/bin/nft "${RELEASE_TAG}" --version)"
docker cp "${container_id}:/opt/nftables/bin/nft" "${tmp_bin}"

if ! file "${tmp_bin}" | grep -qi "statically linked"; then
  echo "Built nft binary is not statically linked" >&2
  file "${tmp_bin}" >&2
  exit 1
fi

version_output="$(docker run --rm --entrypoint /opt/nftables/bin/nft "${RELEASE_TAG}" --version)"
if [[ "${version_output}" != *"nftables v${NFTABLES_VERSION}"* ]]; then
  echo "Unexpected nft version output: ${version_output}" >&2
  exit 1
fi

echo "OK: ${version_output}"
echo "Ready to publish:"
echo "  docker push ${RELEASE_TAG}"
echo "  docker push ${LATEST_TAG}"
