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
                               failure
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

`fast_forward` races these two primary DoH endpoints and accepts the first usable response:

- `https://root.hagezi.org/dns-query` → `188.34.161.210`
- `https://wurzn.hagezi.org/dns-query` → `159.69.155.94`

If the primary path produces no response, the sequence invokes:

- `https://juuri.hagezi.org/dns-query` → `95.217.163.17`

The HTTPS hostname remains the TLS identity while `dial_addr` pins the connection to the supplied IPv4 address.

## Request protection

The gateway enforces:

- **100 requests / 60 seconds / client IP** using a fixed-window limiter.
- At most **8192 distinct client keys per rate-limit window**; new clients are rejected with HTTP 429 once the memory guard is full.
- **16 concurrent requests per service instance**. Requests beyond the hard cap receive HTTP 503 instead of accumulating a large in-process queue.
- Maximum DoH DNS message size of **4096 bytes** for both request and response.
- DoH POST requires `Content-Type: application/dns-message`.
- DoH GET requires a valid unpadded URL-safe base64 `dns` parameter.
- `ReadHeaderTimeout=5s`, `ReadTimeout=8s`, `WriteTimeout=8s`, `IdleTimeout=20s`.
- No debug/info request logging by default.

The client-IP source defaults to `X-Forwarded-For`, matching the Koyeb reverse-proxy deployment model. The gateway falls back to `X-Real-IP` and then the socket peer address.

## Resource tuning

The container is tuned for the requested limits:

- Runtime: **Alpine Linux 3.24.2**.
- Build toolchain: **Go 1.19.13**.
- MosDNS: **v4.5.3**.
- `GOMAXPROCS=1` to avoid oversubscribing a 0.1 vCPU instance.
- `GOGC=150` reduces garbage-collection frequency while `GOMEMLIMIT=128MiB` still provides a soft runtime memory target per Go process.
- Gateway concurrency is capped at **16 in-flight requests**, with at most **16** gateway-to-MosDNS connections and **4** idle connections retained per host.
- MosDNS keeps an **8192-entry cache** while avoiding `cache_everything` to limit memory use from EDNS variants.
- DoH upstream HTTP/3 is not enabled; persistent HTTP connections/pipelining are used instead to reduce CPU overhead.
- The unused loopback UDP/TCP MosDNS listeners are omitted; the gateway is the sole consumer of the MosDNS HTTP endpoint.

## Koyeb deployment

Koyeb web services always provide a `PORT` environment variable. Koyeb supplies it as a numeric value such as `8080`; the gateway normalizes that form to the Go listen address `:8080` automatically.

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

`make test` runs the YAML/schema check, `go vet`, and the Go test suite. The configuration check requires Python 3.8+ with PyYAML.

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
├── Dockerfile
├── Makefile
├── README.md
├── VERSION
├── check-config.sh
├── entrypoint.sh
├── go.mod
├── main.go
├── main_test.go
└── mosdns.yaml
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
