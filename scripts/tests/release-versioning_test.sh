#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${REPO_ROOT}/scripts/lib/release-versioning.sh"

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

assert_equal() {
    local want="$1"
    local got="$2"
    local label="$3"
    [[ "${got}" == "${want}" ]] || fail "${label}: got ${got@Q}, want ${want@Q}"
}

for valid in v0.1.0 v1.0.0 v12.34.56; do
    vaka_is_stable_version "${valid}" || fail "stable version rejected: ${valid}"
done
for invalid in 0.1.0 v01.0.0 v0.01.0 v0.1.00 v0.1 v0.1.0-rc.1 nightly.abc; do
    if vaka_is_stable_version "${invalid}"; then
        fail "invalid stable version accepted: ${invalid}"
    fi
done

for valid in v0.1.0 0123456789ab; do
    vaka_is_release_cli_version "${valid}" || fail "release CLI version rejected: ${valid}"
done
for invalid in 0123456789a 0123456789ABC v0.1.0-rc.1; do
    if vaka_is_release_cli_version "${invalid}"; then
        fail "invalid release CLI version accepted: ${invalid}"
    fi
done

nightly="$(vaka_nightly_runtime_version v0.1.0 0123456789ab)"
assert_equal "v0.1.0-nightly.0123456789ab" "${nightly}" "nightly runtime version"
vaka_is_effective_runtime_version "${nightly}" || fail "derived nightly version rejected"
if vaka_is_effective_runtime_version "v0.1.0-nightly.not-hex"; then
    fail "invalid nightly runtime version accepted"
fi

tmp="$(mktemp -d)"
cleanup() { rm -rf -- "${tmp}"; }
trap cleanup EXIT

printf 'alpha' > "${tmp}/a"
printf 'beta' > "${tmp}/b"
hash_ab="$(vaka_hash_paths "${tmp}" a b)"
hash_ab_again="$(vaka_hash_paths "${tmp}" a b)"
hash_ba="$(vaka_hash_paths "${tmp}" b a)"
assert_equal "${hash_ab}" "${hash_ab_again}" "stable path fingerprint"
[[ "${hash_ab}" != "${hash_ba}" ]] || fail "path order must affect fingerprint"
printf 'changed' > "${tmp}/b"
hash_changed="$(vaka_hash_paths "${tmp}" a b)"
[[ "${hash_ab}" != "${hash_changed}" ]] || fail "content change must affect fingerprint"

mkdir -p "${tmp}/nft"
printf 'FROM scratch\n' >"${tmp}/nft/Dockerfile"
nft_hash="$(vaka_nft_inputs_sha256 "${tmp}" 1.2.3)"
assert_equal "${nft_hash}" "$(vaka_nft_inputs_sha256 "${tmp}" 1.2.3)" "stable nft fingerprint"
[[ "${nft_hash}" != "$(vaka_nft_inputs_sha256 "${tmp}" 1.2.4)" ]] || fail "nft version must affect fingerprint"

state="${tmp}/state"
cat > "${state}" <<'STATE'
FORMAT=1
CLI_VERSION=v0.2.0
UNTRUSTED=$(touch /tmp/vaka-state-parser-must-not-execute)
STATE
rm -f /tmp/vaka-state-parser-must-not-execute
assert_equal "v0.2.0" "$(vaka_state_require "${state}" CLI_VERSION)" "state value"
assert_equal '$(touch /tmp/vaka-state-parser-must-not-execute)' \
    "$(vaka_state_require "${state}" UNTRUSTED)" "literal state parsing"
[[ ! -e /tmp/vaka-state-parser-must-not-execute ]] || fail "state parser executed file content"
if vaka_state_require "${state}" MISSING >/dev/null 2>&1; then
    fail "missing required state value accepted"
fi

printf 'CLI_VERSION=duplicate\n' >> "${state}"
if vaka_state_require "${state}" CLI_VERSION >/dev/null 2>&1; then
    fail "duplicate prepared-state key accepted"
fi

echo "PASS: release versioning helpers"
