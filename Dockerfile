# syntax=docker/dockerfile:1

ARG GO_VERSION=1.19.13
ARG MOSDNS_VERSION=v4.5.3
ARG ALPINE_VERSION=3.24.2

FROM golang:${GO_VERSION}-bullseye AS mosdns-builder
ARG MOSDNS_VERSION
WORKDIR /src
RUN git clone --depth 1 --branch "${MOSDNS_VERSION}" https://github.com/IrineSistiana/mosdns.git .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w -buildid=' -o /out/mosdns .

FROM golang:${GO_VERSION}-bullseye AS gateway-builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
ARG GATEWAY_VERSION=0.1.0
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -buildid= -X main.version=${GATEWAY_VERSION}" \
    -o /out/doh-gateway .

FROM alpine:${ALPINE_VERSION}

ARG BUILD_DATE=unknown
ARG MOSDNS_VERSION=v4.5.3
ARG GATEWAY_VERSION=0.1.0

RUN apk add --no-cache ca-certificates \
    && addgroup -S app \
    && adduser -S -D -H -s /sbin/nologin -G app app \
    && mkdir -p /etc/mosdns \
    && chown -R app:app /etc/mosdns

COPY --from=mosdns-builder /out/mosdns /usr/local/bin/mosdns
COPY --from=gateway-builder /out/doh-gateway /usr/local/bin/doh-gateway
COPY --chown=app:app mosdns.yaml /etc/mosdns/config.yaml
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/mosdns /usr/local/bin/doh-gateway /usr/local/bin/entrypoint.sh

ENV PORT=8080 \
    MOSDNS_CONFIG=/etc/mosdns/config.yaml \
    MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query \
    RATE_LIMIT=100 \
    RATE_WINDOW=60s \
    MAX_CONCURRENT_REQUESTS=64 \
    UPSTREAM_TIMEOUT=8s \
    CLIENT_IP_HEADER=X-Forwarded-For \
    GOMAXPROCS=1 \
    GOGC=100 \
    GOMEMLIMIT=128MiB \
    TZ=UTC

EXPOSE 8080
USER app

LABEL org.opencontainers.image.title="MosDNS 4.5.3 + DoH Gateway" \
      org.opencontainers.image.description="Koyeb-optimized DoH gateway with MosDNS v4.5.3 and HaGeZi upstream failover" \
      org.opencontainers.image.version="${GATEWAY_VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.vendor="custom" \
      org.opencontainers.image.base.name="alpine:${ALPINE_VERSION}" \
      org.opencontainers.image.mosdns.version="${MOSDNS_VERSION}"

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
