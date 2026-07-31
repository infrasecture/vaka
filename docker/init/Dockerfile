# docker/init/Dockerfile
# Assembles the emsi/vaka-init scratch image from two pre-built static binaries.
# The build context contains a normalized rootfs/ tree assembled by build.sh:
#   rootfs/opt/vaka/sbin/vaka-init — static vaka-init binary
#   rootfs/opt/vaka/sbin/nft       — static nft binary
#
# Usage when baking into a harness image:
#   FROM emsi/vaka-init:runtime-v0.1.0 AS vaka
#   FROM ubuntu:24.04
#   COPY --from=vaka /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
#   COPY --from=vaka /opt/vaka/sbin/nft       /opt/vaka/sbin/nft

ARG RUNTIME_VERSION=dev
ARG NFTABLES_VERSION=unknown
ARG RUNTIME_INPUTS_SHA256=unknown
ARG NFT_INPUTS_SHA256=unknown

FROM scratch
ARG RUNTIME_VERSION=dev
ARG NFTABLES_VERSION=unknown
ARG RUNTIME_INPUTS_SHA256=unknown
ARG NFT_INPUTS_SHA256=unknown
LABEL org.opencontainers.image.title="emsi/vaka-init" \
      org.opencontainers.image.description="vaka-init + nft static binaries for the vaka secure container layer" \
      org.opencontainers.image.version="${RUNTIME_VERSION}" \
      agent.vaka.runtime.version="${RUNTIME_VERSION}" \
      agent.vaka.runtime.inputs-sha256="${RUNTIME_INPUTS_SHA256}" \
      agent.vaka.nftables.version="${NFTABLES_VERSION}" \
      agent.vaka.nftables.inputs-sha256="${NFT_INPUTS_SHA256}" \
      org.opencontainers.image.source="https://github.com/infrasecture/vaka"

COPY --chmod=0555 rootfs/ /
