# Custom MosDNS v4.5.3 build with a small disk-assisted warm-cache extension.
# The upstream source is cloned at the v4.5.3 tag, then only the cache plugin
# is extended with optional dump_file/dump_interval support.
FROM golang:1.20-alpine AS build

RUN apk add --no-cache git ca-certificates
WORKDIR /src

RUN git clone --depth 1 --branch v4.5.3 https://github.com/IrineSistiana/mosdns.git .

COPY content/warm_backend.go /src/plugin/executable/cache/warm_backend.go

# Add the two custom cache arguments and wrap the stock in-memory backend.
RUN sed -i '/WhenHit string `yaml:"when_hit"`/a\	DumpFile          string `yaml:"dump_file"`\
\tDumpInterval      int    `yaml:"dump_interval"`' /src/plugin/executable/cache/cache.go \
 && sed -i 's/c = mem_cache.NewMemCache(args.Size, 0)/c = newWarmBackend(mem_cache.NewMemCache(args.Size, 0), args.DumpFile, args.DumpInterval, bp.L())/' /src/plugin/executable/cache/cache.go \
 && gofmt -w /src/plugin/executable/cache/cache.go /src/plugin/executable/cache/warm_backend.go \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/mosdns . \
 && /out/mosdns version

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /etc/mosdns
COPY --from=build /out/mosdns /usr/bin/mosdns
COPY content/config.yaml ./config.yaml
COPY content/entrypoint.sh ./entrypoint.sh
RUN chmod 0755 ./entrypoint.sh && mkdir -p /var/cache/mosdns
ENV PORT=8080
ENV DOH_PATH=/dns-query
EXPOSE 8080
ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
