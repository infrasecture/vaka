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
#       ./build.sh --release --packages --push
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

sha256_of() {
    local path="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "${path}" | awk '{print $1}'
        return
    fi
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "${path}" | awk '{print $1}'
        return
    fi
    echo "ERROR: neither sha256sum nor shasum is available" >&2
    exit 1
}

write_formula_file() {
    local formula_path="$1"
    local class_name="$2"
    local formula_version="$3"
    local tag="$4"
    local amd_sha="$5"
    local arm_sha="$6"
    local desc_suffix="$7"

    cat > "${formula_path}" <<EOF
class ${class_name} < Formula
  desc "Declarative egress firewall for Docker containers${desc_suffix}"
  homepage "https://github.com/infrasecture/vaka"
  version "${formula_version}"
  license "LGPL-2.1-only"

  on_arm do
    url "https://github.com/infrasecture/vaka/releases/download/${tag}/vaka-brew-darwin-arm64.tar.gz"
    sha256 "${arm_sha}"
  end

  on_intel do
    url "https://github.com/infrasecture/vaka/releases/download/${tag}/vaka-brew-darwin-amd64.tar.gz"
    sha256 "${amd_sha}"
  end

  def install
    bin.install "vaka"
    bin.install "vaka-init"
  end

  test do
    output = shell_output("#{bin}/vaka version")
    assert_match "vaka ", output
  end
end
EOF
}

make_brew_bundle() {
    local darwin_bin="$1"
    local init_bin="$2"
    local out_tar="$3"
    local tmp
    tmp="$(mktemp -d)"
    cp "${darwin_bin}" "${tmp}/vaka"
    cp "${init_bin}" "${tmp}/vaka-init"
    chmod 0755 "${tmp}/vaka" "${tmp}/vaka-init"
    tar -C "${tmp}" -czf "${out_tar}" vaka vaka-init
    rm -rf -- "${tmp}"
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

if ! git config --file .gitmodules --get "submodule.homebrew-tap.path" >/dev/null 2>&1; then
    echo "ERROR: homebrew-tap submodule is not configured in .gitmodules" >&2
    exit 1
fi

echo "==> Ensuring homebrew-tap submodule checkout..."
git submodule update --init --checkout -- homebrew-tap
tap_path="${SCRIPT_DIR}/homebrew-tap"
if [[ ! -d "${tap_path}" ]]; then
    echo "ERROR: homebrew-tap submodule directory is missing at ${tap_path}" >&2
    exit 1
fi
if [[ -n "$(git -C "${tap_path}" status --porcelain)" ]]; then
    echo "ERROR: homebrew-tap submodule has uncommitted changes; clean it first" >&2
    exit 1
fi

# Keep tap branch current before writing formulas.
git -C "${tap_path}" fetch origin main
git -C "${tap_path}" checkout main
git -C "${tap_path}" pull --ff-only origin main

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

echo "==> Building release artifacts and publishing container images with VERSION=${release_tag}"
VERSION="${release_tag}" ./build.sh --release --packages --push

artifacts=()
artifact_names=()
pkg_version="${release_tag#v}"

# GitHub release payload policy:
#   - include package artifacts (.deb/.rpm/.pkg.tar.*)
#   - include macOS vaka binaries
#   - include Homebrew bundles containing both vaka + vaka-init
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

required_runtime=(
    "dist/vaka-init-linux-amd64"
    "dist/vaka-init-linux-arm64"
)
for path in "${required_runtime[@]}"; do
    if [[ ! -f "${path}" ]]; then
        echo "ERROR: missing required runtime binary: ${path}" >&2
        exit 1
    fi
done

brew_bundle_amd="dist/vaka-brew-darwin-amd64.tar.gz"
brew_bundle_arm="dist/vaka-brew-darwin-arm64.tar.gz"
make_brew_bundle "dist/vaka-darwin-amd64" "dist/vaka-init-linux-amd64" "${brew_bundle_amd}"
make_brew_bundle "dist/vaka-darwin-arm64" "dist/vaka-init-linux-arm64" "${brew_bundle_arm}"
artifacts+=("${brew_bundle_amd}" "${brew_bundle_arm}")
artifact_names+=("$(basename "${brew_bundle_amd}")" "$(basename "${brew_bundle_arm}")")

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

echo "==> Updating Homebrew tap formulas..."
amd_sha="$(sha256_of "${brew_bundle_amd}")"
arm_sha="$(sha256_of "${brew_bundle_arm}")"

if [[ "${is_prerelease}" == "true" ]]; then
    formula_rel_path="Formula/vaka-nightly.rb"
    formula_class="VakaNightly"
    formula_version="0.0.0-nightly.$(git show -s --date=format:%Y%m%d%H%M --format=%cd HEAD).${head_short}"
    formula_desc_suffix=" (nightly)"
    tap_commit_msg="chore(formula): update vaka-nightly to ${release_tag}"
else
    formula_rel_path="Formula/vaka.rb"
    formula_class="Vaka"
    formula_version="${pkg_version}"
    formula_desc_suffix=""
    tap_commit_msg="chore(formula): update vaka to ${release_tag}"
fi

write_formula_file \
    "${tap_path}/${formula_rel_path}" \
    "${formula_class}" \
    "${formula_version}" \
    "${release_tag}" \
    "${amd_sha}" \
    "${arm_sha}" \
    "${formula_desc_suffix}"

git -C "${tap_path}" add "${formula_rel_path}"
if ! git -C "${tap_path}" diff --cached --quiet; then
    git -C "${tap_path}" commit -m "${tap_commit_msg}"
    git -C "${tap_path}" push origin HEAD
else
    echo "    Homebrew formula unchanged; skipping tap commit/push."
fi

git add homebrew-tap
if ! git diff --cached --quiet -- homebrew-tap; then
    git commit -m "chore(submodule): bump homebrew-tap after ${release_tag} release"
else
    echo "    Submodule pointer unchanged; no superproject commit needed."
fi

echo ""
echo "Release complete:"
echo "  tag:   ${release_tag}"
echo "  title: ${release_title}"
echo "  mode:  $( [[ "${is_prerelease}" == "true" ]] && echo nightly || echo tagged-release )"
