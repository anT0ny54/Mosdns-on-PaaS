# syntax=docker/dockerfile:1

# MosDNS v4 pulls in old dependencies that only build with an old Go toolchain,
# so it keeps its own pinned Go. The gateway is our own code and internet-facing,
# so it builds with a currently supported Go release.
ARG MOSDNS_GO_VERSION=1.19.13
ARG GATEWAY_GO_VERSION=1.27
ARG MOSDNS_VERSION=v4.5.3
# Optional supply-chain pin: set to the full commit SHA of the MOSDNS_VERSION tag
# and the build fails if the tag ever resolves to a different commit.
ARG MOSDNS_COMMIT=
ARG ALPINE_VERSION=3.24.2
ARG GATEWAY_VERSION=0.5.4

FROM golang:${MOSDNS_GO_VERSION}-bookworm AS mosdns-builder
ARG MOSDNS_VERSION
ARG MOSDNS_COMMIT
ARG TARGETARCH
WORKDIR /src
RUN git clone --depth 1 --branch "${MOSDNS_VERSION}" https://github.com/IrineSistiana/mosdns.git . \
    && if [ -n "${MOSDNS_COMMIT}" ] && [ "$(git rev-parse HEAD)" != "${MOSDNS_COMMIT}" ]; then \
         echo "MosDNS ${MOSDNS_VERSION} resolved to $(git rev-parse HEAD), expected ${MOSDNS_COMMIT}" >&2; \
         exit 1; \
       fi
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /out/mosdns .

FROM golang:${GATEWAY_GO_VERSION}-bookworm AS gateway-builder
ARG GATEWAY_VERSION
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=${GATEWAY_VERSION}" \
    -o /out/doh-gateway .

FROM alpine:${ALPINE_VERSION}

RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -D -H -s /sbin/nologin -G app app \
    && mkdir -p /etc/mosdns \
    && chown -R app:app /etc/mosdns

COPY --from=mosdns-builder --chmod=0755 /out/mosdns /usr/local/bin/mosdns
COPY --from=gateway-builder --chmod=0755 /out/doh-gateway /usr/local/bin/doh-gateway
COPY --chown=app:app --chmod=0644 mosdns.yaml /etc/mosdns/config.yaml
COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

# Only values that differ from the gateway's built-in defaults (see main.go) or
# that couple the two processes are set here. MOSDNS_DOH_URL must match the
# listener in mosdns.yaml; `make check-config` verifies that.
ENV MOSDNS_CONFIG=/etc/mosdns/config.yaml \
    MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query \
    GOMAXPROCS=1 \
    GOGC=150 \
    GOMEMLIMIT=160MiB

EXPOSE 8080
USER app

# Global ARGs (declared before the first FROM) are only visible inside a stage
# once re-declared there, so ALPINE_VERSION has to be repeated here or the
# base.name label below would silently expand to "alpine:". They are declared
# after the RUN layers so a changing BUILD_DATE does not bust the layer cache.
ARG ALPINE_VERSION
ARG BUILD_DATE=unknown
ARG MOSDNS_VERSION
ARG GATEWAY_VERSION

LABEL org.opencontainers.image.title="MosDNS ${MOSDNS_VERSION} + DoH Gateway" \
      org.opencontainers.image.description="Koyeb-optimized DoH gateway with MosDNS ${MOSDNS_VERSION} and HaGeZi upstream failover" \
      org.opencontainers.image.version="${GATEWAY_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.vendor="custom" \
      org.opencontainers.image.base.name="alpine:${ALPINE_VERSION}" \
      org.opencontainers.image.mosdns.version="${MOSDNS_VERSION}"

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
