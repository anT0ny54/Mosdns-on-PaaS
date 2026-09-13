# Koyeb-optimized MosDNS public DoH service.
# Uses the official MosDNS runtime so upstream security/bug fixes do not
# require recompiling the DNS core in this project.
FROM --platform=linux/amd64 golang:1.25-alpine AS helper-build

WORKDIR /src
COPY content/ip_conn_proxy.go .
COPY content/upstream_probe.go .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/ip-conn-proxy ./ip_conn_proxy.go && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/mosdns-probe ./upstream_probe.go

FROM --platform=linux/amd64 irinesistiana/mosdns:v5.3.4

USER root
WORKDIR /etc/mosdns

RUN apk add --no-cache ca-certificates && \
    addgroup -S mosdns 2>/dev/null || true && \
    adduser -S -G mosdns mosdns 2>/dev/null || true && \
    mkdir -p /var/cache/mosdns && \
    chown -R mosdns:mosdns /etc/mosdns /var/cache/mosdns

COPY --from=helper-build /out/ip-conn-proxy /usr/local/bin/ip-conn-proxy
COPY --from=helper-build /out/mosdns-probe /usr/local/bin/mosdns-probe
COPY content/config.yaml /etc/mosdns/config.yaml
COPY content/entrypoint.sh /etc/mosdns/entrypoint.sh

RUN chmod 0755 /usr/local/bin/ip-conn-proxy /usr/local/bin/mosdns-probe /etc/mosdns/entrypoint.sh

ENV PORT=8080 \
    MOSDNS_BACKEND_PORT=18080 \
    DOH_PATH=/dns-query \
    HEALTH_PATH=/health \
    CACHE_SIZE=4096 \
    CACHE_DUMP_FILE=/var/cache/mosdns/cache.dump \
    CACHE_DUMP_INTERVAL=1800 \
    DOH_RATE_LIMIT=5 \
    DOH_RATE_BURST=12 \
    DOH_RATE_MAX_IPS=512 \
    GLOBAL_RATE_LIMIT=40 \
    GLOBAL_RATE_BURST=80 \
    GLOBAL_CONN_LIMIT=128 \
    DOH_MAX_BODY_BYTES=4096 \
    IP_CONN_LIMIT=0 \
    UPSTREAM_IDLE_TIMEOUT=30 \
    DOH_IDLE_TIMEOUT=90 \
    HEALTH_TIMEOUT_MS=1200 \
    HEALTH_EWMA_ALPHA=0.35 \
    HEALTH_FAILURE_PENALTY_MS=1500 \
    HEALTH_STATE_FILE=/tmp/mosdns-upstream-state.tsv \
    GOMEMLIMIT=384MiB \
    UPSTREAM_0_IP=188.34.161.210 \
    UPSTREAM_1_IP=159.69.155.94 \
    UPSTREAM_2_IP=95.217.163.17

EXPOSE 8080

USER mosdns
ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
