# Custom MosDNS v4.5.3 build. Go 1.19 is required by v4.5.3 dependencies.
FROM --platform=linux/amd64 golang:1.19-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates perl
RUN git clone --depth 1 --branch v4.5.3 https://github.com/IrineSistiana/mosdns.git .
COPY content/warm_backend.go /src/plugin/executable/cache/warm_backend.go
RUN perl -0pi -e 's/(WhenHit\s+string\s+`yaml:"when_hit"`)/$1\n\tDumpFile          string `yaml:"dump_file"`\n\tDumpInterval      int    `yaml:"dump_interval"`/' /src/plugin/executable/cache/cache.go \
 && perl -0pi -e 's/c = mem_cache\.NewMemCache\(args\.Size, 0\)/c = newWarmBackend(mem_cache.NewMemCache(args.Size, 0), args.DumpFile, args.DumpInterval, bp.L())/' /src/plugin/executable/cache/cache.go \
 && gofmt -w /src/plugin/executable/cache/cache.go /src/plugin/executable/cache/warm_backend.go \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/mosdns . \
 && /out/mosdns version

FROM --platform=linux/amd64 alpine:3.17
RUN apk add --no-cache ca-certificates
WORKDIR /etc/mosdns
COPY --from=build /out/mosdns /usr/local/bin/mosdns
COPY content/config.yaml ./config.yaml
COPY content/entrypoint.sh ./entrypoint.sh
RUN chmod 0755 ./entrypoint.sh
ENV PORT=8080
ENV DOH_PATH=/dns-query
EXPOSE 8080
ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
