# docker/init/Dockerfile
# Assembles the emsi/vaka-init scratch image from two pre-built static binaries.
# The build context must contain exactly two files:
#   vaka-init   — static vaka-init binary (native arch, built by build.sh)
#   nft         — static nft binary (native arch, extracted from emsi/nft-static)
#
# Usage when baking into a harness image:
#   FROM emsi/vaka-init:latest AS vaka
#   FROM ubuntu:24.04
#   COPY --from=vaka /opt/vaka/sbin/vaka-init /opt/vaka/sbin/vaka-init
#   COPY --from=vaka /opt/vaka/sbin/nft       /opt/vaka/sbin/nft

ARG RUNTIME_VERSION=dev
ARG NFTABLES_VERSION=unknown

FROM scratch
ARG RUNTIME_VERSION=dev
LABEL org.opencontainers.image.title="emsi/vaka-init" \
      org.opencontainers.image.description="vaka-init + nft static binaries for the vaka secure container layer" \
      org.opencontainers.image.version="${RUNTIME_VERSION}" \
      org.opencontainers.image.source="https://github.com/infrasecture/vaka"

COPY --chmod=0555 vaka-init /opt/vaka/sbin/vaka-init
COPY --chmod=0555 nft       /opt/vaka/sbin/nft
VOLUME /opt/vaka
