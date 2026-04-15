# docker/init/Dockerfile
# Assembles the emsi/vaka-init scratch image from two pre-built static binaries.
# The build context must contain exactly two files:
#   vaka-init   — static vaka-init binary (native arch, built by build.sh)
#   nft         — static nft binary (native arch, extracted from emsi/nft-static)
#
# build.sh creates a minimal temp context and passes it here. No Go build stage,
# no external FROM — the image is just two COPYs into scratch.
#
# Usage in a harness Dockerfile:
#   FROM emsi/vaka-init:latest AS vaka
#   FROM ubuntu:24.04
#   COPY --from=vaka /opt/vaka/bin/vaka-init /usr/local/sbin/vaka-init
#   COPY --from=vaka /opt/vaka/bin/nft       /usr/local/sbin/nft

ARG VERSION=dev
ARG NFTABLES_VERSION=unknown

FROM scratch
ARG VERSION=dev
LABEL org.opencontainers.image.title="emsi/vaka-init" \
      org.opencontainers.image.description="vaka-init + nft static binaries for the vaka secure container layer" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/infrasecture/vaka"

COPY vaka-init /opt/vaka/bin/vaka-init
COPY nft       /opt/vaka/bin/nft
