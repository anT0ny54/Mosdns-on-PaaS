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

Health endpoint:

```text
https://<your-koyeb-domain>/healthz
```

## Upstream policy

`fast_forward` uses the following two primary DoH endpoints and returns the first usable response:

- `https://root.hagezi.org/dns-query` → `188.34.161.210`
- `https://wurzn.hagezi.org/dns-query` → `159.69.155.94`

If the primary path has not produced a usable result within the configured `fast_fallback: 2000 ms` window, the secondary path is activated:

- `https://juuri.hagezi.org/dns-query` → `95.217.163.17`

The HTTPS hostname remains the TLS identity while `dial_addr` pins the connection to the supplied IPv4 address.

## Request protection

The gateway enforces:

- **100 requests / 60 seconds / client IP** using a fixed-window limiter.
- At most **65536 distinct client keys per rate-limit window in the shipped Docker image**; new clients are rejected with HTTP 429 once the memory guard is full. The Go source default is also 65536 when `RATE_LIMIT_CLIENTS` is not set.
- Up to **512 active requests per service instance** can be admitted; requests beyond that receive an immediate HTTP 503.
- The shipped Docker image limits backend processing to **32 requests at once** (`MAX_CONCURRENT_REQUESTS=32`). A queued request waits at most **100ms** (`QUEUE_WAIT=100ms`) for a processing slot; if no slot opens, it receives HTTP 503 immediately instead of accumulating long tail latency. This separates burst absorption from CPU/upstream concurrency.
- Maximum DoH DNS message size of **4096 bytes** for both request and response.
- DoH POST requires `Content-Type: application/dns-message`.
- DoH GET requires a valid unpadded URL-safe base64 `dns` parameter.
- `ReadHeaderTimeout=5s`, `ReadTimeout=8s`, `WriteTimeout=8s`, `IdleTimeout=20s`.
- Upstream response-header timeout is **5s**; the shipped Docker image sets the gateway's overall upstream request context timeout to **4s** (`UPSTREAM_TIMEOUT=4s`). The HTTP client permits up to **32 connections per MosDNS host**.
- Request logging is disabled by default.

The client-IP source defaults to `X-Forwarded-For`, matching the Koyeb reverse-proxy deployment model. The gateway falls back to `X-Real-IP` and then the socket peer address.

## Resource tuning

The container is configured for the target limits:

- Runtime: **Alpine Linux 3.24.2**.
- Build toolchain: **Go 1.19.13**.
- MosDNS: **v4.5.3**.
- `GOMAXPROCS=1` keeps the gateway and MosDNS from oversubscribing a 0.1 vCPU instance.
- `GOGC=150` reduces garbage-collection frequency while `GOMEMLIMIT=160MiB` provides the configured soft runtime memory target per Go process in the shipped image.
- The gateway admits up to **512 active requests**, but the shipped image limits backend processing to **32** at once and fast-rejects requests that wait more than **100ms** for a processing slot. This keeps burst memory and upstream work bounded for a 0.1 vCPU instance.
- The HTTP client allows up to **32 connections per MosDNS host** and **128 idle connections globally**. The 32-connection transport limit matches the processing semaphore.
- MosDNS uses a **32768-entry cache** and a **30-second lazy-cache reply TTL**, with `lazy_cache_ttl=3600s` and `cache_everything=false`, to reduce repeated upstream traffic without caching every response indiscriminately.
- DoH upstream HTTP/3 is not enabled; the configured upstream entries enable MosDNS upstream connection pipelining, use `idle_timeout=20s`, and allow `max_conns=2` per upstream.
- The unused loopback UDP/TCP MosDNS listeners are omitted; the gateway is the sole consumer of the MosDNS HTTP endpoint.

### Shipped container defaults

These are the effective defaults baked into the Docker image and `mosdns.yaml`:

| Setting | Value | Source |
|---|---:|---|
| `PORT` | `8080` | Dockerfile |
| `RATE_LIMIT` | `100` requests / `60s` | Dockerfile |
| `RATE_LIMIT_CLIENTS` | `65536` | Dockerfile |
| `MAX_ACTIVE_REQUESTS` | `512` | Go default |
| `MAX_CONCURRENT_REQUESTS` | `32` | Dockerfile |
| `QUEUE_WAIT` | `100ms` | Dockerfile |
| `UPSTREAM_TIMEOUT` | `4s` | Dockerfile |
| HTTP response-header timeout | `5s` | Go transport |
| HTTP max connections per MosDNS host | `32` | Go transport |
| HTTP max idle connections | `128` | Go transport |
| `GOMAXPROCS` | `1` | Dockerfile |
| `GOGC` | `150` | Dockerfile |
| `GOMEMLIMIT` | `160MiB` | Dockerfile |
| MosDNS cache size | `32768` | `mosdns.yaml` |
| MosDNS lazy-cache reply TTL | `30s` | `mosdns.yaml` |
| MosDNS lazy-cache TTL | `3600s` | `mosdns.yaml` |
| MosDNS `cache_everything` | `false` | `mosdns.yaml` |

The Go source defaults are **32 concurrent backend requests**, **65536 rate-limit client keys**, and a **4s upstream timeout** when the corresponding environment variables are not set. The shipped Docker image keeps the upstream timeout at the Go default of **4s**; its concurrency and client-key values match the Go defaults.

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
8080:http:/readyz
```

`/healthz` is a lightweight gateway liveness check; `/readyz` also verifies that the internal MosDNS DoH listener is reachable.

The same service can be deployed from this repository using Koyeb's Docker builder.

## Build

`make test` runs the repository's configuration validation script, `go vet`, and the Go test suite. The configuration check requires Python 3.8+ with PyYAML.

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

The image builds MosDNS v4.5.3 from its tagged source with Go 1.19.13, then builds the gateway with the same toolchain and runs both in an Alpine 3.24 runtime image.

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
