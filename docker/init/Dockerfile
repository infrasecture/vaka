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
ARG VERSION=dev

FROM ${NFTABLES_IMAGE} AS nft

FROM golang:1.25-alpine AS builder

# Disable workspace mode: go.work says "use ." which is implied inside the
# container, and GOWORK=off keeps the build hermetic.
ENV GOWORK=off

WORKDIR /src

# Copy go.mod and go.sum first for dependency layer caching.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build vaka-init as a fully static binary.
# CGO_ENABLED=0 disables cgo; -tags netgo,osusergo force pure-Go net and
# user/group implementations; -extldflags=-static guards against any indirect
# static archive pulls. The result has no shared library dependencies and
# runs on any Linux base including scratch.
ARG VERSION
RUN CGO_ENABLED=0 GOOS=linux GOTOOLCHAIN=local go build \
    -trimpath \
    -tags "netgo,osusergo" \
    -ldflags="-s -w -extldflags=-static -X main.version=${VERSION}" \
    -o /out/vaka-init \
    ./cmd/vaka-init

FROM scratch

ARG VERSION
LABEL org.opencontainers.image.title="emsi/vaka-init" \
      org.opencontainers.image.description="vaka-init + nft static binaries for the vaka secure container layer" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/infrasecture/vaka"

COPY --from=nft     /opt/nftables/bin/nft   /opt/vaka/bin/nft
COPY --from=builder /out/vaka-init          /opt/vaka/bin/vaka-init
