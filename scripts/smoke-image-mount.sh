#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
source "${REPO_ROOT}/scripts/lib/release-versioning.sh"

die() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

normalize_version() {
    printf '%s\n' "${1#v}"
}

case "$(uname -m)" in
    x86_64) native_arch=amd64 ;;
    aarch64|arm64) native_arch=arm64 ;;
    *) die "unsupported host architecture: $(uname -m)" ;;
esac

VAKA_BIN="${VAKA_BIN:-${REPO_ROOT}/dist/vaka-linux-${native_arch}}"
SERVICE_IMAGE="${VAKA_SMOKE_SERVICE_IMAGE:-alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d}"
EXPECT_ENGINE="${VAKA_SMOKE_EXPECT_ENGINE_VERSION:-}"
EXPECT_COMPOSE="${VAKA_SMOKE_EXPECT_COMPOSE_VERSION:-}"

command -v docker >/dev/null 2>&1 || die "docker CLI not found"
[[ -x "${VAKA_BIN}" ]] || die "Vaka binary is not executable: ${VAKA_BIN}; run ./build.sh --rebuild-go first"

runtime_version="$(tr -d '[:space:]' < "${REPO_ROOT}/internal/runtimebundle/VERSION")"
prepared_state="${REPO_ROOT}/dist/.vaka-release-state"
if [[ -f "${prepared_state}" ]]; then
    prepared_commit="$(vaka_state_require "${prepared_state}" GIT_COMMIT 2>/dev/null || true)"
    current_commit="$(git -C "${REPO_ROOT}" rev-parse --verify HEAD)"
    if [[ "${prepared_commit}" == "${current_commit}" ]]; then
        runtime_version="$(vaka_state_require "${prepared_state}" RUNTIME_EFFECTIVE_VERSION)"
    fi
fi
[[ -n "${runtime_version}" ]] || die "runtime bundle version is empty"
engine_version="$(docker version --format '{{.Server.Version}}')" || die "cannot query Docker Engine"
daemon_arch_raw="$(docker version --format '{{.Server.Arch}}')" || die "cannot query Docker target architecture"
case "${daemon_arch_raw}" in
    amd64|x86_64) daemon_arch=amd64 ;;
    arm64|aarch64) daemon_arch=arm64 ;;
    *) die "unsupported Docker target architecture: ${daemon_arch_raw}" ;;
esac
runtime_ref="emsi/vaka-init:runtime-${runtime_version}-${daemon_arch}"

compose_version="$(docker compose version --short)" || die "cannot query Docker Compose"

if [[ -n "${EXPECT_ENGINE}" ]] && \
   [[ "$(normalize_version "${engine_version}")" != "$(normalize_version "${EXPECT_ENGINE}")" ]]; then
    die "Docker Engine ${engine_version} selected; expected exactly ${EXPECT_ENGINE}"
fi
if [[ -n "${EXPECT_COMPOSE}" ]] && \
   [[ "$(normalize_version "${compose_version}")" != "$(normalize_version "${EXPECT_COMPOSE}")" ]]; then
    die "Docker Compose ${compose_version} selected; expected exactly ${EXPECT_COMPOSE}"
fi

printf '==> Docker Engine %s; Docker Compose %s\n' "${engine_version}" "${compose_version}"
"${VAKA_BIN}" doctor

runtime_id="$(docker image inspect "${runtime_ref}" --format '{{.Id}}' 2>/dev/null)" || \
    die "runtime image ${runtime_ref} is absent from the selected Docker target; run ./build.sh first or load the image into that target"
[[ "${runtime_id}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "runtime image has invalid local ID: ${runtime_id}"

remove_service_image=false
if ! docker image inspect "${SERVICE_IMAGE}" >/dev/null 2>&1; then
    printf '==> Pulling smoke service image %s\n' "${SERVICE_IMAGE}"
    docker image pull "${SERVICE_IMAGE}" >/dev/null
    remove_service_image=true
fi

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/vaka-image-mount-smoke.XXXXXX")"
project="vaka-image-mount-smoke-$$"
compose_file="${tmp_dir}/compose.yaml"
policy_file="${tmp_dir}/vaka.yaml"
container_id=""

cleanup() {
    local exit_code=$?
    set +e
    docker compose --project-name "${project}" --file "${compose_file}" down --volumes --remove-orphans >/dev/null 2>&1
    if [[ "${remove_service_image}" == "true" ]]; then
        docker image rm "${SERVICE_IMAGE}" >/dev/null 2>&1
    fi
    rm -rf -- "${tmp_dir}"
    return "${exit_code}"
}
trap cleanup EXIT

export VAKA_SMOKE_SERVICE_IMAGE="${SERVICE_IMAGE}"

cat > "${compose_file}" <<'YAML'
services:
  app:
    image: "${VAKA_SMOKE_SERVICE_IMAGE}"
    command: ["sleep", "3600"]
    cap_drop: [SETUID, SETGID]
    healthcheck:
      test: ["CMD-SHELL", "sed -n '/^Cap/p' /proc/self/status > /tmp/vaka-health-caps"]
      interval: 1s
      timeout: 5s
      retries: 10
YAML

cat > "${policy_file}" <<'YAML'
apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy

services:
  app:
    network:
      egress:
        defaultAction: reject
YAML

printf '==> Starting smoke service through Vaka\n'
"${VAKA_BIN}" \
    "--vaka-file=${policy_file}" \
    --vaka-pull=never \
    compose \
    --project-name "${project}" \
    --file "${compose_file}" \
    up --detach

container_id="$(docker compose --project-name "${project}" --file "${compose_file}" ps --quiet app)"
[[ -n "${container_id}" ]] || die "Compose did not return a container ID for the smoke service"

container_state="$(docker container inspect "${container_id}" --format '{{.State.Status}}')"
if [[ "${container_state}" != "running" ]]; then
    docker container logs "${container_id}" >&2 || true
    die "smoke service is ${container_state}, expected running"
fi

requested_mount="$(docker container inspect "${container_id}" --format '{{range .HostConfig.Mounts}}{{if eq .Target "/opt/vaka"}}{{printf "%s|%s|%t|%s" .Type .Source .ReadOnly .ImageOptions.Subpath}}{{end}}{{end}}')"
IFS='|' read -r requested_type requested_source requested_read_only requested_subpath <<<"${requested_mount}"
[[ "${requested_type}" == image && "${requested_read_only}" == true && "${requested_subpath}" == opt/vaka ]] || \
    die "/opt/vaka mount request is ${requested_mount:-absent}; expected image|<runtime-image-id-or-prefix>|true|opt/vaka"
requested_source_id="$(docker image inspect "${requested_source}" --format '{{.Id}}' 2>/dev/null)" || \
    die "/opt/vaka mount source ${requested_source:-absent} does not resolve on the selected Docker target"
[[ "${requested_source_id}" == "${runtime_id}" ]] || \
    die "/opt/vaka mount source ${requested_source} resolves to ${requested_source_id}; expected ${runtime_id}"

realized_mount="$(docker container inspect "${container_id}" --format '{{range .Mounts}}{{if eq .Destination "/opt/vaka"}}{{printf "%s|%s|%t" .Type .Destination .RW}}{{end}}{{end}}')"
expected_realized="image|/opt/vaka|false"
[[ "${realized_mount}" == "${expected_realized}" ]] || \
    die "/opt/vaka realized mount is ${realized_mount:-absent}; expected ${expected_realized}"

label_image="$(docker container inspect "${container_id}" --format '{{index .Config.Labels "agent.vaka.runtime-image"}}')"
[[ "${label_image}" == "${runtime_id}" ]] || \
    die "container runtime-image label is ${label_image}; expected ${runtime_id}"

label_version="$(docker container inspect "${container_id}" --format '{{index .Config.Labels "agent.vaka.runtime.version"}}')"
[[ "${label_version}" == "${runtime_version}" ]] || \
    die "container runtime-version label is ${label_version}; expected ${runtime_version}"

docker container exec "${container_id}" test -x /opt/vaka/sbin/vaka-init || \
    die "mounted vaka-init is not executable"
docker container exec "${container_id}" test -x /opt/vaka/sbin/nft || \
    die "mounted nft is not executable"

reported_version="$(docker container exec "${container_id}" /opt/vaka/sbin/vaka-init --version)"
[[ "${reported_version}" == "${runtime_version}" ]] || \
    die "mounted vaka-init reports ${reported_version}; expected ${runtime_version}"
docker container exec "${container_id}" /opt/vaka/sbin/nft --version >/dev/null || \
    die "mounted nft cannot execute"

if docker container exec "${container_id}" /bin/sh -c 'printf smoke > /opt/vaka/.vaka-smoke-write' >/dev/null 2>&1; then
    die "write to read-only /opt/vaka mount unexpectedly succeeded"
fi

assert_cap_absent() {
    local source=$1
    local caps=$2
    local cap_name=$3
    local cap_bit=$4
    local name hex value
    for name in CapInh CapPrm CapEff CapBnd CapAmb; do
        hex="$(awk -v key="${name}:" '$1 == key {print $2}' <<<"${caps}")"
        [[ -n "${hex}" ]] || die "${source} did not report ${name}: ${caps}"
        value=$((16#${hex}))
        (( (value & (1 << cap_bit)) == 0 )) || die "${source} retained ${cap_name} in ${name}: ${hex}"
    done
}

assert_no_net_admin() {
    assert_cap_absent "$1" "$2" NET_ADMIN 12
}

printf '==> Verifying Vaka exec trampoline capabilities\n'
exec_caps="$("${VAKA_BIN}" \
    "--vaka-file=${policy_file}" \
    compose \
    --project-name "${project}" \
    --file "${compose_file}" \
    exec -T app /bin/sh -c "sed -n '/^Cap/p' /proc/self/status")"
assert_no_net_admin "vaka exec" "${exec_caps}"

printf '==> Verifying Vaka exec --user identity transition\n'
exec_user_output="$("${VAKA_BIN}" \
    "--vaka-file=${policy_file}" \
    compose \
    --project-name "${project}" \
    --file "${compose_file}" \
    exec -T --user 65534:65534 app /bin/sh -c 'printf "%s:%s\n" "$(id -u)" "$(id -g)"; sed -n "/^Cap/p" /proc/self/status')"
exec_user_identity="${exec_user_output%%$'\n'*}"
exec_user_caps="${exec_user_output#*$'\n'}"
[[ "${exec_user_identity}" == 65534:65534 ]] || \
    die "vaka exec --user ran as ${exec_user_identity}; expected 65534:65534"
assert_no_net_admin "vaka exec --user" "${exec_user_caps}"
assert_cap_absent "vaka exec --user" "${exec_user_caps}" SETGID 6
assert_cap_absent "vaka exec --user" "${exec_user_caps}" SETUID 7

set +e
"${VAKA_BIN}" \
    "--vaka-file=${policy_file}" \
    compose \
    --project-name "${project}" \
    --file "${compose_file}" \
    exec -T app /bin/sh -c 'exit 23'
exec_status=$?
set -e
[[ ${exec_status} -eq 23 ]] || die "vaka exec returned ${exec_status}; expected command status 23"

if "${VAKA_BIN}" \
    "--vaka-file=${policy_file}" \
    compose \
    --project-name "${project}" \
    --file "${compose_file}" \
    exec -T app /opt/vaka/sbin/nft delete table inet vaka >/dev/null 2>&1; then
    die "vaka exec command modified nftables despite dropped NET_ADMIN"
fi

printf '==> Verifying wrapped healthcheck capabilities\n'
health_caps=""
for _ in $(seq 1 20); do
    health_caps="$(docker container exec "${container_id}" sh -c 'cat /tmp/vaka-health-caps 2>/dev/null' || true)"
    [[ -n "${health_caps}" ]] && break
    sleep 0.5
done
[[ -n "${health_caps}" ]] || die "wrapped healthcheck did not record its capabilities"
assert_no_net_admin "healthcheck" "${health_caps}"

printf 'PASS: exact image ID %s is read-only; exec identity switching works; exec and healthchecks drop temporary capabilities\n' "${runtime_id}"
