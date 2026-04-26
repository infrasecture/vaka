#!/usr/bin/env bash
# release.sh — build and publish a vaka GitHub release from local machine.
#
# Default mode (stable release):
#   - Requires a release tag (vX.Y.Z) pointing at HEAD.
#   - Fails if no release tag is found.
#
# Nightly mode:
#   - Uses the full commit SHA as the release tag.
#   - Marks the release as a pre-release.
#
# Build behavior:
#   - Calls build.sh exactly once:
#       ./build.sh --release --packages --push
#   - Generates SHA256SUMS for all non-hidden dist/ files.
#
# Requirements:
#   - git
#   - docker (authenticated for push destination)
#   - gh (authenticated to target repo)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

usage() {
    cat <<'EOF'
Usage:
  ./release.sh [--nightly] [--title TITLE] [--notes-file PATH]

Options:
  --nightly          Create a nightly pre-release tagged with full git SHA.
  --title TITLE      Override GitHub release title (default: tag).
  --notes-file PATH  Use explicit release notes file (otherwise --generate-notes).
  -h, --help         Show this help.

Behavior:
  - Default mode requires a release tag (vX.Y.Z) on HEAD; otherwise exits non-zero.
  - Build is executed once via: ./build.sh --release --packages --push
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
    release_tag="${head_commit}"
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
    release_title="${release_tag}"
fi

mkdir -p dist
find dist -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +

echo "==> Building and publishing artifacts/images with VERSION=${release_tag}"
VERSION="${release_tag}" ./build.sh --release --packages --push

shopt -s nullglob
artifacts=()
artifact_names=()
for path in dist/*; do
    [[ -f "${path}" ]] || continue
    name="$(basename "${path}")"
    [[ "${name}" == .* ]] && continue
    artifacts+=("${path}")
    artifact_names+=("${name}")
done
shopt -u nullglob

if [[ "${#artifacts[@]}" -eq 0 ]]; then
    echo "ERROR: no release artifacts found in dist/" >&2
    exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
    (cd dist && sha256sum "${artifact_names[@]}" > SHA256SUMS)
elif command -v shasum >/dev/null 2>&1; then
    (cd dist && shasum -a 256 "${artifact_names[@]}" > SHA256SUMS)
else
    echo "ERROR: neither sha256sum nor shasum is available for checksums" >&2
    exit 1
fi
artifacts+=("dist/SHA256SUMS")

echo "==> Creating GitHub release ${release_tag}"
gh_args=(release create "${release_tag}")
gh_args+=("${artifacts[@]}")
gh_args+=(--title "${release_title}")

if [[ -n "${notes_file}" ]]; then
    gh_args+=(--notes-file "${notes_file}")
else
    gh_args+=(--generate-notes)
fi

if [[ "${is_prerelease}" == "true" ]]; then
    gh_args+=(--prerelease --target "${head_commit}")
else
    gh_args+=(--verify-tag)
fi

gh "${gh_args[@]}"

echo ""
echo "Release complete:"
echo "  tag:   ${release_tag}"
echo "  title: ${release_title}"
echo "  mode:  $( [[ "${is_prerelease}" == "true" ]] && echo nightly || echo tagged-release )"
