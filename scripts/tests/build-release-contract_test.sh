#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
cd "${REPO_ROOT}"

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

assert_file_contains() {
    local path="$1"
    local text="$2"
    grep -Fq -- "${text}" "${path}" || fail "${path} does not contain: ${text}"
}

assert_fails_with() {
    local expected="$1"
    shift
    local output rc
    set +e
    output="$({ "$@"; } 2>&1)"
    rc=$?
    set -e
    [[ ${rc} -ne 0 ]] || fail "command unexpectedly succeeded: $*"
    grep -Fq -- "${expected}" <<<"${output}" || fail "failure did not contain ${expected}: ${output}"
}

bash -n build.sh release.sh scripts/release-runtime.sh scripts/smoke-image-mount.sh

build_help="$(./build.sh --help)"
grep -Fq -- '--cli-version VERSION' <<<"${build_help}" || fail "build help omits explicit CLI version"
grep -Fq -- '--preflight-runtime' <<<"${build_help}" || fail "build help omits runtime preflight"
grep -Fq -- '--publish-runtime' <<<"${build_help}" || fail "build help omits runtime publication"
if grep -Fq -- '--publish-prepared' <<<"${build_help}"; then
    fail "build help collides with release.sh --publish-prepared"
fi
assert_fails_with 'was removed' ./build.sh --push
assert_fails_with 'CLI release version must be' ./build.sh --cli-version v1.2.3-rc.1
assert_fails_with 'runtime registry actions cannot be combined with build options' ./build.sh --preflight-runtime --release
assert_fails_with 'unknown argument' ./build.sh --publish-prepared

release_help="$(./release.sh --help)"
grep -Fq -- '--prepare-only' <<<"${release_help}" || fail "release help omits prepare-only"
grep -Fq -- '--publish-prepared' <<<"${release_help}" || fail "release help omits publish-prepared"
assert_fails_with 'stable releases require --version' ./release.sh --prepare-only
assert_fails_with 'mutually exclusive' ./release.sh --nightly --version v1.2.3

if grep -Eq '^[[:space:]]*docker push' build.sh; then
    fail "build.sh must not publish images"
fi
if grep -Fq 'dst: /opt/vaka' build.sh; then
    fail "host package template still installs runtime internals"
fi
if grep -Fq 'bin.install "vaka-init"' release.sh; then
    fail "Homebrew formula still installs vaka-init"
fi
if grep -Eq 'tar .*vaka-init' release.sh; then
    fail "Homebrew archive still bundles vaka-init"
fi

assert_file_contains build.sh 'vaka-internal/nft-static'
assert_file_contains build.sh 'golang:1.25.12-alpine@sha256:'
assert_file_contains build.sh 'ghcr.io/goreleaser/nfpm:v2.47.0@sha256:'
assert_file_contains nft/Dockerfile 'FROM alpine:3.21@sha256:'
assert_file_contains release.sh 'GOVULNCHECK_VERSION:-v1.6.0'
assert_file_contains release.sh 'ARTIFACT_LIST_SHA256'
assert_file_contains scripts/smoke-image-mount.sh 'RUNTIME_EFFECTIVE_VERSION'

echo "PASS: build and release command contracts"
