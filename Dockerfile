# syntax=docker/dockerfile:1

# Base images are written out literally in each FROM line (tag + multi-arch
# index digest) so the whole build is reproducible AND Dependabot can read
# them: it does not expand ARG values in FROM lines (dependabot-core #2057),
# so ARG-templated images were never updated. .github/dependabot.yml only
# accepts digest/patch updates for golang, which keeps the MosDNS builder on
# Go 1.19 and leaves gateway Go minor bumps to a manual change that also
# updates go.mod and ci.yml (check-config.sh enforces that they agree).
ARG MOSDNS_VERSION=v4.5.3
# Supply-chain pin (mandatory): the full commit SHA the MOSDNS_VERSION tag must
# resolve to; the build fails if the tag ever moves. Re-resolve with:
#   git ls-remote https://github.com/IrineSistiana/mosdns.git 'refs/tags/v4.5.3^{}'
ARG MOSDNS_COMMIT=760a660192fdf996463b024d1d0e19bd66b6ed31

# Builders run on the build host and cross-compile for TARGETARCH, so
# multi-arch builds (docker buildx --platform ...) do not run Go under QEMU.
# MosDNS v4 pulls in quic-go v0.30, which refuses to build on Go >= 1.20, so it
# keeps its own pinned Go 1.19 (an accepted risk: Go 1.19 is out of upstream
# support; the long-term fix is MosDNS v5).
FROM --platform=$BUILDPLATFORM golang:1.19.13-bookworm@sha256:da9da58d86d106a5dda2ce249b00cf3b31cdd626ea41597e476de7b4eebad8c4 AS mosdns-builder
ARG MOSDNS_VERSION
ARG MOSDNS_COMMIT
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src
RUN git clone --depth 1 --branch "${MOSDNS_VERSION}" https://github.com/IrineSistiana/mosdns.git . \
    && if [ -z "${MOSDNS_COMMIT}" ]; then \
         echo "MOSDNS_COMMIT must be set to the commit SHA of ${MOSDNS_VERSION} (see the ARG comment above)" >&2; \
         exit 1; \
       fi \
    && if [ "$(git rev-parse HEAD)" != "${MOSDNS_COMMIT}" ]; then \
         echo "MosDNS ${MOSDNS_VERSION} resolved to $(git rev-parse HEAD), expected ${MOSDNS_COMMIT}" >&2; \
         exit 1; \
       fi
# GOARM is only meaningful (and only set) for 32-bit arm, e.g. linux/arm/v7.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "${TARGETARCH}" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi \
    && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /out/mosdns .

# The gateway is our own code and internet-facing, so it builds with a
# currently supported Go release (same major.minor as go.mod and ci.yml).
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS gateway-builder
# Optional override; by default the version comes from the VERSION file, so it
# has a single source of truth and plain `docker build` (e.g. Koyeb) gets it.
ARG GATEWAY_VERSION=
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
WORKDIR /src
COPY go.mod main.go VERSION ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    if [ "${TARGETARCH}" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi \
    && version="${GATEWAY_VERSION:-$(cat VERSION)}" \
    && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=${version}" \
    -o /out/doh-gateway .

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

RUN apk add --no-cache ca-certificates tini \
    && addgroup -S app \
    && adduser -S -D -H -s /sbin/nologin -G app app \
    && mkdir -p /etc/mosdns

COPY --from=mosdns-builder --chmod=0755 /out/mosdns /usr/local/bin/mosdns
COPY --from=gateway-builder --chmod=0755 /out/doh-gateway /usr/local/bin/doh-gateway
# Root-owned and read-only: the runtime user has no reason to change the
# config, and this config makes MosDNS write nothing to its working directory.
COPY --chmod=0444 mosdns.yaml /etc/mosdns/config.yaml
COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh
# Licenses shipped in the final image: this repository's LICENSE and the license
# of the MosDNS source the bundled binary is built from.
COPY --chmod=0644 LICENSE /usr/share/licenses/doh-gateway/LICENSE
COPY --from=mosdns-builder --chmod=0644 /src/LICENSE /usr/share/licenses/mosdns/LICENSE

# Only values that differ from the built-in defaults (main.go and entrypoint.sh)
# or that couple the two processes are set here. MOSDNS_CONFIG (the entrypoint
# default), MOSDNS_DOH_URL and UPSTREAM_TIMEOUT (gateway defaults) repeat
# built-in defaults on purpose: they make the coupling
# explicit, and `make check-config` verifies that MOSDNS_DOH_URL matches the
# listener in mosdns.yaml and that UPSTREAM_TIMEOUT stays above the MosDNS
# server timeout. Every other gateway setting
# (RATE_LIMIT, RATE_WINDOW, MAX_*, QUEUE_WAIT, ...) uses the built-in default.
ENV MOSDNS_CONFIG=/etc/mosdns/config.yaml \
    MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query \
    GOMAXPROCS=1 \
    GOMEMLIMIT=160MiB \
    UPSTREAM_TIMEOUT=2.5s \
    CLIENT_IP_HEADER=X-Forwarded-For

EXPOSE 8080
USER app

# Global ARGs (declared before the first FROM) are only visible inside a stage
# once re-declared there. They are declared after the RUN layers so a changing
# BUILD_DATE does not bust the layer cache.
ARG BUILD_DATE=unknown
ARG MOSDNS_VERSION
ARG GATEWAY_VERSION=

LABEL org.opencontainers.image.title="MosDNS ${MOSDNS_VERSION} + DoH Gateway" \
      org.opencontainers.image.description="Koyeb-optimized DoH gateway with MosDNS ${MOSDNS_VERSION} and HaGeZi upstream failover" \
      org.opencontainers.image.version="${GATEWAY_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.vendor="custom" \
      org.opencontainers.image.base.name="docker.io/library/alpine" \
      io.github.ant0ny54.mosdns.version="${MOSDNS_VERSION}"

# tini runs as PID 1: it forwards signals to the entrypoint and reaps any
# process reparented to PID 1. The entrypoint polls its two children and
# reaps them itself.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
