# Mosdns-on-PaaS

MosDNS v4.5.3 + a custom Go DoH gateway, packaged as a single container for
PaaS platforms (Koyeb and similar). The gateway is the only public listener;
MosDNS serves plain-HTTP DNS-over-HTTPS-format queries on loopback behind it
(TLS is terminated by the platform, not by this image).

## Architecture

- **doh-gateway** (this repo, `main.go`): an HTTP front door that validates,
  rate-limits, queues, and forwards DoH requests to MosDNS. Runs as an
  unprivileged user (`app`), drains gracefully on SIGTERM/SIGINT (4 s). HTTP
  server limits: 5 s to read request headers, 5 s to read the whole request,
  2 s to receive a POST body (a DNS message is at most 4 KiB; a slower upload
  gets `400` and frees its slot), 20 s keep-alive idle timeout, 8 KiB of
  request headers, and a write deadline of the full request budget — 2 s body
  read + `QUEUE_WAIT` + `UPSTREAM_TIMEOUT` + a 2 s margin — but never less
  than 8 s. With the default settings that budget is 2 + 0.1 + 2.5 + 2 = 6.6 s,
  so the 8 s floor applies; raised timeouts raise the deadline with them. The
  body read is included because the write deadline starts when the request
  headers arrive, so a slow POST upload still ends in a `503`/`504` instead of
  a dropped connection. A startup warning is logged if `UPSTREAM_TIMEOUT` is
  not above the MosDNS server timeout (2 s).
- **MosDNS v4.5.3** (built from upstream in the Dockerfile): listens on
  `127.0.0.1:8081` (`/dns-query`), answers the readiness probe name locally,
  strips every EDNS0 option except ECS, answers from a cache (32768 entries, lazy
  cache 3600 s served with a 30 s reply TTL) and otherwise races two HaGeZi
  DoH upstreams, falling back to a third HaGeZi endpoint if no answer arrives
  within 400 ms.

`entrypoint.sh` starts both processes and then polls them once per second. A
child counts as gone when it has exited, was killed by a signal (OOM kill,
SIGSEGV, SIGKILL) or is a zombie (state read from `/proc`; without `/proc` it
falls back to `kill -0`). If either is gone, it stops the other and exits with
status 1. The loop does not use `wait -n`: BusyBox ash does not return from it
when a child is killed by a signal, so the container would keep running with
only one process. On `TERM`, `INT` or `HUP` it stops the gateway first (`TERM`,
then `KILL` if it is still alive after a 6 s grace period) so the gateway can
drain while MosDNS is still available, then stops MosDNS the same way (3 s
grace). Together that is 9 s, inside Docker's default 10 s stop timeout. A
signal that arrives while the entrypoint is already tearing down after a child
crash is ignored, so the exit status stays 1. Exit is polled every 0.2 s, so a child that stops quickly costs no
extra wait (this needs a `sleep` that accepts fractions; BusyBox in Alpine
does). Supervision is process-level only: it detects an *exit*, not a wedge. If
MosDNS hangs without exiting, point the platform health check at the gateway's
`/readyz` (HTTP, not just TCP). The entrypoint honours these optional
variables: `MOSDNS_CONFIG` (default `/etc/mosdns/config.yaml`), `MOSDNS_DIR`
(default `/etc/mosdns`), and `MOSDNS_BIN` / `GATEWAY_BIN` (default
`/usr/local/bin/mosdns` and `/usr/local/bin/doh-gateway`; they exist so
`test-entrypoint.sh` can substitute fake children).

## HTTP endpoints

| Path | Behavior |
|---|---|
| `GET/POST /dns-query` | DoH. POST requires `Content-Type: application/dns-message` (case-insensitive, no parameters); GET requires a raw (unpadded) base64url `dns` query parameter. Messages must be 12-4096 bytes; a backend 2xx answer shorter than 12 bytes is rejected with `502`. |
| `GET/HEAD /healthz` | Liveness. Always `200 ok` if the gateway process is up. |
| `GET/HEAD /readyz` | Readiness of the gateway **and MosDNS**, independent of the public upstreams. Sends `A ready.doh-gateway.invalid` (fixed name, incrementing transaction ID) through MosDNS, where the `readiness` plugin answers it locally before the cache, and validates the response (HTTP 2xx with `Content-Type: application/dns-message`, then transaction ID, QR bit, RCODE 0 (NOERROR), QDCOUNT 1, echoed question, at least one answer). An NXDOMAIN or empty answer means the local rule is missing and is reported as not ready. An upstream outage therefore does not fail the probe (DNS queries return SERVFAIL instead), so a platform health check on `/readyz` cannot restart-loop the container while HaGeZi is unreachable. Each probe has a 1 s budget and goes through a dedicated readiness client (single connection). The result is cached 5 s on success and 1 s on failure; concurrent callers share one in-flight probe, and each caller still honours its own request context: a disconnecting client stops waiting, and if it owned the probe, the probe is canceled, never cached, not counted in `doh_gateway_ready_probe_total{result="not_ready"}`, and no response is written or counted for that client. |
| `GET/HEAD /metrics` | Plain-text Prometheus exposition (`doh_gateway_*`): request (`/dns-query` only), rate-limit, active-reject, queue-timeout and backend-error counters; responses by status class (`2xx`/`4xx`/`5xx`, counting every response the gateway writes, including `/healthz`, `/readyz`, `/metrics` and `404`; the gateway never redirects and turns a backend 1xx/3xx into `502`, so there is no `3xx` series; a client that disconnects mid-request, on `/dns-query` or `/readyz`, is not counted, since no response is written); readiness probe results; `doh_gateway_build_info{version="..."}`; queue-wait and backend-latency totals and sample counts (averages are left to the scraper: divide `_seconds_total` by `_samples_total`; backend latency covers only valid answers: a sample is recorded after a 2xx `application/dns-message` response of 12-4096 bytes has been completely read and validated, measured from sending the request until the body was read. Timeouts, truncated, oversize, wrong-type or too-short bodies, and non-2xx answers add no sample; they are counted in `doh_gateway_backend_error_total` instead). |

`/healthz`, `/readyz` and `/metrics` are served unauthenticated on the same public listener and are not subject to the rate limiter, the active-request ceiling or the processing queue (only `/dns-query` is). `/readyz` is bounded by its result cache (one local MosDNS query per 5 s on success, per 1 s on failure) and never reaches the upstreams. Concurrent `/readyz` callers share one in-flight probe even when the result cache is disabled. Block or restrict `/metrics` at the platform edge if you do not want it public.

Anything else returns `404`; a wrong method returns `405` with an `Allow` header (`GET, POST` on `/dns-query`, `GET, HEAD` on the other endpoints). Every response generated by the gateway carries `X-Content-Type-Options: nosniff`.

`/dns-query` status codes generated by the gateway:

| Status | Cause |
|---|---|
| `400` | Malformed or missing `dns` parameter, unreadable body, or message shorter than 12 bytes. |
| `405` | Method other than GET/POST (`Allow: GET, POST`). |
| `413` | POST body larger than 4096 bytes. |
| `415` | POST without exactly `application/dns-message`. |
| `429` | Per-client rate limit exceeded (`Retry-After` = seconds to the end of the window). |
| `502` | Backend unreachable, truncated/oversize/non-DoH response, a 2xx body shorter than the 12-byte DNS header, or 1xx/3xx from the backend. |
| `503` | Active-request ceiling reached, or no processing slot within `QUEUE_WAIT` (both with `Retry-After: 1`). |
| `504` | Backend did not answer (response headers or body) within `UPSTREAM_TIMEOUT`. |

A 4xx/5xx from MosDNS itself is relayed to the client with its status. If the
gateway observes client cancellation before writing a response—while waiting
for MosDNS or while reading its answer—it returns without writing a response
and does not increment a response-status class. Once a response status has
been written, that status remains counted even if a subsequent body write
fails because the client disconnected.

Gateway-generated `429`, `503`, the `400` for a bad POST body and the `502`
"bad upstream request" carry a short `text/plain` body. The other
gateway-generated errors (`400` for a bad GET parameter, `405`, `413`, `415`,
`504`, and `502` for an unreachable, oversize or non-DoH backend response) have
an empty body.

## Configuration (environment variables)

Gateway:

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | Listen address (plain number 1-65535, or a Go address such as `127.0.0.1:8080`). An out-of-range number falls back to `8080`; any other value is passed to the listener as-is and is fatal at startup if it is not a valid address. |
| `MOSDNS_DOH_URL` | `http://127.0.0.1:8081/dns-query` | Backend; must be absolute http(s). Fatal at startup if malformed. Keep it in sync with the listener in `mosdns.yaml`. |
| `RATE_LIMIT` | `100` | `/dns-query` requests per client per window. Every GET/POST counts, including ones later rejected with `400`/`413`/`415`. |
| `RATE_WINDOW` | `60s` | Rate-limit window (fixed, clock-aligned windows). |
| `RATE_LIMIT_CLIENTS` | `65536` | Max distinct client keys per window; new clients are denied beyond this until the window rolls over. IPv6 clients are grouped by /64. |
| `MAX_ACTIVE_REQUESTS` | `16` | Hard ceiling on concurrently admitted requests (extra get `503` + `Retry-After: 1`). |
| `MAX_CONCURRENT_REQUESTS` | `8` | Requests actually executing against MosDNS at once (also the size of the backend connection pool, idle and total); the rest queue up to `QUEUE_WAIT`. A slot is released as soon as MosDNS's answer has been read, before it is written to the client, so a slow-reading client does not hold one. If it is set higher than `MAX_ACTIVE_REQUESTS`, a warning is logged at startup: requests are then rejected before they can queue, so `QUEUE_WAIT` never applies. |
| `QUEUE_WAIT` | `100ms` | Max wait for a processing slot, then `503`. |
| `UPSTREAM_TIMEOUT` | `2.5s` | Per-request backend timeout (`504` on timeout, `502` otherwise). Must stay above MosDNS `servers[0].timeout` (2 s). The server write deadline grows with it — it always covers the body-read + queue + upstream budget plus a 2 s margin, so even a slow POST upload followed by a full queue wait and an upstream timeout still ends in the intended `503`/`504` (see Architecture). |
| `CLIENT_IP_HEADER` | `none` (`X-Forwarded-For` in the shipped Dockerfile) | Set to e.g. `X-Forwarded-For` to trust a reverse proxy's client-IP header: the last entry of the last header value is used; if it is missing or not a valid IP, the socket peer is used. No other header (such as `X-Real-IP`, which clients can set themselves) is trusted. IPv4-mapped IPv6 addresses are normalised to IPv4, and IPv6 clients are rate-limited per /64. Default keys rate limits on the socket peer only (see the note below). |
| `TRUSTED_PROXY_CIDRS` | empty (every peer may set the header) | Comma-separated CIDR list of the proxy peers allowed to set `CLIENT_IP_HEADER`. When set, a header arriving from any other socket peer is ignored and that peer is rate-limited by its own address, so a client that can reach the container directly cannot rotate spoofed addresses to get fresh rate-limit buckets or exhaust the per-window client cap. Example: `TRUSTED_PROXY_CIDRS=10.0.0.0/8,2001:db8::/32`. Fatal at startup if a CIDR is malformed; a warning is logged if it is set while `CLIENT_IP_HEADER` is `none`. |

**Behind a PaaS proxy, set `CLIENT_IP_HEADER`.** With the default (`none`) the
rate limiter sees only the socket peer, which on Koyeb and similar platforms is
typically the platform's own proxy, so every client arriving through the same proxy
address shares one `RATE_LIMIT` bucket (100 requests per minute by default).
The shipped Dockerfile already sets `CLIENT_IP_HEADER=X-Forwarded-For` so each
real client gets its own bucket; set it back to `none` (or remove it) if the
container is not reachable solely through your trusted proxy, because the
header is otherwise spoofable. If the container can be reached both through
the proxy and directly, also set `TRUSTED_PROXY_CIDRS` to the proxy's ranges
so spoofed headers from direct clients are ignored.

Integer variables must be >= 1 and duration variables must be positive Go
durations (`100ms`, `2.5s`). An invalid value is ignored: the default is used
and a warning is logged at startup.

The Dockerfile sets only `MOSDNS_CONFIG`, `MOSDNS_DOH_URL`, `UPSTREAM_TIMEOUT=2.5s`
(pinned because `check-config.sh` ties it to the MosDNS timeout), `CLIENT_IP_HEADER=X-Forwarded-For`
and the runtime tuning values `GOMAXPROCS=1` and `GOMEMLIMIT=160MiB`; everything
else, including `PORT`, `RATE_LIMIT`, `RATE_WINDOW`, `MAX_CONCURRENT_REQUESTS`,
`MAX_ACTIVE_REQUESTS` and `QUEUE_WAIT`, relies on the built-in defaults above
(the image still `EXPOSE`s 8080). The Go runtime values are ordinary environment
variables, so both the gateway and MosDNS inherit them; the memory limit applies
to each process separately, not to the container as a whole.

Client headers (Cookie, Authorization, ...) are never forwarded upstream; only
`Accept` and, for POST, `Content-Type` are set by the gateway (Go's HTTP client
adds its own `Host`, `User-Agent` and `Content-Length`). The client address is
used only for rate limiting and is not sent to MosDNS, because `mosdns.yaml`
does not use it; `check-config.sh` therefore rejects
`get_user_ip_from_header`. The answer is relayed with hop-by-hop headers removed
and `Content-Length` recomputed from the validated body. Backend redirects are
never followed.

## MosDNS configuration

`mosdns.yaml` runs, in order: `readiness` (an `arbitrary` plugin answering
`ready.doh-gateway.invalid A 127.0.0.1` for `/readyz`, never cached and never
forwarded), `_edns0_filter_ecs_only`, `cache` (`cache_everything: true`,
`when_hit: _return`), then a `primary` / `secondary` fallback in `main_sequence` with `fast_fallback: 400`
(ms). `primary_fast` races `root.hagezi.org` and `wurzn.hagezi.org`;
`fallback_hagezi` is `juuri.hagezi.org`. Upstreams use pinned `dial_addr` IPs,
so no bootstrap DNS lookup is needed, but those IPs must be updated by hand if
HaGeZi renumbers its endpoints. Both `primary_fast` upstreams are
`trusted: true`: MosDNS v4.5.3 trusts only the first upstream of a
`fast_forward` by default and drops non-NOERROR answers (NXDOMAIN, SERVFAIL)
from the others, which would reduce the race to `root.hagezi.org` for those
answers. The server timeout is 2 s (mirrored by `mosdnsServerTimeout` in
`main.go`). `enable_pipeline` has no effect on DoH upstreams in MosDNS v4.5.3
(it only applies to TCP/DoT) and `max_conns` already defaults to 2 for DoH, so
the file sets neither; `check-config.sh` validates these and other optional
keys (`idle_timeout`, `always_standby`) only when present.

Why the EDNS filter and `cache_everything`: MosDNS v4.5.3 only caches "simple"
queries unless `cache_everything` is true, and any EDNS0 OPT record (sent by
nearly every DoH client, including browsers) makes a query non-simple, so the
previous config sent almost every query upstream (~100-170 ms each).
`cache_everything` alone is not enough either, because EDNS padding and
cookies differ per query and are part of the cache key. With
`_edns0_filter_ecs_only` ahead of the cache, repeat queries are answered from
cache in under 1 ms (measured end to end through the gateway).

`check-config.sh` (requires Python 3.8+ with PyYAML; run via
`make check-config`) validates it:

- YAML structure, plugin tags/types and upstream entries;
- the entry sequence (`servers[0].exec`) layout: optional local-answer
  (`arbitrary`/`hosts`) and `_edns0_filter_*` entries, then a `cache` plugin,
  then a `primary`/`secondary` fallback whose tags are `fast_forward` plugins
  with at least one upstream each (tag names are free); in a `fast_forward`
  with several upstreams, every upstream after the first must be
  `trusted: true`;
- `cache_everything` is true (unless `_edns0_filter_no_edns0` is used) and an
  `_edns0_filter_*` entry precedes the cache;
- an `arbitrary` plugin before the cache answers the gateway's
  `readinessProbeName` from `main.go`, so `/readyz` cannot silently start
  depending on the upstreams;
- exactly one server with exactly one listener, which must use the `http`
  protocol and bind to loopback (an `api.http` listener, if present, must also
  be loopback);
- the Dockerfile's `MOSDNS_DOH_URL` host, port and path match that listener;
- the timeout chain `fast_fallback < servers[0].timeout < UPSTREAM_TIMEOUT`,
  using `UPSTREAM_TIMEOUT` from the Dockerfile (`<number>ms`, `<number>s`,
  `<number>m` or `<number>h`) or, if unset there, `defaultUpstreamTO` from
  `main.go` (`<n> * time.Millisecond`, `<n> * time.Second`, `<n> * time.Minute`
  or `<n> * time.Hour`). The check fails (rather than silently skipping) if
  neither can be read; `mosdnsServerTimeout` in `main.go` must equal
  `servers[0].timeout`;
- `get_user_ip_from_header` is not set on the listener (the gateway does not
  send the client address);
- the Go version (major.minor) agrees across `go.mod`, the Dockerfile's
  `gateway-builder` `FROM golang:...` line and every `go-version` in
  `.github/workflows/ci.yml` (skipped for files that are absent), and the
  `mosdns-builder` stage uses Go 1.19.

The checks are written for the pinned, MosDNS v4.5.3-compatible layout. If
`MOSDNS_VERSION` is overridden at image-build time, review this script for
compatibility with the selected MosDNS release.

## Build and test

```sh
make test            # gofmt check + check-config + entrypoint-test + go vet + go test -race (needs Python 3.8+/PyYAML)
make check-config    # validate mosdns.yaml and cross-file consistency only
make entrypoint-test # run test-entrypoint.sh (supervision test with fake children)
make entrypoint-test-busybox # the same under BusyBox ash, the shell of the image
make vet             # go vet only
make build           # local binary ./doh-gateway
make docker-build    # multi-stage image (MosDNS + gateway)
make run             # local gateway on :8080 (needs a backend on :8081)
make clean           # remove ./doh-gateway
```

`test-entrypoint.sh` runs the real `entrypoint.sh` against fake MosDNS and
gateway processes and checks, for each scenario, both the exit code and that
neither fake child is still running afterwards: TERM shutdown exits 0; a
SIGKILLed MosDNS or gateway and a child that exits on its own are detected
(exit 1, the other child stopped); a child that ignores TERM is KILLed after its
grace period; MosDNS exiting while the gateway ignores TERM still ends in
KILL and exit 1; and a TERM that arrives during that crash teardown keeps exit
1. The seven scenarios take roughly 25 s. It runs under `sh`;
set `TEST_SHELL` (for example `TEST_SHELL="busybox sh"`) to run it under another
shell.

`go test ./...` on its own does not need Python, but it does need Go 1.27 or
newer (see `go.mod`).

`make test` runs the unit tests with `-race`, which covers the
concurrency-sensitive paths (POST buffer pool, per-client limiter, processing
queue, shared readiness probe). CI (`.github/workflows/ci.yml`) runs `make
test`, the entrypoint test again under BusyBox ash, and a full Buildx image
build (GitHub Actions layer cache, no push) followed by a smoke test of the
built image: it starts the container, waits for `/readyz`, checks `/healthz`,
a DoH query for the local readiness name, a `404`, the `build_info` version,
and that `docker stop` finishes within 10 s (no public upstream is contacted).
It runs on every push to `main` and every pull request, has read-only
repository permissions and cancels a run that a newer push to the same ref
supersedes.

Optional hard throughput gate: `REQUIRE_5000_RPS=1 go test -run TestFiveThousandRPSTarget`
(it drives the gateway with as many concurrent workers as the default
`MAX_ACTIVE_REQUESTS`, because more in-flight requests than that are rejected
with `503` by design).
Sustained-load benchmark: `go test -run ^$ -bench BenchmarkGatewayParallelPOST -benchmem` (reports ns/op and allocations; it is not a rate gate).

Docker build arguments (all have defaults in the Dockerfile). The builder
stages run on `$BUILDPLATFORM` and cross-compile for `TARGETARCH` (GOARM is
set only for 32-bit arm), so `docker buildx build --platform
linux/amd64,linux/arm64 .` does not run Go under emulation. The base images
(`golang:1.19.13-bookworm` for MosDNS, `golang:1.27-bookworm` for the gateway,
`alpine:3.24.2` for the runtime) are written literally in the `FROM` lines,
each pinned by multi-arch digest. They are not build arguments because
Dependabot cannot read ARG-templated `FROM` lines. `.github/dependabot.yml`
proposes digest and patch updates weekly; for `golang` it ignores minor and
major bumps, which keeps the MosDNS builder on Go 1.19 and leaves gateway Go
minor upgrades to a manual change of the `FROM` line, `go.mod` and `ci.yml`
together.

| Argument | Default | Notes |
|---|---|---|
| `MOSDNS_VERSION` | `v4.5.3` | MosDNS tag to build. |
| `MOSDNS_COMMIT` | commit of `v4.5.3` | Mandatory full SHA; the build fails if it is empty or the tag resolves to a different commit. |
| `GATEWAY_VERSION` | empty: the build reads `VERSION` | Optional override for `main.version`; also sets the image `version` label (empty unless passed). |
| `BUILD_DATE` | `unknown` | Image `created` label. |

`make docker-build` passes `GATEWAY_VERSION` (from `VERSION`) and `BUILD_DATE`.

## Deployment

- The image exposes port `8080`; the gateway listens on `PORT` (default
  `8080`). Run it behind a TLS-terminating platform proxy; the image itself
  serves plain HTTP.
- Use `GET /readyz` as the HTTP health check: it checks the gateway and
  MosDNS but not the public upstreams, so an upstream outage does not make
  the platform restart the instance (Koyeb restarts after 3 consecutive
  failed checks). `/healthz` only shows that the gateway process is up.
- Keep `CLIENT_IP_HEADER=X-Forwarded-For` (the image default) only when the
  container is reachable solely through your trusted proxy; otherwise set it to
  `none`. If the container can be reached both through the proxy and directly,
  also set `TRUSTED_PROXY_CIDRS` to the proxy's ranges so spoofed headers from
  direct clients are ignored.
- Restrict `/metrics` at the platform edge if it should not be public.
- The container runs as the unprivileged user `app`.

## Repository automation

- `.github/workflows/ci.yml` — described under *Build and test* above. It does
  not cache Go modules (the project has no dependencies and no `go.sum`).
- `.github/dependabot.yml` — weekly update PRs for the Dockerfile base images
  (digests and patch releases; no golang minor/major bumps) and the GitHub
  Actions versions.

## Version

The gateway version lives only in `VERSION` (currently 0.5.14); the Dockerfile
reads it during the build (plain `docker build`, as Koyeb does, gets it too)
and injects it with `-ldflags -X main.version=...`. The MosDNS version and its
commit pin are set in the Dockerfile (`ARG MOSDNS_VERSION`, `ARG
MOSDNS_COMMIT`). To bump the gateway version, edit `VERSION` only. Release notes are in [`CHANGELOG.md`](CHANGELOG.md).

## 🌐 Free DNS Services

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns.mydoh.workers.dev/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` |

## ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:

- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See [`LICENSE`](LICENSE). The container image includes this license at
`/usr/share/licenses/doh-gateway/LICENSE` and the license of the bundled MosDNS
source at `/usr/share/licenses/mosdns/LICENSE`.
