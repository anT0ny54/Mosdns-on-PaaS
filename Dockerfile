# MosDNS v4.5.3, tuned for a tiny PaaS Web Service.
# Runtime target: 512 MiB RAM / 0.1 vCPU / 2 GiB SSD.
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

# MosDNS v4.5.3's DoH client internally replaces the caller context with a
# fresh 5-second background context. That makes our sequential failover timeout
# unable to cancel a stalled request, leaving orphaned HTTP exchanges behind and
# exhausting the upstream connection pools. Patch only that context boundary:
# keep the upstream's 5s hard cap, but make it inherit the caller's cancellation
# and earlier deadline. The exact source shape is checked so a future MosDNS
# change cannot silently invalidate the patch.
RUN test "$(grep -Fc 'ctx, cancel := context.WithTimeout(context.Background(), defaultDoHTimeout)' /src/pkg/upstream/doh/upstream.go)" -eq 1 \
 && test "$(grep -Fc 'r, err := u.exchange(ctx, utils.BytesToStringUnsafe(urlBuf))' /src/pkg/upstream/doh/upstream.go)" -eq 1 \
 && test "$(grep -Fc 'We overwrite the ctx with a fixed timout context here.' /src/pkg/upstream/doh/upstream.go)" -eq 1 \
 && awk ' \
      index($0, "ctx, cancel := context.WithTimeout(context.Background(), defaultDoHTimeout)") > 0 { \
        print "\t\t// Keep a hard timeout while inheriting the caller cancellation."; \
        print "\t\texchangeCtx := ctx"; \
        print "\t\tcancel := func() {}"; \
        print "\t\tif deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > defaultDoHTimeout {"; \
        print "\t\t\texchangeCtx, cancel = context.WithTimeout(ctx, defaultDoHTimeout)"; \
        print "\t\t}"; \
        print "\t\tdefer cancel()"; \
        skip = 1; \
        next; \
      } \
      skip { skip = 0; next } \
      { print }' /src/pkg/upstream/doh/upstream.go > /src/pkg/upstream/doh/upstream.go.tmp \
 && mv /src/pkg/upstream/doh/upstream.go.tmp /src/pkg/upstream/doh/upstream.go \
 && sed -i 's|r, err := u.exchange(ctx, utils.BytesToStringUnsafe(urlBuf))|r, err := u.exchange(exchangeCtx, utils.BytesToStringUnsafe(urlBuf))|' /src/pkg/upstream/doh/upstream.go \
 && test "$(grep -Fc 'exchangeCtx := ctx' /src/pkg/upstream/doh/upstream.go)" -eq 1 \
 && test "$(grep -Fc 'exchangeCtx, cancel = context.WithTimeout(ctx, defaultDoHTimeout)' /src/pkg/upstream/doh/upstream.go)" -eq 1 \
 && test "$(grep -Fc 'r, err := u.exchange(exchangeCtx, utils.BytesToStringUnsafe(urlBuf))' /src/pkg/upstream/doh/upstream.go)" -eq 1 \
 && gofmt -w /src/pkg/upstream/doh/upstream.go

# Keep the warm-start disk cache feature without maintaining a fork.
COPY content/warm_backend.go /src/plugin/executable/cache/warm_backend.go
COPY content/warm_backend_test.go /src/plugin/executable/cache/warm_backend_test.go
RUN test "$(grep -Fc 'CompressResp      bool   `yaml:"compress_resp"`' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && awk ' \
      /CompressResp[[:space:]]+bool[[:space:]]+`yaml:"compress_resp"`/ { \
        print; \
        print "\tMaxEntryBytes    int    `yaml:\"max_entry_bytes\"`"; \
        print "\tDumpFile         string `yaml:\"dump_file\"`"; \
        print "\tDumpInterval     int    `yaml:\"dump_interval\"`"; \
        next; \
      } \
      { print }' /src/plugin/executable/cache/cache.go > /src/plugin/executable/cache/cache.go.tmp \
 && mv /src/plugin/executable/cache/cache.go.tmp /src/plugin/executable/cache/cache.go \
 && test "$(grep -Ec '^[[:space:]]*MaxEntryBytes[[:space:]]+int[[:space:]]+`yaml:"max_entry_bytes"`[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && test "$(grep -Ec '^[[:space:]]*DumpFile[[:space:]]+string[[:space:]]+`yaml:"dump_file"`[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && test "$(grep -Ec '^[[:space:]]*DumpInterval[[:space:]]+int[[:space:]]+`yaml:"dump_interval"`[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && test "$(grep -Fc 'v, err := r.Pack()' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && awk ' \
      /v, err := r.Pack\(\)/ { \
        print; \
        after_pack = 1; \
        next; \
      } \
      after_pack && /^[[:space:]]*if err != nil[[:space:]]*\{/ { \
        print; \
        in_pack_error = 1; \
        next; \
      } \
      in_pack_error && /^[[:space:]]*\}/ { \
        print; \
        print "\tif c.args.MaxEntryBytes > 0 && len(v) > c.args.MaxEntryBytes {"; \
        print "\t\treturn nil"; \
        print "\t}"; \
        in_pack_error = 0; \
        after_pack = 0; \
        next; \
      } \
      { print }' /src/plugin/executable/cache/cache.go > /src/plugin/executable/cache/cache.go.tmp \
 && mv /src/plugin/executable/cache/cache.go.tmp /src/plugin/executable/cache/cache.go \
 && test "$(grep -Fc 'if c.args.MaxEntryBytes > 0 && len(v) > c.args.MaxEntryBytes {' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && awk ' \
      /v, err := r.Pack\(\)/ { pack = NR; next } \
      pack && err_start == 0 && /^[[:space:]]*if err != nil[[:space:]]*\{/ { err_start = NR; next } \
      err_start > 0 && err_end == 0 && /^[[:space:]]*\}/ { err_end = NR; next } \
      err_end > 0 && limit_line == 0 && /if c.args.MaxEntryBytes > 0 && len\(v\) > c.args.MaxEntryBytes/ { limit_line = NR } \
      END { exit !(pack > 0 && err_start > pack && err_end > err_start && limit_line > err_end) }' /src/plugin/executable/cache/cache.go \
 && test "$(grep -Ec '^[[:space:]]*c = mem_cache.NewMemCache\(args.Size, 0\)[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && sed -i 's|c = mem_cache.NewMemCache(args.Size, 0)|c = newWarmBackend(mem_cache.NewMemCache(args.Size, 0), args.DumpFile, args.DumpInterval, args.Size, args.MaxEntryBytes, bp.L())|' /src/plugin/executable/cache/cache.go \
 && test "$(grep -Ec '^[[:space:]]*c = newWarmBackend\(mem_cache.NewMemCache\(args.Size, 0\), args.DumpFile, args.DumpInterval, args.Size, args.MaxEntryBytes, bp.L\(\)\)[[:space:]]*$' /src/plugin/executable/cache/cache.go)" -eq 1 \
 && gofmt -w /src/plugin/executable/cache/cache.go /src/plugin/executable/cache/warm_backend.go

# The probe helper and the DoH proxy are separate `package main` programs, so
# each gets its own directory instead of sharing /src/probe.
COPY content/upstream_probe.go /src/probe/main.go
COPY content/ip_conn_proxy.go /src/proxy/main.go
COPY content/doh_guard.go /src/proxy/guard.go
COPY content/ip_conn_proxy_test.go /src/proxy/main_test.go
COPY content/doh_guard_test.go /src/proxy/guard_test.go
COPY content/upstream_probe_test.go /src/probe/main_test.go
COPY content/sequential_forward.go /src/plugin/executable/fast_forward/sequential_forward.go
COPY content/sequential_forward_test.go /src/plugin/executable/fast_forward/sequential_forward_test.go

RUN go test ./plugin/executable/cache ./plugin/executable/fast_forward ./probe ./proxy \
 && go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/mosdns . \
 && go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/mosdns-probe ./probe \
 && go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/ip-conn-proxy ./proxy \
 && /out/mosdns version

FROM --platform=linux/amd64 alpine:3.24

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

# Runtime defaults (ports, resource guards, cache size, memory limits, upstream pins)
# live in exactly one place: the `: "${NAME:=default}"` block at the top of
# entrypoint.sh, which validates bounded numeric values and supported text inputs.
# Override any of them with
# service environment variables; see README.md for the full table.

EXPOSE 8080
STOPSIGNAL SIGTERM
USER mosdns
ENTRYPOINT ["/etc/mosdns/entrypoint.sh"]
