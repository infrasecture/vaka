FROM alpine:3.21 AS builder

ARG NFTABLES_VERSION=1.1.6
ARG LIBNFTNL_VERSION=1.3.1
ARG LIBMNL_VERSION=1.0.5

ARG NFTABLES_SHA256=372931bda8556b310636a2f9020adc710f9bab66f47efe0ce90bff800ac2530c
ARG LIBNFTNL_SHA256=607da28dba66fbdeccf8ef1395dded9077e8d19f2995f9a4d45a9c2f0bcffba8
ARG LIBMNL_SHA256=274b9b919ef3152bfb3da3a13c950dd60d6e2bcd54230ffeca298d03b40d0525

ARG NETFILTER_GPG_KEY_URL=https://www.netfilter.org/files/coreteam-gpg-key-0xD70D1A666ACF2B21.txt
ARG NETFILTER_GPG_FPR=8C5F7146A1757A65E2422A94D70D1A666ACF2B21
ARG LIBMNL_GPG_KEY_URL=https://www.netfilter.org/files/coreteam-gpg-key-0xD55D978A8A1420E4.txt
ARG LIBMNL_GPG_FPR=37D964ACC04981C75500FB9BD55D978A8A1420E4

WORKDIR /tmp/src

RUN set -eux; \
    apk add --no-cache \
      build-base \
      autoconf \
      automake \
      libtool \
      pkgconf \
      linux-headers \
      bash \
      bison \
      flex \
      curl \
      gnupg \
      xz \
      bzip2 \
      tar \
      coreutils

RUN set -eux; \
    NFTABLES_TARBALL="nftables-${NFTABLES_VERSION}.tar.xz"; \
    LIBNFTNL_TARBALL="libnftnl-${LIBNFTNL_VERSION}.tar.xz"; \
    LIBMNL_TARBALL="libmnl-${LIBMNL_VERSION}.tar.bz2"; \
    curl -fsSLO "https://www.netfilter.org/pub/nftables/${NFTABLES_TARBALL}"; \
    curl -fsSLO "https://www.netfilter.org/pub/nftables/${NFTABLES_TARBALL}.sig"; \
    curl -fsSLO "https://ftp.netfilter.org/pub/libnftnl/${LIBNFTNL_TARBALL}"; \
    curl -fsSLO "https://ftp.netfilter.org/pub/libnftnl/${LIBNFTNL_TARBALL}.sig"; \
    curl -fsSLO "https://www.netfilter.org/pub/libmnl/${LIBMNL_TARBALL}"; \
    curl -fsSLO "https://www.netfilter.org/pub/libmnl/${LIBMNL_TARBALL}.sig"; \
    echo "${NFTABLES_SHA256}  ${NFTABLES_TARBALL}" | sha256sum -c -; \
    echo "${LIBNFTNL_SHA256}  ${LIBNFTNL_TARBALL}" | sha256sum -c -; \
    echo "${LIBMNL_SHA256}  ${LIBMNL_TARBALL}" | sha256sum -c -; \
    export GNUPGHOME="$(mktemp -d)"; \
    curl -fsSL "${NETFILTER_GPG_KEY_URL}" -o netfilter-coreteam-current.asc; \
    curl -fsSL "${LIBMNL_GPG_KEY_URL}" -o netfilter-coreteam-libmnl.asc; \
    gpg --batch --import netfilter-coreteam-current.asc; \
    gpg --batch --import netfilter-coreteam-libmnl.asc; \
    gpg --batch --with-colons --fingerprint | grep -q "fpr:::::::::${NETFILTER_GPG_FPR}:"; \
    gpg --batch --with-colons --fingerprint | grep -q "fpr:::::::::${LIBMNL_GPG_FPR}:"; \
    gpg --batch --status-fd 1 --verify "${NFTABLES_TARBALL}.sig" "${NFTABLES_TARBALL}" 2>/dev/null | grep -q "^\\[GNUPG:\\] VALIDSIG ${NETFILTER_GPG_FPR} "; \
    gpg --batch --status-fd 1 --verify "${LIBNFTNL_TARBALL}.sig" "${LIBNFTNL_TARBALL}" 2>/dev/null | grep -q "^\\[GNUPG:\\] VALIDSIG ${NETFILTER_GPG_FPR} "; \
    gpg --batch --status-fd 1 --verify "${LIBMNL_TARBALL}.sig" "${LIBMNL_TARBALL}" 2>/dev/null | grep -q "^\\[GNUPG:\\] VALIDSIG ${LIBMNL_GPG_FPR} "; \
    rm -rf "${GNUPGHOME}" netfilter-coreteam-current.asc netfilter-coreteam-libmnl.asc

RUN set -eux; \
    tar -xf "libmnl-${LIBMNL_VERSION}.tar.bz2"; \
    cd "libmnl-${LIBMNL_VERSION}"; \
    ./configure \
      --prefix=/usr/local \
      --enable-static \
      --disable-shared \
      --disable-dependency-tracking; \
    make -j"$(getconf _NPROCESSORS_ONLN)"; \
    make install

RUN set -eux; \
    tar -xf "libnftnl-${LIBNFTNL_VERSION}.tar.xz"; \
    cd "libnftnl-${LIBNFTNL_VERSION}"; \
    PKG_CONFIG_PATH=/usr/local/lib/pkgconfig \
    ./configure \
      --prefix=/usr/local \
      --enable-static \
      --disable-shared \
      --disable-dependency-tracking; \
    make -j"$(getconf _NPROCESSORS_ONLN)"; \
    make install

RUN set -eux; \
    tar -xf "nftables-${NFTABLES_VERSION}.tar.xz"; \
    cd "nftables-${NFTABLES_VERSION}"; \
    PKG_CONFIG_PATH=/usr/local/lib/pkgconfig \
    CONFIG_SHELL=/bin/bash \
    ./configure \
      --prefix=/usr/local \
      --sbindir=/opt/vaka/sbin \
      --enable-static \
      --disable-shared \
      --with-mini-gmp \
      --without-cli \
      --without-json \
      --without-xtables \
      --disable-man-doc \
      --disable-dependency-tracking; \
    make -j"$(getconf _NPROCESSORS_ONLN)" LDFLAGS="-all-static" src/nft; \
    install -D -m 0755 src/nft /opt/nftables/bin/nft; \
    mkdir -p /opt/nftables; \
    printf '%s\n' \
      "NFTABLES_VERSION=${NFTABLES_VERSION}" \
      "NFTABLES_SHA256=${NFTABLES_SHA256}" \
      "LIBNFTNL_VERSION=${LIBNFTNL_VERSION}" \
      "LIBNFTNL_SHA256=${LIBNFTNL_SHA256}" \
      "LIBMNL_VERSION=${LIBMNL_VERSION}" \
      "LIBMNL_SHA256=${LIBMNL_SHA256}" \
      "NETFILTER_GPG_FPR=${NETFILTER_GPG_FPR}" \
      "LIBMNL_GPG_FPR=${LIBMNL_GPG_FPR}" \
      > /opt/nftables/BUILDINFO

FROM scratch AS artifacts
COPY --from=builder /opt/nftables /opt/nftables
