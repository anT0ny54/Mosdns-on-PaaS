# Changelog

## 0.5.12 — 2026-10-14

Fixes from a full-project review: cache, readiness, shutdown timing, slow
uploads, and supply-chain pinning.

### Fixed
- **The cache almost never hit.** MosDNS v4.5.3 skips the cache for any query
  carrying an EDNS0 OPT record unless `cache_everything` is set, and nearly
  every DoH client sends one. With `cache_everything` alone, per-query EDNS
  padding and cookies still split the cache keys. `mosdns.yaml` now runs
  `_edns0_filter_ecs_only` before a `cache_everything: true` cache. Measured
  end to end through the gateway: repeat queries went from ~100-170 ms
  (upstream) to under 1 ms.
- **`/readyz` depended on the public upstreams.** Each probe was a unique name
  that went to HaGeZi, so an upstream outage failed the health check and the
  platform restarted a healthy container (Koyeb restarts after 3 failed
  checks). The probe is now a fixed `A ready.doh-gateway.invalid`, answered
  locally by a new `readiness` (`arbitrary`) plugin before the cache. The
  gateway requires NOERROR with at least one answer, so an NXDOMAIN (local rule
  missing) is reported as not ready. Verified: with all upstreams unreachable,
  `/readyz` stays 200 (queries return SERVFAIL after 2 s); with MosDNS down it
  returns 503.
- **Concurrent `/readyz` calls queued behind each other.** Callers now share
  one in-flight probe, and each still honours its own request context.
- **Shutdown did not fit Docker's 10 s stop timeout.** The gateway drain is now
  4 s (was 10 s), and the entrypoint graces are 6 s and 3 s (were 15 s and 5 s).
- **A TERM during crash teardown turned exit 1 into exit 0.** The supervision
  loop now ignores further signals once it is tearing down after a child
  died. New entrypoint test scenarios 6 and 7 cover this; the test fails if
  the fix is removed.
- **A slow POST body held an active slot for the whole 5 s ReadTimeout.** The
  body now has a 2 s read deadline; a slower upload gets `400` (`Connection:
  close`) and frees its slot. The deadline is cleared after the body is read,
  so it cannot cancel a slow upstream answer (covered by a test).
- **`X-Forwarded-For: unknown` was sent to MosDNS** when the client address
  could not be parsed; the header is now only sent with a valid IP.
  IPv4-mapped IPv6 clients are normalised to IPv4.
- **Duplicate `X-Content-Type-Options`** when MosDNS also set it; the gateway
  now sets it once.
- **The limiter map never shrank** after a spike of distinct clients; it is
  reallocated at window rollover when it held more than 4096 keys.
- **`ListenAndServe` errors other than `ErrServerClosed` exited 0**; any
  unexpected return now exits 1. A startup warning is logged when
  `UPSTREAM_TIMEOUT` is not above the MosDNS server timeout.

### Changed
- Gateway internals: a `newGateway(options)` constructor validates the backend
  URL once at startup (an invalid `MOSDNS_DOH_URL` now fails at start instead
  of on every request) and replaces the lazy `sync.Once`, the per-gateway
  size-limit fields and the constructor fallbacks. The pooled-request-body
  wrapper was replaced by an exact-size copy, which is simpler and about 12%
  faster in the POST benchmark. The transport no longer sets
  `ResponseHeaderTimeout` (the per-request context timeout already covers it),
  and `IdleConnTimeout` is 15 s, below MosDNS's 20 s, so the gateway never
  reuses a connection MosDNS is closing.
- The write deadline is 2 s body read + `QUEUE_WAIT` + `UPSTREAM_TIMEOUT` + 2 s,
  with an 8 s floor, which is the effective default (was 9.6 s).
- Metrics: the `*_seconds_avg` gauges are removed (compute
  `_seconds_total / _samples_total` in the scraper); new
  `doh_gateway_build_info{version="..."}`.
- Dockerfile: every base image is pinned by multi-arch digest; builder stages
  run on `$BUILDPLATFORM` and cross-compile (GOARM only for 32-bit arm); the
  version is read from `VERSION`, so `VERSION` is the single source of truth
  and plain `docker build` (Koyeb) gets it too; the MosDNS config is
  root-owned and read-only (`0444`); the label is renamed to
  `io.github.ant0ny54.mosdns.version`.
- `check-config.sh` validates the layout generically instead of fixed tag
  names. It checks that the cache and EDNS filter are set up, that the
  readiness rule matches `main.go`, and that the `MOSDNS_DOH_URL` host
  matches. Go versions are compared on major.minor, and the obsolete
  VERSION/`GATEWAY_VERSION` check is gone.
- `entrypoint.sh` `is_alive` uses only shell builtins (no `sed` fork per poll).
- `make test` adds a gofmt check and runs `go test -race`; CI drops the
  separate race job and builds the image with Buildx and the GitHub Actions
  cache. New `.github/dependabot.yml` covers Docker digests and Actions.
- `test-entrypoint.sh` now also checks, after every scenario, that neither
  fake child is still running (not only the exit code).
- Tests: shared constructors, duplicate tests merged, and new tests for every
  fix above. The benchmark is renamed `BenchmarkGatewayParallelPOST`.
  Verified: `go vet`, staticcheck (all checks) and deadcode are clean;
  `go test -race` passes, with 83.5% coverage; the 5000 RPS gate measured
  ~20k RPS; the entrypoint test passes under dash and BusyBox ash.

### Removed
- `.github/workflows/Keep-Alive.yml` and `keep-alive.txt`. They pushed a commit
  twice a month only to keep scheduled workflows alive, but the repository
  has no scheduled workflows (CI runs on push and pull request).

### Known, not changed
- MosDNS v4.5.3 still builds with Go 1.19 (quic-go v0.30 does not build on
  newer Go). The long-term fix is MosDNS v5.
- With `CLIENT_IP_HEADER` set, a request without a valid value in that header
  falls back to `X-Real-IP`; behind the trusted proxy this cannot happen.
- Not verified here: the Docker build itself (no Docker in the review
  environment). CI builds it on push.

