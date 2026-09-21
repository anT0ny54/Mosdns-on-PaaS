# MosDNS v4.5.3, tuned for a tiny Koyeb Web Service.
# Runtime target: 512 MiB RAM / 0.25 vCPU / 2 GiB SSD.
#
# Build stage MUST stay on Go 1.19.x: v4.5.3 transitively depends on
# github.com/lucas-clemente/quic-go v0.30.0 (pulled in by the built-in
# `forward` plugin even though this deployment config does not use the
# built-in `forward` or `fast_forward` executables for the query path), and
# that quic-go version has a deliberate compile-time guard that refuses to
# build on Go 1.20+ ("can't be built on Go 1.20 yet"). This is a hard
# upstream constraint, not a stale pin -- do not bump past golang:1.19 here
# without also replacing/vendoring quic-go. The runtime stage below has no
# Go toolchain and can be kept current independently.
FROM --platform=linux/amd64 golang:1.19.13-alpine3.18 AS build

WORKDIR /src
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64

RUN apk add --no-cache ca-certificates git \
    && git clone --depth 1 --branch v4.5.3 https://github.com/IrineSistiana/mosdns.git . \
    && test "$(git rev-parse HEAD)" = "760a660192fdf996463b024d1d0e19bd66b6ed31"

# Cache dependency download separately from the custom plugin layers so source
# edits do not force a full module re-download during image builds.
RUN go mod download

# Keep the warm-start disk cache feature without maintaining a fork.
COPY content/warm_backend.go /src/plugin/executable/cache/warm_backend.go
RUN awk '1; /WhenHit[[:space:]]*string[[:space:]]*`yaml:"when_hit"`/ {print "\tDumpFile          string `yaml:\"dump_file\"`"; print "\tDumpInterval      int    `yaml:\"dump_interval\"`"}' /src/plugin/executable/cache/cache.go > /src/plugin/executable/cache/cache.go.tmp \
 && mv /src/plugin/executable/cache/cache.go.tmp /src/plugin/executable/cache/cache.go \
 && test "$(grep -Ec '^[[:space:]]*DumpFile[[:space:]]+string[[:space:]]+`yaml:"dump_file"`[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && test "$(grep -Ec '^[[:space:]]*DumpInterval[[:space:]]+int[[:space:]]+`yaml:"dump_interval"`[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && test "$(grep -Ec '^[[:space:]]*c = mem_cache.NewMemCache\(args.Size, 0\)[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && sed -i 's|c = mem_cache.NewMemCache(args.Size, 0)|c = newWarmBackend(mem_cache.NewMemCache(args.Size, 0), args.DumpFile, args.DumpInterval, args.Size, bp.L())|' /src/plugin/executable/cache/cache.go \
 && test "$(grep -Ec '^[[:space:]]*c = newWarmBackend\(mem_cache.NewMemCache\(args.Size, 0\), args.DumpFile, args.DumpInterval, args.Size, bp.L\(\)\)[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && gofmt -w /src/plugin/executable/cache/cache.go /src/plugin/executable/cache/warm_backend.go

# The probe helper and the DoH proxy are separate `package main` programs, so
# each gets its own directory instead of sharing /src/probe.
COPY content/upstream_probe.go /src/probe/main.go
COPY content/ip_conn_proxy.go /src/proxy/main.go
COPY content/ip_conn_proxy_test.go /src/proxy/main_test.go
COPY content/sequential_forward.go /src/plugin/executable/fast_forward/sequential_forward.go

RUN go test ./proxy \
 && go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/mosdns . \
 && go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/mosdns-probe ./probe \
 && go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/ip-conn-proxy ./proxy \
 && /out/mosdns version

FROM --platform=linux/amd64 alpine:3.24.2

RUN apk add --no-cache ca-certificates \
    && addgroup -S mosdns \
    && adduser -S -G mosdns mosdns

WORKDIR /etc/mosdns
COPY --from=build /out/mosdns /usr/local/bin/mosdns
COPY --from=build /out/mosdns-probe /usr/local/bin/mosdns-probe
COPY --from=build /out/ip-conn-proxy /usr/local/bin/ip-conn-proxy
COPY content/config.yaml ./config.yaml
COPY content/entrypoint.sh ./entrypoint.sh
COPY VERSION ./VERSION

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
    HEALTH_RATE_LIMIT=2 \
    HEALTH_RATE_BURST=4 \
    GLOBAL_HEALTH_RATE_LIMIT=10 \
    GLOBAL_HEALTH_RATE_BURST=20 \
    GLOBAL_CONN_LIMIT=128 \
    DOH_MAX_BODY_BYTES=4096 \
    DOH_IDLE_TIMEOUT=120 \
    UPSTREAM_IDLE_TIMEOUT=30 \
    UPSTREAM_MAX_CONNS=2 \
    CACHE_SIZE=2048 \
    CACHE_DUMP_INTERVAL=3300 \
    HEALTH_TIMEOUT_MS=1200 \
    HEALTH_BACKEND_TIMEOUT_MS=1000 \
    HEALTH_CHECK=true \
    HEALTH_EWMA_ALPHA=0.35 \
    HEALTH_FAILURE_PENALTY_MS=1500 \
    HEALTH_SWITCH_MARGIN_PCT=0.20 \
    HEALTH_SWITCH_MARGIN_MS=25 \
    HEALTH_STATE_FILE=/tmp/mosdns-upstream-state.tsv \
    HEALTH_INTERVAL=300 \
    HEALTH_FAILS_TO_SWITCH=2 \
    HEALTH_RESTART_COOLDOWN=900 \
    GOMEMLIMIT=256MiB \
    GOMAXPROCS=1 \
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
