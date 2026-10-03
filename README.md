# Mosdns-on-PaaS

MosDNS v4.5.3 + a custom Go DoH gateway, packaged as a single container for
PaaS platforms (Koyeb and similar). The gateway is the only public listener;
MosDNS serves DNS-over-HTTPS on loopback behind it.

## Architecture

- **doh-gateway** (this repo, `main.go`): an HTTP front door that validates,
  rate-limits, queues, and forwards DoH requests to MosDNS. Runs as an
  unprivileged user, drains gracefully on SIGTERM (10 s).
- **MosDNS v4.5.3** (built from upstream in the Dockerfile): listens on
  `127.0.0.1:8081`, answers with a cache plus a two-HaGeZi-upstream race and
  a 400 ms fast fallback to a standby HaGeZi endpoint.

`entrypoint.sh` starts both, stops the gateway first on shutdown (so it can
drain while MosDNS still answers), and treats either process exiting as a
deployment failure.

## HTTP endpoints

| Path | Behavior |
|---|---|
| `GET/POST /dns-query` | DoH. POST requires `Content-Type: application/dns-message`; GET requires a raw (unpadded) base64url `dns` query parameter. Bodies must be 12-4096 bytes. |
| `GET/HEAD /healthz` | Liveness. Always `200 ok` if the gateway process is up. |
| `GET/HEAD /readyz` | Readiness. Sends a real DNS query through MosDNS and validates the response (transaction ID, QR bit, RCODE 0/3, echoed question). Result cached 5 s on success, 1 s on failure. |
| `GET/HEAD /metrics` | Plain-text Prometheus-style counters: request totals, rate-limit/queue/backend-error totals, 2xx-5xx counts, queue/backend latency averages. |

## Configuration (environment variables)

Gateway:

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | Listen address (plain number or Go address). |
| `MOSDNS_DOH_URL` | `http://127.0.0.1:8081/dns-query` | Backend; must be absolute http(s). Fatal at startup if malformed. |
| `RATE_LIMIT` | `240` | Requests per client per window. |
| `RATE_WINDOW` | `60s` | Rate-limit window. |
| `RATE_LIMIT_CLIENTS` | `65536` | Max distinct client keys per window; new clients are denied beyond this. IPv6 clients are grouped by /64. |
| `MAX_ACTIVE_REQUESTS` | `512` | Hard ceiling on concurrently admitted requests (extra get `503` + `Retry-After: 1`). |
| `MAX_CONCURRENT_REQUESTS` | `20` | Requests actually executing against MosDNS at once; the rest queue up to `QUEUE_WAIT`. |
| `QUEUE_WAIT` | `200ms` | Max wait for a processing slot, then `503`. |
| `UPSTREAM_TIMEOUT` | `3s` | Per-request backend timeout (`504` on timeout, `502` otherwise). Must stay above MosDNS `servers[0].timeout` (2 s). |
| `CLIENT_IP_HEADER` | `none` | Set to e.g. `X-Forwarded-For` to trust a reverse proxy's client-IP header (with `X-Real-IP` fallback). Default keys rate limits on the socket peer only. |

Runtime tuning (set in the Dockerfile): `GOMAXPROCS=1`, `GOGC=150`,
`GOMEMLIMIT=160MiB`.

Client headers (Cookie, Authorization, ...) are never forwarded upstream; only
`Accept`, `Content-Type`, and `X-Forwarded-For` are sent. Backend redirects
are never followed.

## MosDNS configuration

`mosdns.yaml` is validated by `check-config.sh` (requires Python 3.8+ with
PyYAML) - run via `make check-config`. It pins the expected plugin layout
(cache, then primary_fast race, then fallback_hagezi with 400 ms
`fast_fallback`) and enforces that the plain-HTTP listener stays on loopback.

## Build and test

```sh
make test            # config check + go vet + go test (needs Python 3.8+/PyYAML)
make build           # local binary ./doh-gateway
make docker-build    # multi-stage image (MosDNS + gateway)
make run             # local gateway on :8080 (needs a backend on :8081)
```

Optional hard throughput gate: `REQUIRE_5000_RPS=1 go test -run TestFiveThousandRPSTarget`.
Sustained-load benchmark: `go test -bench BenchmarkGateway5000RPS -benchmem`.

## Version

Gateway version is tracked in `VERSION` (0.5.4) and injected at build time
via `-ldflags -X main.version=...`; the MosDNS version is pinned in the
Dockerfile (`ARG MOSDNS_VERSION=v4.5.3`, with an optional `MOSDNS_COMMIT`
supply-chain pin).

## 🌐 Free DNS Services

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not used in 15 minutes) |

## ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:

- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See [`LICENSE`](LICENSE).
