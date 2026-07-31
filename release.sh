#!/usr/bin/env bash
# release.sh — prepare and optionally publish a vaka release from a builder host.
#
# Stable versions are explicit. Preparation builds and tests without publishing;
# publication consumes the exact prepared state, publishes the runtime, and only
# then creates the Git tag and GitHub/Homebrew release data.
#
# Requirements:
#   - git
#   - docker
#   - gh (authenticated to target repo)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"
source "${SCRIPT_DIR}/scripts/lib/release-versioning.sh"

usage() {
    cat <<'EOF'
Usage:
  ./release.sh --version vX.Y.Z [--prepare-only|--publish-prepared]
  ./release.sh --nightly [--prepare-only|--publish-prepared]

Options:
  --version VERSION  Stable CLI release version; tag creation happens at publish.
  --nightly          Use the 12-character Git ID and a commit-specific runtime.
  --prepare-only     Build, test, package, and preflight without publishing.
  --publish-prepared Publish existing dist/ prepared state; do not rebuild.
  --title TITLE      Override GitHub release title (default: tag).
  --notes-file PATH  Use explicit release notes file (otherwise --generate-notes).
  -h, --help         Show this help.

Behavior:
  - With neither phase flag, preparation and publication run in sequence.
  - Preparation never writes registry, Git, GitHub, or Homebrew state.
  - Stable tags and :latest are updated only after immutable runtime publication.
  - Nightlies never update the runtime :latest tag.
  - If Homebrew formula changes, script updates and pushes homebrew-tap,
    then commits and pushes the vaka repo submodule pointer bump.
EOF
}

nightly=false
requested_version=""
prepare_only=false
publish_prepared=false
release_title=""
notes_file=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --nightly)
            nightly=true
            shift
            ;;
        --version)
            [[ $# -ge 2 ]] || { echo "ERROR: --version requires a value" >&2; exit 1; }
            requested_version="$2"
            shift 2
            ;;
        --prepare-only)
            prepare_only=true
            shift
            ;;
        --publish-prepared)
            publish_prepared=true
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

if [[ "${prepare_only}" == true && "${publish_prepared}" == true ]]; then
    echo "ERROR: --prepare-only and --publish-prepared are mutually exclusive" >&2
    exit 1
fi
if [[ "${nightly}" == true && -n "${requested_version}" ]]; then
    echo "ERROR: --nightly and --version are mutually exclusive" >&2
    exit 1
fi
if [[ "${nightly}" == false ]]; then
    [[ -n "${requested_version}" ]] || { echo "ERROR: stable releases require --version vX.Y.Z" >&2; exit 1; }
    vaka_require_stable_version "CLI release version" "${requested_version}"
fi

do_prepare=true
do_publish=true
[[ "${prepare_only}" == true ]] && do_publish=false
[[ "${publish_prepared}" == true ]] && do_prepare=false

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
    local out_tar="$2"
    local tmp
    tmp="$(mktemp -d)"
    cp "${darwin_bin}" "${tmp}/vaka"
    chmod 0755 "${tmp}/vaka"
    tar -C "${tmp}" -czf "${out_tar}" vaka
    rm -rf -- "${tmp}"
}

# govulncheck_gate scans the module for *reachable* vulnerabilities using the
# release build image (so its standard-library analysis matches the shipped
# binaries) and aborts the release if any reachable vulnerability is not listed
# in .govulncheck-allowlist. Vulnerabilities present but not called are ignored;
# so are allowlisted ones (reachable but with no fix available yet). Override for
# a local/dev build with VAKA_SKIP_VULNCHECK=1 when a currently-unfixable
# dependency would otherwise block functionality testing.
#   Keep the default image in sync with build.sh's GOLANG_IMAGE.
VULNCHECK_RESULT=passed
govulncheck_gate() {
    local image="${GOLANG_IMAGE:-golang:1.25.12-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587}"
    local vuln_version="${GOVULNCHECK_VERSION:-v1.6.0}"
    local allowlist_file="${SCRIPT_DIR}/.govulncheck-allowlist"
    local out rc reachable allow blocking count id attempt

    if [[ -n "${VAKA_SKIP_VULNCHECK:-}" ]]; then
        VULNCHECK_RESULT=skipped
        echo "==> govulncheck gate SKIPPED (VAKA_SKIP_VULNCHECK set)." >&2
        echo "    Do not skip this for a public release." >&2
        return 0
    fi

    [[ "${vuln_version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
        echo "ERROR: GOVULNCHECK_VERSION must be a canonical vX.Y.Z module version" >&2
        exit 1
    }

    echo "==> govulncheck: scanning for reachable vulnerabilities (${image})..."
    # Reuse build.sh's module/build caches so this does not re-download every
    # dependency (slow, and flaky against the module proxy). Retry a few times to
    # ride out transient proxy/network errors while caches are cold.
    for attempt in 1 2 3; do
        set +e
        out="$(docker run --rm \
            -v "${SCRIPT_DIR}:/src:ro" \
            -v "vaka-gomodcache:/go/pkg/mod" \
            -v "vaka-gobuildcache:/root/.cache/go/build" \
            -w /src "${image}" \
            sh -c "go install golang.org/x/vuln/cmd/govulncheck@${vuln_version} && govulncheck ./..." 2>&1)"
        rc=$?
        set -e
        # 0 = clean, 3 = findings; both are definitive. Anything else is a
        # tool/setup/network error worth retrying.
        [[ ${rc} -eq 0 || ${rc} -eq 3 ]] && break
        echo "    govulncheck run failed (exit ${rc}); retrying (${attempt}/3)..." >&2
    done

    if [[ ${rc} -eq 0 ]]; then
        echo "    No reachable vulnerabilities. OK."
        return 0
    fi
    if [[ ${rc} -ne 3 ]]; then
        echo "ERROR: govulncheck did not run cleanly (exit ${rc}):" >&2
        printf '%s\n' "${out}" | tail -20 >&2
        echo "       Fix the tool/setup, or override with VAKA_SKIP_VULNCHECK=1 for a local build." >&2
        exit 1
    fi

    # In default output, only *called* (reachable) vulns get "Vulnerability #" blocks.
    reachable="$(printf '%s\n' "${out}" | grep '^Vulnerability #' | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u)"
    if [[ -z "${reachable}" ]]; then
        echo "ERROR: govulncheck reported findings (exit 3) but no vulnerability IDs were parsed;" >&2
        echo "       its output format may have changed. Raw tail:" >&2
        printf '%s\n' "${out}" | tail -20 >&2
        exit 1
    fi

    allow=""
    if [[ -f "${allowlist_file}" ]]; then
        allow="$(sed -E 's/#.*//; s/[[:space:]]+//g' "${allowlist_file}" | grep -E '^GO-[0-9]+-[0-9]+$' | sort -u || true)"
    fi
    blocking="$(comm -23 <(printf '%s\n' "${reachable}") <(printf '%s\n' "${allow}") || true)"

    if [[ -z "${blocking}" ]]; then
        count="$(printf '%s\n' "${reachable}" | grep -cE '^GO-')"
        echo "    ${count} reachable, all accepted in .govulncheck-allowlist. OK."
        return 0
    fi

    echo "" >&2
    echo "ERROR: release blocked — reachable vulnerabilities not in .govulncheck-allowlist:" >&2
    while IFS= read -r id; do
        [[ -n "${id}" ]] && echo "         ${id}  https://pkg.go.dev/vuln/${id}" >&2
    done <<<"${blocking}"
    echo "" >&2
    echo "  vaka's code calls these. Choose one:" >&2
    echo "    * bump the offending dependency or Go toolchain, then re-run; or" >&2
    echo "    * if no fix exists yet, add the id(s) to .govulncheck-allowlist with a note; or" >&2
    echo "    * for a LOCAL build only: VAKA_SKIP_VULNCHECK=1 ./release.sh" >&2
    exit 1
}

go_test_gate() {
    local image="${GOLANG_IMAGE:-golang:1.25.12-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587}"
    echo "==> Running release test suite (${image})..."
    docker run --rm \
        --volume "${SCRIPT_DIR}:/src:ro" \
        --volume "vaka-gomodcache:/go/pkg/mod" \
        --volume "vaka-gobuildcache:/root/.cache/go/build" \
        --workdir /src \
        --env GOWORK=off \
        "${image}" \
        go test ./...
}

require_clean_source() {
    git rev-parse --is-inside-work-tree >/dev/null 2>&1 || {
        echo "ERROR: not inside a git repository" >&2
        exit 1
    }
    [[ -z "$(git status --porcelain)" ]] || {
        echo "ERROR: working tree is not clean; commit/stash changes before release" >&2
        exit 1
    }
}

prepare_artifacts() {
    local pkg_version="$1"
    local head_commit="$2"
    local path name hash
    local -a artifact_names=()
    local -a package_files=()

    artifacts=(
        dist/vaka-darwin-amd64
        dist/vaka-darwin-arm64
        dist/component-manifest.json
    )
    for path in "${artifacts[@]}"; do
        [[ -f "${path}" ]] || { echo "ERROR: missing release artifact: ${path}" >&2; exit 1; }
    done

    brew_bundle_amd="dist/vaka-brew-darwin-amd64.tar.gz"
    brew_bundle_arm="dist/vaka-brew-darwin-arm64.tar.gz"
    make_brew_bundle dist/vaka-darwin-amd64 "${brew_bundle_amd}"
    make_brew_bundle dist/vaka-darwin-arm64 "${brew_bundle_arm}"
    artifacts+=("${brew_bundle_amd}" "${brew_bundle_arm}")

    mapfile -d '' package_files < <(
        find dist -maxdepth 1 -type f \
            \( -name "vaka_${pkg_version}_*.deb" -o -name "vaka-${pkg_version}-*.rpm" \
               -o -name "vaka-${pkg_version}-*.pkg.tar.*" -o -name "vaka_${pkg_version}_*.pkg.tar.*" \) \
            -print0 | LC_ALL=C sort -z
    )
    [[ "$(printf '%s\n' "${package_files[@]}" | grep -c '\.deb$')" -eq 2 ]] || { echo "ERROR: release requires exactly two .deb packages" >&2; exit 1; }
    [[ "$(printf '%s\n' "${package_files[@]}" | grep -c '\.rpm$')" -eq 2 ]] || { echo "ERROR: release requires exactly two .rpm packages" >&2; exit 1; }
    [[ "$(printf '%s\n' "${package_files[@]}" | grep -c '\.pkg\.tar\.')" -eq 2 ]] || { echo "ERROR: release requires exactly two Arch packages" >&2; exit 1; }
    artifacts+=("${package_files[@]}")

    : >dist/SHA256SUMS
    for path in "${artifacts[@]}"; do
        name="$(basename "${path}")"
        hash="$(sha256_of "${path}")"
        printf '%s  %s\n' "${hash}" "${name}" >>dist/SHA256SUMS
        artifact_names+=("${name}")
    done
    artifacts+=(dist/SHA256SUMS)

    artifacts_file=dist/.vaka-release-artifacts
    {
        printf 'FORMAT=1\nGIT_COMMIT=%s\nCLI_VERSION=%s\n' "${head_commit}" "${release_tag}"
        for path in "${artifacts[@]}"; do
            printf 'ARTIFACT=%s\n' "${path}"
        done
    } >"${artifacts_file}"
}

load_prepared_artifacts() {
    local path hash name expected
    local artifacts_file=dist/.vaka-release-artifacts
    [[ -f "${artifacts_file}" ]] || { echo "ERROR: prepared artifact list is missing" >&2; exit 1; }
    [[ "$(vaka_state_require "${artifacts_file}" FORMAT)" == 1 ]] || { echo "ERROR: unsupported artifact-list format" >&2; exit 1; }
    [[ "$(vaka_state_require "${artifacts_file}" GIT_COMMIT)" == "${head_commit}" ]] || { echo "ERROR: artifact list belongs to another commit" >&2; exit 1; }
    [[ "$(vaka_state_require "${artifacts_file}" CLI_VERSION)" == "${release_tag}" ]] || { echo "ERROR: artifact list belongs to another CLI version" >&2; exit 1; }

    mapfile -t artifacts < <(awk -F= '$1 == "ARTIFACT" { print substr($0, 10) }' "${artifacts_file}")
    [[ "${#artifacts[@]}" -gt 0 ]] || { echo "ERROR: prepared artifact list is empty" >&2; exit 1; }
    for path in "${artifacts[@]}"; do
        [[ "${path}" =~ ^dist/[A-Za-z0-9._+-]+$ && "${path}" != *..* ]] || { echo "ERROR: unsafe prepared artifact path: ${path}" >&2; exit 1; }
        [[ -f "${path}" ]] || { echo "ERROR: prepared artifact is missing: ${path}" >&2; exit 1; }
        case "$(basename "${path}")" in
            nft-*|vaka-init-*) echo "ERROR: runtime internals must not be GitHub release assets: ${path}" >&2; exit 1 ;;
        esac
    done

    [[ -f dist/SHA256SUMS ]] || { echo "ERROR: SHA256SUMS is missing" >&2; exit 1; }
    while read -r expected name; do
        [[ "${expected}" =~ ^[0-9a-f]{64}$ && "${name}" =~ ^[A-Za-z0-9._+-]+$ ]] || { echo "ERROR: malformed SHA256SUMS entry" >&2; exit 1; }
        path="dist/${name}"
        [[ -f "${path}" ]] || { echo "ERROR: checksummed artifact is missing: ${path}" >&2; exit 1; }
        hash="$(sha256_of "${path}")"
        [[ "${hash}" == "${expected}" ]] || { echo "ERROR: artifact checksum mismatch: ${path}" >&2; exit 1; }
    done <dist/SHA256SUMS

    brew_bundle_amd=dist/vaka-brew-darwin-amd64.tar.gz
    brew_bundle_arm=dist/vaka-brew-darwin-arm64.tar.gz
    [[ -f "${brew_bundle_amd}" && -f "${brew_bundle_arm}" ]] || { echo "ERROR: prepared Homebrew bundles are missing" >&2; exit 1; }
}

write_release_gates() {
    local state_sha
    state_sha="$(sha256_of dist/.vaka-release-state)"
    {
        printf 'FORMAT=1\nGIT_COMMIT=%s\nCLI_VERSION=%s\n' "${head_commit}" "${release_tag}"
        printf 'BUILD_STATE_SHA256=%s\n' "${state_sha}"
        printf 'ARTIFACT_LIST_SHA256=%s\n' "$(sha256_of dist/.vaka-release-artifacts)"
        printf 'CHECKSUMS_SHA256=%s\n' "$(sha256_of dist/SHA256SUMS)"
        printf 'GO_TEST=passed\nVULNCHECK=%s\nIMAGE_MOUNT_SMOKE=passed\nREGISTRY_PREFLIGHT=passed\n' "${VULNCHECK_RESULT}"
    } >dist/.vaka-release-gates
}

verify_release_gates() {
    local gates=dist/.vaka-release-gates
    [[ -f "${gates}" ]] || { echo "ERROR: prepared release gates are missing" >&2; exit 1; }
    [[ "$(vaka_state_require "${gates}" FORMAT)" == 1 ]] || { echo "ERROR: unsupported release-gate format" >&2; exit 1; }
    [[ "$(vaka_state_require "${gates}" GIT_COMMIT)" == "${head_commit}" ]] || { echo "ERROR: release gates belong to another commit" >&2; exit 1; }
    [[ "$(vaka_state_require "${gates}" CLI_VERSION)" == "${release_tag}" ]] || { echo "ERROR: release gates belong to another CLI version" >&2; exit 1; }
    [[ "$(vaka_state_require "${gates}" BUILD_STATE_SHA256)" == "$(sha256_of dist/.vaka-release-state)" ]] || { echo "ERROR: prepared build state changed after release gates" >&2; exit 1; }
    [[ "$(vaka_state_require "${gates}" ARTIFACT_LIST_SHA256)" == "$(sha256_of dist/.vaka-release-artifacts)" ]] || { echo "ERROR: prepared artifact list changed after release gates" >&2; exit 1; }
    [[ "$(vaka_state_require "${gates}" CHECKSUMS_SHA256)" == "$(sha256_of dist/SHA256SUMS)" ]] || { echo "ERROR: prepared checksums changed after release gates" >&2; exit 1; }
    for key in GO_TEST VULNCHECK IMAGE_MOUNT_SMOKE REGISTRY_PREFLIGHT; do
        [[ "$(vaka_state_require "${gates}" "${key}")" == passed ]] || { echo "ERROR: release gate ${key} did not pass" >&2; exit 1; }
    done
}

initialize_publish_services() {
    require_cmd gh
    [[ -z "${notes_file}" || -f "${notes_file}" ]] || { echo "ERROR: release notes file not found: ${notes_file}" >&2; exit 1; }
    git config --file .gitmodules --get "submodule.homebrew-tap.path" >/dev/null 2>&1 || {
        echo "ERROR: homebrew-tap submodule is not configured" >&2
        exit 1
    }

    origin_url="$(git config --get remote.origin.url || true)"
    repo_slug="$(printf '%s' "${origin_url}" | sed -E -e 's#^git@github\.com:##' -e 's#^https://github\.com/##' -e 's#\.git$##')"
    if [[ -n "${repo_slug}" && "${repo_slug}" != "${origin_url}" ]]; then
        gh repo view "${repo_slug}" >/dev/null 2>&1 || { echo "ERROR: gh cannot access ${repo_slug}" >&2; exit 1; }
    else
        gh auth token >/dev/null 2>&1 || { echo "ERROR: gh has no active auth token" >&2; exit 1; }
    fi

    echo "==> Preparing Homebrew tap checkout..."
    git submodule update --init --checkout -- homebrew-tap
    tap_path="${SCRIPT_DIR}/homebrew-tap"
    [[ -d "${tap_path}" ]] || { echo "ERROR: homebrew-tap checkout is missing" >&2; exit 1; }
    [[ -z "$(git -C "${tap_path}" status --porcelain)" ]] || { echo "ERROR: homebrew-tap has uncommitted changes" >&2; exit 1; }

    git fetch --quiet origin
    git branch -r --contains "${head_commit}" | grep -q . || {
        echo "ERROR: release commit ${head_commit} is not reachable from an origin branch" >&2
        exit 1
    }

    validate_release_tag
    github_release_exists=false
    if gh release view "${release_tag}" >/dev/null 2>&1; then
        existing_prerelease="$(gh release view "${release_tag}" --json isPrerelease --jq .isPrerelease)"
        [[ "${existing_prerelease}" == "${is_prerelease}" ]] || { echo "ERROR: existing GitHub release has the wrong prerelease state" >&2; exit 1; }
        github_release_exists=true
    fi
}

validate_release_tag() {
    local target remote_target
    if git rev-parse -q --verify "refs/tags/${release_tag}" >/dev/null; then
        target="$(git rev-list -n1 "${release_tag}")"
        [[ "${target}" == "${head_commit}" ]] || { echo "ERROR: local tag ${release_tag} points to ${target}" >&2; exit 1; }
    fi

    remote_target="$(git ls-remote --tags origin "refs/tags/${release_tag}^{}" | awk '{print $1}')"
    [[ -n "${remote_target}" ]] || remote_target="$(git ls-remote --tags origin "refs/tags/${release_tag}" | awk '{print $1}')"
    if [[ -n "${remote_target}" ]]; then
        [[ "${remote_target}" == "${head_commit}" ]] || { echo "ERROR: remote tag ${release_tag} points to ${remote_target}" >&2; exit 1; }
    fi
}

ensure_release_tag() {
    validate_release_tag
    if ! git rev-parse -q --verify "refs/tags/${release_tag}" >/dev/null; then
        git tag "${release_tag}" "${head_commit}"
    fi
    if ! git ls-remote --exit-code --tags origin "refs/tags/${release_tag}" >/dev/null 2>&1; then
        echo "==> Pushing release tag ${release_tag}"
        git push origin "refs/tags/${release_tag}:refs/tags/${release_tag}"
    fi
}

require_cmd git
require_cmd docker
require_clean_source

head_commit="$(git rev-parse --verify HEAD)"
head_short="$(git rev-parse --short=12 HEAD)"
is_prerelease=false
if [[ "${nightly}" == true ]]; then
    release_tag="${head_short}"
    is_prerelease=true
else
    release_tag="${requested_version}"
fi
pkg_version="${release_tag#v}"
[[ -n "${release_title}" ]] || release_title="$( [[ "${nightly}" == true ]] && printf 'vaka-%s' "${head_short}" || printf '%s' "${release_tag}" )"

if [[ "${do_prepare}" == true ]]; then
    docker volume create vaka-gomodcache >/dev/null
    docker volume create vaka-gobuildcache >/dev/null
    govulncheck_gate
    go_test_gate

    echo "==> Preparing release ${release_tag}; no publication occurs in this phase"
    ARCHS="amd64 arm64" \
    CLI_TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64" \
        ./build.sh --release --packages --cli-version "${release_tag}" --rebuild-cli

    echo "==> Running real image-mount smoke test"
    ./scripts/smoke-image-mount.sh

    prepare_artifacts "${pkg_version}" "${head_commit}"
    echo "==> Read-only runtime registry preflight"
    ./scripts/release-runtime.sh preflight
    write_release_gates
    echo "Prepared release ${release_tag}; registry, Git tags, GitHub, and Homebrew were not modified."
fi

if [[ "${do_publish}" == false ]]; then
    exit 0
fi

load_prepared_artifacts
verify_release_gates
initialize_publish_services

state_channel="$(vaka_state_require dist/.vaka-release-state CHANNEL)"
state_cli="$(vaka_state_require dist/.vaka-release-state CLI_VERSION)"
state_commit="$(vaka_state_require dist/.vaka-release-state GIT_COMMIT)"
[[ "${state_cli}" == "${release_tag}" && "${state_commit}" == "${head_commit}" ]] || { echo "ERROR: prepared build does not match requested release" >&2; exit 1; }
if [[ "${nightly}" == true ]]; then
    [[ "${state_channel}" == nightly ]] || { echo "ERROR: prepared build is not nightly" >&2; exit 1; }
else
    [[ "${state_channel}" == stable ]] || { echo "ERROR: prepared build is not stable" >&2; exit 1; }
fi

echo "==> Repeating runtime preflight immediately before publication"
./scripts/release-runtime.sh preflight
./scripts/release-runtime.sh publish

# Git identity is intentionally created only after immutable runtime publication.
ensure_release_tag

if [[ "${github_release_exists}" == true ]]; then
    echo "==> GitHub release exists; replacing its prepared assets for retry safety"
    gh release upload "${release_tag}" "${artifacts[@]}" --clobber
else
    echo "==> Creating GitHub release ${release_tag}"
    gh_args=(release create "${release_tag}" "${artifacts[@]}" --title "${release_title}")
    if [[ -n "${notes_file}" ]]; then
        gh_args+=(--notes-file "${notes_file}")
    elif [[ "${is_prerelease}" == true ]]; then
        gh_args+=(--notes "Nightly build for commit ${head_commit}")
    else
        gh_args+=(--generate-notes)
    fi
    [[ "${is_prerelease}" == true ]] && gh_args+=(--prerelease)
    gh_args+=(--verify-tag)
    gh "${gh_args[@]}"
fi

echo "==> Updating Homebrew tap formula..."
git -C "${tap_path}" fetch origin main
git -C "${tap_path}" checkout -B main origin/main
amd_sha="$(sha256_of "${brew_bundle_amd}")"
arm_sha="$(sha256_of "${brew_bundle_arm}")"
if [[ "${is_prerelease}" == true ]]; then
    formula_rel_path=Formula/vaka-nightly.rb
    formula_class=VakaNightly
    formula_version="0.0.0-nightly.$(git show -s --date=format:%Y%m%d%H%M --format=%cd "${head_commit}").${head_short}"
    formula_desc_suffix=" (nightly)"
    tap_commit_msg="chore(formula): update vaka-nightly to ${release_tag}"
else
    formula_rel_path=Formula/vaka.rb
    formula_class=Vaka
    formula_version="${pkg_version}"
    formula_desc_suffix=""
    tap_commit_msg="chore(formula): update vaka to ${release_tag}"
fi

write_formula_file "${tap_path}/${formula_rel_path}" "${formula_class}" "${formula_version}" \
    "${release_tag}" "${amd_sha}" "${arm_sha}" "${formula_desc_suffix}"
git -C "${tap_path}" add "${formula_rel_path}"
if ! git -C "${tap_path}" diff --cached --quiet; then
    git -C "${tap_path}" commit -m "${tap_commit_msg}"
    git -C "${tap_path}" push origin HEAD
else
    echo "    Homebrew formula unchanged; skipping tap commit/push."
fi

git add homebrew-tap
vaka_repo_commit_created=false
if ! git diff --cached --quiet -- homebrew-tap; then
    git commit -m "chore(submodule): bump homebrew-tap after ${release_tag} release"
    vaka_repo_commit_created=true
fi
if [[ "${vaka_repo_commit_created}" == true ]]; then
    current_branch="$(git symbolic-ref --quiet --short HEAD || true)"
    [[ -n "${current_branch}" ]] || { echo "ERROR: cannot push submodule update from detached HEAD" >&2; exit 1; }
    git push origin "${current_branch}"
fi

echo "Release complete: ${release_tag} ($( [[ "${is_prerelease}" == true ]] && echo nightly || echo stable ))"
