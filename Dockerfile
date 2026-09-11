# v7.5 stable build: placeholder-safe runtime config generation
# MosDNS v4.5.3 stable Koyeb build. Foreground MosDNS; Koyeb-managed lifecycle; no in-container restart loop.
FROM --platform=linux/amd64 golang:1.19-alpine3.17 AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates perl
RUN git clone --depth 1 --branch v4.5.3 https://github.com/IrineSistiana/mosdns.git .
COPY content/warm_backend.go /src/plugin/executable/cache/warm_backend.go
RUN mkdir -p /src/probe
COPY content/upstream_probe.go /src/probe/main.go
COPY content/ip_conn_proxy.go /src/probe/ip_conn_proxy.go
RUN perl -0pi -e 's/(WhenHit\s+string\s+`yaml:"when_hit"`)/$1\n\tDumpFile          string `yaml:"dump_file"`\n\tDumpInterval      int    `yaml:"dump_interval"`/' /src/plugin/executable/cache/cache.go \
 && perl -0pi -e 's/c = mem_cache\.NewMemCache\(args\.Size, 0\)/c = newWarmBackend(mem_cache.NewMemCache(args.Size, 0), args.DumpFile, args.DumpInterval, args.Size, bp.L())/' /src/plugin/executable/cache/cache.go \
 && gofmt -w /src/plugin/executable/cache/cache.go /src/plugin/executable/cache/warm_backend.go \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/mosdns . \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/mosdns-probe ./probe/main.go \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/ip-conn-proxy ./probe/ip_conn_proxy.go \
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
ENV PORT=8080
ENV MOSDNS_BACKEND_PORT=18080
ENV IP_CONN_LIMIT=4
ENV HEALTH_PATH=/health
ENV DOH_PATH=/dns-query
ENV GOMEMLIMIT=384MiB
EXPOSE 8080
USER mosdns
ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
