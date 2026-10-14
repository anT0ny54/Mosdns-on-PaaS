# syntax=docker/dockerfile:1

# MosDNS v4 pulls in quic-go v0.30, which refuses to build on Go >= 1.20, so it
# keeps its own pinned Go (an accepted risk: Go 1.19 is out of upstream
# support; the long-term fix is MosDNS v5). The gateway is our own code and
# internet-facing, so it builds with a currently supported Go release.
ARG MOSDNS_GO_VERSION=1.19.13
ARG GATEWAY_GO_VERSION=1.27
ARG MOSDNS_VERSION=v4.5.3
# Supply-chain pin (mandatory): the full commit SHA the MOSDNS_VERSION tag must
# resolve to; the build fails if the tag ever moves. Re-resolve with:
#   git ls-remote https://github.com/IrineSistiana/mosdns.git 'refs/tags/v4.5.3^{}'
ARG MOSDNS_COMMIT=760a660192fdf996463b024d1d0e19bd66b6ed31
ARG ALPINE_VERSION=3.24.2
# Base images are pinned by digest as well (multi-arch index digests), so the
# whole build is reproducible; Dependabot (.github/dependabot.yml) proposes
# updates. Changing a *_VERSION above requires updating its digest too.
ARG MOSDNS_GO_DIGEST=sha256:da9da58d86d106a5dda2ce249b00cf3b31cdd626ea41597e476de7b4eebad8c4
ARG GATEWAY_GO_DIGEST=sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61
ARG ALPINE_DIGEST=sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# Builders run on the build host and cross-compile for TARGETARCH, so
# multi-arch builds (docker buildx --platform ...) do not run Go under QEMU.
FROM --platform=$BUILDPLATFORM golang:${MOSDNS_GO_VERSION}-bookworm@${MOSDNS_GO_DIGEST} AS mosdns-builder
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

FROM --platform=$BUILDPLATFORM golang:${GATEWAY_GO_VERSION}-bookworm@${GATEWAY_GO_DIGEST} AS gateway-builder
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

FROM alpine:${ALPINE_VERSION}@${ALPINE_DIGEST}

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

# Only values that differ from the gateway's built-in defaults (see main.go) or
# that couple the two processes are set here. MOSDNS_DOH_URL must match the
# listener in mosdns.yaml, and UPSTREAM_TIMEOUT must stay above the MosDNS server
# timeout; `make check-config` verifies both. Every other gateway setting
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
ARG ALPINE_VERSION
ARG BUILD_DATE=unknown
ARG MOSDNS_VERSION
ARG GATEWAY_VERSION=

LABEL org.opencontainers.image.title="MosDNS ${MOSDNS_VERSION} + DoH Gateway" \
      org.opencontainers.image.description="Koyeb-optimized DoH gateway with MosDNS ${MOSDNS_VERSION} and HaGeZi upstream failover" \
      org.opencontainers.image.version="${GATEWAY_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.vendor="custom" \
      org.opencontainers.image.base.name="alpine:${ALPINE_VERSION}" \
      io.github.ant0ny54.mosdns.version="${MOSDNS_VERSION}"

# tini runs as PID 1: it forwards signals to the entrypoint and reaps any
# process reparented to PID 1. The entrypoint polls its two children and
# reaps them itself.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/entrypoint.sh"]
