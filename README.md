# MosDNS v4.5.3 + DoH Gateway for Koyeb

A small, hardened DNS-over-HTTPS gateway built around **MosDNS v4.5.3**, tuned for a **512 MiB / 0.1 vCPU Koyeb web service**.

## Architecture

```text
                             ┌─ HaGeZiDNS1
DoH client → Koyeb → Gateway ┤
                             └─ HaGeZiDNS2
                                  ↓
                               primary
                                  │
                              fallback
                                  ↓
                             HaGeZiDNS3
```

The gateway is the only publicly exposed HTTP listener. MosDNS handles DNS resolution internally on loopback.

### Ports

| Listener | Purpose | Exposure |
|---|---|---|
| `${PORT}` (default `8080`) | DoH gateway | Public Koyeb HTTP |
| `127.0.0.1:8081` | MosDNS DoH backend | Internal only |

Public endpoint after deployment:

```text
https://<your-koyeb-domain>/dns-query
```

Health endpoints:

```text
https://<your-koyeb-domain>/healthz
https://<your-koyeb-domain>/readyz
https://<your-koyeb-domain>/metrics
```

## Upstream policy

`fast_forward` uses the following two primary DoH endpoints and returns the first usable response:

- `https://root.hagezi.org/dns-query` → `188.34.161.210`
- `https://wurzn.hagezi.org/dns-query` → `159.69.155.94`

If the primary path has not produced a usable result within the configured `fast_fallback: 400 ms` window, the secondary path is activated:

- `https://juuri.hagezi.org/dns-query` → `95.217.163.17`

The HTTPS hostname remains the TLS identity while `dial_addr` pins the connection to the supplied IPv4 address.

## Request protection

The gateway enforces:

- **240 requests / 60 seconds / client IP** using a fixed-window limiter. IPv6 clients are grouped by **/64**.
- At most **65536 distinct client keys per rate-limit window in the shipped Docker image**; new clients are rejected with HTTP 429 once the memory guard is full. The Go source default is also 65536 when `RATE_LIMIT_CLIENTS` is not set.
- Up to **512 active requests per service instance** can be admitted; requests beyond that receive an immediate HTTP 503.
- The shipped Docker image limits backend processing to **20 requests at once** (`MAX_CONCURRENT_REQUESTS=20`). A queued request waits at most **200ms** (`QUEUE_WAIT=200ms`) for a processing slot; if no slot opens, it receives HTTP 503 immediately instead of accumulating long tail latency. This separates burst absorption from CPU/upstream concurrency.
- DoH DNS messages must be between **12 bytes** (a bare DNS header) and **4096 bytes**; the 4096-byte cap applies to both request and response.
- Request bodies are read and validated **before** a backend processing slot is taken, so a slow-sending client cannot occupy one.
- Only `Accept`, `Content-Type` and `X-Forwarded-For` are forwarded to MosDNS; cookies and other client headers are dropped.
- DoH POST requires `Content-Type: application/dns-message`.
- DoH GET requires a valid unpadded URL-safe base64 `dns` parameter.
- `ReadHeaderTimeout=5s`, `ReadTimeout=5s`, `WriteTimeout=8s`, `IdleTimeout=20s`.
- Graceful shutdown: on SIGTERM/SIGINT the gateway stops accepting connections and drains in-flight requests for up to 10s.
- The gateway's upstream request timeout is **3s** (`UPSTREAM_TIMEOUT`, default), and the HTTP response-header timeout follows it. It is deliberately above MosDNS's own 2s server timeout so a slow upstream surfaces as a MosDNS SERVFAIL instead of a gateway timeout. The HTTP client permits up to `MAX_CONCURRENT_REQUESTS` (default **20**) connections per MosDNS host.
- Request logging is disabled by default.

The client-IP source defaults to `X-Forwarded-For`, matching the Koyeb reverse-proxy deployment model. The gateway falls back to `X-Real-IP` and then the socket peer address. Because these headers are only trustworthy behind a proxy that sets them, set `CLIENT_IP_HEADER=none` when the gateway is exposed directly: both headers are then ignored and the socket peer address is used.

## Resource tuning

The container is configured for the target limits:

- Runtime: **Alpine Linux 3.24.2**.
- Build toolchain: **Go 1.19.13** for MosDNS (required by its dependencies) and **Go 1.24** for the gateway.
- MosDNS: **v4.5.3**.
- `GOMAXPROCS=1` keeps the gateway and MosDNS from oversubscribing a 0.1 vCPU instance.
- `GOGC=150` reduces garbage-collection frequency while `GOMEMLIMIT=160MiB` provides the configured soft runtime memory target per Go process in the shipped image.
- The gateway admits up to **512 active requests**, but the shipped image limits backend processing to **20** at once and fast-rejects requests that wait more than **200ms** for a processing slot. This keeps burst memory and upstream work bounded for a 0.1 vCPU instance.
- The HTTP client allows up to **20 connections per MosDNS host** and **128 idle connections globally**. A separate one-connection client is reserved for `/readyz`.
- MosDNS uses a **32768-entry cache** and a **30-second lazy-cache reply TTL**, with `lazy_cache_ttl=3600s` and `cache_everything=false`, to reduce repeated upstream traffic without caching every response indiscriminately.
- DoH upstream HTTP/3 is not enabled; the configured upstream entries enable MosDNS upstream connection pipelining, use `idle_timeout=20s`, and allow `max_conns=2` per upstream.
- The unused loopback UDP/TCP MosDNS listeners are omitted; the gateway is the sole consumer of the MosDNS HTTP endpoint.

### Shipped container defaults

These are the effective defaults baked into the Docker image and `mosdns.yaml`:

| Setting | Value | Source |
|---|---:|---|
| `PORT` | `8080` | Dockerfile |
| `RATE_LIMIT` | `240` requests / `60s` | Go default |
| `RATE_LIMIT_CLIENTS` | `65536` | Go default |
| `MAX_ACTIVE_REQUESTS` | `512` | Go default |
| `MAX_CONCURRENT_REQUESTS` | `20` | Go default |
| `QUEUE_WAIT` | `200ms` | Go default |
| `UPSTREAM_TIMEOUT` | `3s` | Go default |
| `CLIENT_IP_HEADER` | `X-Forwarded-For` (`none` to disable) | Go default |
| HTTP response-header timeout | `= UPSTREAM_TIMEOUT` | Go transport |
| HTTP max connections per MosDNS host | `= MAX_CONCURRENT_REQUESTS` | Go transport |
| HTTP max idle connections | `128` | Go transport |
| `GOMAXPROCS` | `1` | Dockerfile |
| `GOGC` | `150` | Dockerfile |
| `GOMEMLIMIT` | `160MiB` | Dockerfile |
| MosDNS cache size | `32768` | `mosdns.yaml` |
| MosDNS lazy-cache reply TTL | `30s` | `mosdns.yaml` |
| MosDNS lazy-cache TTL | `3600s` | `mosdns.yaml` |
| MosDNS `cache_everything` | `false` | `mosdns.yaml` |

The Dockerfile only sets values that are not already the Go source defaults (`PORT`, the MosDNS paths/URL, and the Go runtime limits), so every gateway tunable above can be overridden with an environment variable and otherwise follows `main.go`.

## Koyeb deployment

Koyeb provides the service `PORT` environment variable for Web Services. Koyeb supplies it as a numeric value such as `8080`; the gateway normalizes that form to the Go listen address `:8080` automatically.

Recommended exposed port configuration:

```text
Port: 8080
Protocol: HTTP
Route: /
```

Recommended health check:

```text
8080:http:/healthz
```

`/healthz` is a lightweight gateway liveness check and is the better choice for the platform health check: `/readyz` depends on the HaGeZi upstreams, so using it would make Koyeb restart the instance during an upstream outage that a restart cannot fix. Use `/readyz` for external monitoring. It performs a DoH DNS probe through the internal MosDNS listener and returns ready only when that probe gets a valid non-SERVFAIL DNS response; results are cached for 5s (1s when failing) and probes are serialized, so the public endpoint cannot be used to flood the upstreams. `/metrics` exposes lightweight gateway counters (every `/dns-query` response is counted by status class, including ones the gateway generates itself), queue-wait telemetry, and gateway-to-MosDNS latency. It does not expose per-HaGeZi upstream latency; that remains inside MosDNS.

The same service can be deployed from this repository using Koyeb's Docker builder.

## Build

`make test` runs the repository's configuration validation script, `go vet`, and the Go test suite. The configuration check requires Python 3.8+ with PyYAML, and also verifies that the MosDNS listener is on loopback and matches the Dockerfile's `MOSDNS_DOH_URL`.

```sh
python3 -m pip install pyyaml
make test
make build
sh ./check-config.sh mosdns.yaml
```

Container build:

```sh
docker build -t mosdns-koyeb-doh-gateway:local .
```

The image builds MosDNS v4.5.3 from its tagged source with Go 1.19.13, builds the gateway with Go 1.24, and runs both in an Alpine 3.24 runtime image. Builds target the builder's architecture (`TARGETARCH`). To pin the MosDNS tag to a commit, pass `--build-arg MOSDNS_COMMIT=<full sha>`; the build fails if the tag resolves elsewhere.

## Files

```text
.
├── .dockerignore
├── .gitattributes
├── .gitignore
├── .github/
│   └── workflows/
│       └── Keep-Alive.yml
├── Dockerfile
├── LICENSE
├── Makefile
├── README.md
├── VERSION
├── CHANGELOG.md
├── check-config.sh
├── entrypoint.sh
├── go.mod
├── keep-alive.txt
├── main.go
├── main_test.go
├── mosdns.yaml
└── throughput_test.go
```

## Upstream references

- MosDNS: https://github.com/IrineSistiana/mosdns
- MosDNS v4.5.3: https://github.com/IrineSistiana/mosdns/tree/v4.5.3
- HaGeZi DNS: https://github.com/hagezi/dns-servers
- Koyeb service exposure: https://www.koyeb.com/docs/build-and-deploy/exposing-your-service


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
