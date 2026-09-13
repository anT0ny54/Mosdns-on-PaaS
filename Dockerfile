# MosDNS v4.5.3, tuned for a tiny Koyeb Web Service.
# Runtime target: 512 MiB RAM / 0.1 vCPU / 2 GiB SSD.
FROM --platform=linux/amd64 golang:1.24-alpine3.22 AS build

WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64

RUN apk add --no-cache ca-certificates git perl \
    && git clone --depth 1 --branch v4.5.3 https://github.com/IrineSistiana/mosdns.git .

# Keep the warm-start disk cache feature without maintaining a fork.
COPY content/warm_backend.go /src/plugin/executable/cache/warm_backend.go
RUN perl -0pi -e 's/(WhenHit\s+string\s+`yaml:"when_hit"`)/$1\n\tDumpFile          string `yaml:"dump_file"`\n\tDumpInterval      int    `yaml:"dump_interval"`/' /src/plugin/executable/cache/cache.go \
 && perl -0pi -e 's/c = mem_cache\.NewMemCache\(args\.Size, 0\)/c = newWarmBackend(mem_cache.NewMemCache(args.Size, 0), args.DumpFile, args.DumpInterval, args.Size, bp.L())/' /src/plugin/executable/cache/cache.go \
 && gofmt -w /src/plugin/executable/cache/cache.go /src/plugin/executable/cache/warm_backend.go

COPY content/upstream_probe.go /src/probe/main.go
COPY content/ip_conn_proxy.go /src/probe/ip_conn_proxy.go

RUN go build -trimpath -ldflags='-s -w' -o /out/mosdns . \
 && go build -trimpath -ldflags='-s -w' -o /out/mosdns-probe ./probe/main.go \
 && go build -trimpath -ldflags='-s -w' -o /out/ip-conn-proxy ./probe/ip_conn_proxy.go \
 && /out/mosdns version

FROM --platform=linux/amd64 alpine:3.22

RUN apk add --no-cache ca-certificates \
    && addgroup -S mosdns \
    && adduser -S -G mosdns mosdns

WORKDIR /etc/mosdns
COPY --from=build /out/mosdns /usr/local/bin/mosdns
COPY --from=build /out/mosdns-probe /usr/local/bin/mosdns-probe
COPY --from=build /out/ip-conn-proxy /usr/local/bin/ip-conn-proxy
COPY content/config.yaml ./config.yaml
COPY content/entrypoint.sh ./entrypoint.sh

RUN chmod 0755 ./entrypoint.sh \
    && mkdir -p /var/cache/mosdns \
    && chown -R mosdns:mosdns /etc/mosdns /var/cache/mosdns

ENV PORT=8080 \
    MOSDNS_BACKEND_PORT=18080 \
    IP_CONN_LIMIT=0 \
    DOH_RATE_LIMIT=5 \
    DOH_RATE_BURST=12 \
    DOH_RATE_MAX_IPS=512 \
    GLOBAL_RATE_LIMIT=40 \
    GLOBAL_RATE_BURST=80 \
    GLOBAL_CONN_LIMIT=128 \
    DOH_MAX_BODY_BYTES=4096 \
    DOH_IDLE_TIMEOUT=120 \
    UPSTREAM_IDLE_TIMEOUT=30 \
    UPSTREAM_MAX_CONNS=2 \
    CACHE_SIZE=2048 \
    CACHE_DUMP_INTERVAL=3300 \
    MAX_QPS=15 \
    HEALTH_TIMEOUT_MS=1200 \
    HEALTH_CHECK=true \
    HEALTH_EWMA_ALPHA=0.35 \
    HEALTH_FAILURE_PENALTY_MS=1500 \
    HEALTH_SWITCH_MARGIN_PCT=0.20 \
    HEALTH_SWITCH_MARGIN_MS=25 \
    HEALTH_STATE_FILE=/tmp/mosdns-upstream-state.tsv \
    HEALTH_INTERVAL=300 \
    HEALTH_FAILS_TO_SWITCH=2 \
    HEALTH_RESTART_COOLDOWN=900 \
    GOMEMLIMIT=320MiB \
    GOMAXPROCS=1 \
    UPSTREAM_MODE=doh-only \
    HAGEZI_UPSTREAM=rotate \
    HEALTH_PATH=/health \
    DOH_PATH=/dns-query \
    UPSTREAM_0_IP=188.34.161.210 \
    UPSTREAM_1_IP=159.69.155.94 \
    UPSTREAM_2_IP=95.217.163.17

EXPOSE 8080
STOPSIGNAL SIGTERM
USER mosdns
ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
