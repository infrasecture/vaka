# docker/init/Dockerfile
# Builds emsi/vaka-init — scratch image containing:
#   /opt/vaka/bin/nft        (from emsi/nft-static)
#   /opt/vaka/bin/vaka-init  (built from this repo)
#
# Usage in a harness Dockerfile:
#   FROM emsi/vaka-init:latest AS vaka
#   FROM ubuntu:24.04
#   COPY --from=vaka /opt/vaka/bin/vaka-init /usr/local/sbin/vaka-init
#   COPY --from=vaka /opt/vaka/bin/nft       /usr/local/sbin/nft

ARG NFTABLES_IMAGE=emsi/nft-static:1.1.6

FROM ${NFTABLES_IMAGE} AS nft

FROM golang:1.25-alpine AS builder

ENV GOWORK=off

WORKDIR /src

# Copy go.mod and go.sum first for layer caching.
# go.work is intentionally excluded: it just says "use ." which is implied
# inside the container, and its toolchain directive requires go 1.25.0 which
# predates the stable release. GOWORK=off disables workspace mode cleanly.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=local go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/vaka-init \
    ./cmd/vaka-init

FROM scratch

COPY --from=nft     /opt/nftables/bin/nft   /opt/vaka/bin/nft
COPY --from=builder /out/vaka-init          /opt/vaka/bin/vaka-init
