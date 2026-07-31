#!/usr/bin/env bash

# Shared, side-effect-free helpers for build.sh and release.sh.

vaka_is_stable_version() {
    [[ "${1:-}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
}

vaka_require_stable_version() {
    local component="$1"
    local value="$2"
    if ! vaka_is_stable_version "${value}"; then
        printf 'ERROR: %s must be canonical v-prefixed SemVer (got %s)\n' \
            "${component}" "${value:-<empty>}" >&2
        return 1
    fi
}

vaka_is_nightly_cli_version() {
    [[ "${1:-}" =~ ^[0-9a-f]{12}$ ]]
}

vaka_is_release_cli_version() {
    vaka_is_stable_version "${1:-}" || vaka_is_nightly_cli_version "${1:-}"
}

vaka_require_release_cli_version() {
    local value="$1"
    if ! vaka_is_release_cli_version "${value}"; then
        printf 'ERROR: CLI release version must be canonical v-prefixed SemVer or a 12-character lowercase Git ID (got %s)\n' \
            "${value:-<empty>}" >&2
        return 1
    fi
}

vaka_is_effective_runtime_version() {
    local value="${1:-}"
    vaka_is_stable_version "${value}" || \
        [[ "${value}" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-nightly\.[0-9a-f]{12,64}$ ]]
}

vaka_nightly_runtime_version() {
    local base="$1"
    local commit="$2"
    vaka_require_stable_version "runtime bundle version" "${base}" || return 1
    if [[ ! "${commit}" =~ ^[0-9a-f]{12,64}$ ]]; then
        printf 'ERROR: nightly runtime commit must be a 12-64 character lowercase hex Git ID (got %s)\n' \
            "${commit:-<empty>}" >&2
        return 1
    fi
    printf '%s-nightly.%s\n' "${base}" "${commit}"
}

vaka_sha256_stream() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
        return
    fi
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 | awk '{print $1}'
        return
    fi
    echo "ERROR: neither sha256sum nor shasum is available" >&2
    return 1
}

vaka_sha256_file() {
    local path="$1"
    vaka_sha256_stream < "${path}"
}

# Hash path names and bytes, not mtimes. Callers must pass paths in their
# desired deterministic order.
vaka_hash_paths() {
    local root="$1"
    shift
    (
        cd "${root}"
        local path
        for path in "$@"; do
            [[ -f "${path}" ]] || {
                printf 'ERROR: fingerprint input missing: %s/%s\n' "${root}" "${path}" >&2
                exit 1
            }
            printf '%s\0' "${path}"
            cat -- "${path}"
            printf '\0'
        done
    ) | vaka_sha256_stream
}

vaka_state_get() {
    local state_file="$1"
    local key="$2"
    if [[ ! "${key}" =~ ^[A-Z][A-Z0-9_]*$ ]]; then
        printf 'ERROR: invalid prepared-state key: %s\n' "${key}" >&2
        return 1
    fi
    awk -v wanted="${key}" '
        index($0, wanted "=") == 1 {
            value = substr($0, length(wanted) + 2)
            found++
        }
        END {
            if (found != 1) exit found > 1 ? 2 : 1
            print value
        }
    ' "${state_file}"
}

vaka_state_require() {
    local state_file="$1"
    local key="$2"
    local value
    if ! value="$(vaka_state_get "${state_file}" "${key}")" || [[ -z "${value}" ]]; then
        printf 'ERROR: prepared state %s has no %s value\n' "${state_file}" "${key}" >&2
        return 1
    fi
    printf '%s\n' "${value}"
}
