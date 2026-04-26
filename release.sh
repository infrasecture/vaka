#!/usr/bin/env bash
# release.sh — build and publish a vaka GitHub release from local machine.
#
# Default mode (stable release):
#   - Requires a release tag (vX.Y.Z) pointing at HEAD.
#   - Fails if no release tag is found.
#
# Nightly mode:
#   - Uses short commit SHA (12 chars) as the release tag.
#   - Marks the release as a pre-release.
#
# Build behavior:
#   - Calls build.sh exactly once:
#       ./build.sh --release --packages
#   - Generates SHA256SUMS for this release's artifacts only.
#
# Requirements:
#   - git
#   - docker
#   - gh (authenticated to target repo)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

usage() {
    cat <<'EOF'
Usage:
  ./release.sh [--nightly] [--title TITLE] [--notes-file PATH]

Options:
  --nightly          Create a nightly pre-release tagged with short git SHA.
  --title TITLE      Override GitHub release title (default: tag).
  --notes-file PATH  Use explicit release notes file (otherwise --generate-notes).
  -h, --help         Show this help.

Behavior:
  - Default mode requires a release tag (vX.Y.Z) on HEAD; otherwise exits non-zero.
  - Build is executed once via: ./build.sh --release --packages
EOF
}

nightly=false
release_title=""
notes_file=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --nightly)
            nightly=true
            shift
            ;;
        --title)
            [[ $# -ge 2 ]] || { echo "ERROR: --title requires a value" >&2; exit 1; }
            release_title="$2"
            shift 2
            ;;
        --notes-file)
            [[ $# -ge 2 ]] || { echo "ERROR: --notes-file requires a path" >&2; exit 1; }
            notes_file="$2"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "ERROR: unknown argument: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

require_cmd() {
    local c="$1"
    command -v "${c}" >/dev/null 2>&1 || {
        echo "ERROR: required command not found: ${c}" >&2
        exit 1
    }
}

require_cmd git
require_cmd docker
require_cmd gh

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    echo "ERROR: not inside a git repository" >&2
    exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
    echo "ERROR: working tree is not clean; commit/stash changes before release" >&2
    exit 1
fi

origin_url="$(git config --get remote.origin.url || true)"
repo_slug=""
if [[ -n "${origin_url}" ]]; then
    repo_slug="$(printf '%s' "${origin_url}" | sed -E \
        -e 's#^git@github\.com:##' \
        -e 's#^https://github\.com/##' \
        -e 's#\.git$##')"
fi

if [[ -n "${repo_slug}" && "${repo_slug}" != "${origin_url}" ]]; then
    if ! gh repo view "${repo_slug}" >/dev/null 2>&1; then
        echo "ERROR: gh cannot access GitHub repository ${repo_slug}." >&2
        echo "       Check active account/token with: gh auth status" >&2
        exit 1
    fi
else
    if ! gh auth token >/dev/null 2>&1; then
        echo "ERROR: gh has no active auth token; run: gh auth login" >&2
        exit 1
    fi
fi

head_commit="$(git rev-parse --verify HEAD)"
head_short="$(git rev-parse --short=12 HEAD)"

release_tag=""
is_prerelease=false

if [[ "${nightly}" == "true" ]]; then
    release_tag="${head_short}"
    is_prerelease=true
else
    mapfile -t release_tags < <(git tag --points-at HEAD | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' || true)

    if [[ "${#release_tags[@]}" -eq 0 ]]; then
        echo "ERROR: no release tag (vX.Y.Z) points at HEAD." >&2
        echo "       Tag the commit first, or run with --nightly." >&2
        exit 1
    fi
    if [[ "${#release_tags[@]}" -gt 1 ]]; then
        echo "ERROR: multiple release tags point at HEAD: ${release_tags[*]}" >&2
        echo "       Keep a single release tag on HEAD before running release.sh." >&2
        exit 1
    fi
    release_tag="${release_tags[0]}"
fi

if gh release view "${release_tag}" >/dev/null 2>&1; then
    echo "ERROR: GitHub release already exists for tag ${release_tag}" >&2
    exit 1
fi

if [[ -z "${release_title}" ]]; then
    if [[ "${nightly}" == "true" ]]; then
        release_title="vaka-${head_short}"
    else
        release_title="${release_tag}"
    fi
fi

echo "==> Building release artifacts with VERSION=${release_tag} (no registry publish)"
VERSION="${release_tag}" ./build.sh --release --packages

artifacts=()
artifact_names=()
pkg_version="${release_tag#v}"

# GitHub release payload policy:
#   - include package artifacts only (.deb/.rpm/.pkg.tar.*)
#   - include macOS vaka binaries only
#   - exclude nft/vaka-init artifacts and Linux raw vaka binaries
required_macos=(
    "dist/vaka-darwin-amd64"
    "dist/vaka-darwin-arm64"
)
for path in "${required_macos[@]}"; do
    if [[ ! -f "${path}" ]]; then
        echo "ERROR: missing required macOS binary: ${path}" >&2
        exit 1
    fi
    artifacts+=("${path}")
    artifact_names+=("$(basename "${path}")")
done

shopt -s nullglob
deb_pkgs=(dist/vaka_"${pkg_version}"_*.deb)
rpm_pkgs=(dist/vaka-"${pkg_version}"-*.rpm)
arch_pkgs=(
    dist/vaka-"${pkg_version}"-*.pkg.tar.*
    dist/vaka_"${pkg_version}"_*.pkg.tar.*
)
shopt -u nullglob

if [[ "${#deb_pkgs[@]}" -eq 0 ]]; then
    echo "ERROR: no .deb packages found in dist/" >&2
    exit 1
fi
if [[ "${#rpm_pkgs[@]}" -eq 0 ]]; then
    echo "ERROR: no .rpm packages found in dist/" >&2
    exit 1
fi
if [[ "${#arch_pkgs[@]}" -eq 0 ]]; then
    echo "ERROR: no Arch Linux packages (.pkg.tar.*) found in dist/" >&2
    echo "       Ensure build.sh package phase includes archlinux output." >&2
    exit 1
fi

declare -A seen_pkg=()
for path in "${deb_pkgs[@]}" "${rpm_pkgs[@]}" "${arch_pkgs[@]}"; do
    [[ -n "${seen_pkg[${path}]:-}" ]] && continue
    seen_pkg["${path}"]=1
    artifacts+=("${path}")
    artifact_names+=("$(basename "${path}")")
done

if command -v sha256sum >/dev/null 2>&1; then
    (cd dist && sha256sum "${artifact_names[@]}" > SHA256SUMS)
elif command -v shasum >/dev/null 2>&1; then
    (cd dist && shasum -a 256 "${artifact_names[@]}" > SHA256SUMS)
else
    echo "ERROR: neither sha256sum nor shasum is available for checksums" >&2
    exit 1
fi
artifacts+=("dist/SHA256SUMS")

if [[ "${is_prerelease}" == "true" ]]; then
    # Pre-create and push the nightly tag so release creation can use --verify-tag
    # and avoid API-side auto-tag creation edge cases.
    if git rev-parse -q --verify "refs/tags/${release_tag}" >/dev/null 2>&1; then
        tag_target="$(git rev-list -n1 "${release_tag}")"
        if [[ "${tag_target}" != "${head_commit}" ]]; then
            echo "ERROR: local tag ${release_tag} points to ${tag_target}, expected ${head_commit}" >&2
            exit 1
        fi
    else
        git tag "${release_tag}" "${head_commit}"
    fi
    if git ls-remote --exit-code --tags origin "refs/tags/${release_tag}" >/dev/null 2>&1; then
        remote_tag_target="$(git ls-remote --tags origin "refs/tags/${release_tag}^{}" | awk '{print $1}' || true)"
        if [[ -z "${remote_tag_target}" ]]; then
            remote_tag_target="$(git ls-remote --tags origin "refs/tags/${release_tag}" | awk '{print $1}' || true)"
        fi
        if [[ -n "${remote_tag_target}" && "${remote_tag_target}" != "${head_commit}" ]]; then
            echo "ERROR: remote tag ${release_tag} points to ${remote_tag_target}, expected ${head_commit}" >&2
            exit 1
        fi
    else
        echo "==> Pushing nightly tag ${release_tag}"
        git push origin "refs/tags/${release_tag}:refs/tags/${release_tag}"
    fi
fi

echo "==> Creating GitHub release ${release_tag}"
gh_args=(release create "${release_tag}")
gh_args+=("${artifacts[@]}")
gh_args+=(--title "${release_title}")

if [[ -n "${notes_file}" ]]; then
    gh_args+=(--notes-file "${notes_file}")
else
    if [[ "${is_prerelease}" == "true" ]]; then
        gh_args+=(--notes "Nightly build for commit ${head_commit}")
    else
        gh_args+=(--generate-notes)
    fi
fi

if [[ "${is_prerelease}" == "true" ]]; then
    gh_args+=(--prerelease --verify-tag)
else
    gh_args+=(--verify-tag)
fi

gh "${gh_args[@]}"

echo ""
echo "Release complete:"
echo "  tag:   ${release_tag}"
echo "  title: ${release_title}"
echo "  mode:  $( [[ "${is_prerelease}" == "true" ]] && echo nightly || echo tagged-release )"
