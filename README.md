# MosDNS on Koyeb — optimized public DoH

This project packages the official **MosDNS v5.3.4** with a very small Go front-end for public DNS-over-HTTPS (DoH) on Koyeb.

The target profile is **512 MB RAM / 0.1 vCPU / 2 GB SSD**. The design keeps the hot path small:

```text
Koyeb HTTPS
   │
   ▼
Go DoH guard
   │  per-IP + global rate limits
   │  body/method checks
   │  lightweight health endpoint
   ▼
MosDNS v5.3.4 (loopback only)
   │
   ├─ RAM cache + native cache dump
   │
   └─ HTTPS DoH upstream failover
       ├─ HaGeZi root
       ├─ HaGeZi wurzn
       └─ HaGeZi juuri
```

## What changed

The original project rebuilt MosDNS v4.5.3 and patched its cache implementation with a custom warm-cache backend. That code is no longer necessary: MosDNS v5.3.4 already provides native cache persistence with `dump_file` and `dump_interval`. See the upstream [cache implementation](https://github.com/IrineSistiana/mosdns/blob/v5.3.4/plugin/executable/cache/cache.go).

The new build therefore:

- uses the official `irinesistiana/mosdns:v5.3.4` runtime image;
- removes `warm_backend.go` and the source patching step;
- uses MosDNS v5's `forward` plugin and `fallback` plugin;
- keeps MosDNS private on `127.0.0.1`;
- retains GET and POST RFC 8484 DoH compatibility in the small Go front-end;
- fixes the old fixed-upstream path so all three resolvers remain available;
- removes duplicated/contradictory legacy README content;
- uses a 4,096-entry RAM cache and 30-second upstream idle reuse as a conservative default for the tiny CPU budget;
- keeps an 1,800-second native cache snapshot, and only the cache itself determines whether a dump is actually written.

MosDNS v5.3.4 is the current stable release shown by the upstream project, and the official Docker image is published for amd64, arm/v7, arm64, and ppc64le.

## Koyeb configuration

Koyeb Web Services provide a `PORT` environment variable; this project listens on the default `8080` unless Koyeb overrides it.

Recommended Koyeb setup:

```text
Service type: Web
Builder: Dockerfile
Exposed port: 8080 / HTTP
Route: /
Minimum instances: 1
Maximum instances: 1
```

Use an HTTP health check on:

```text
/health
```

Use an HTTP health check for `/health` so readiness is based on an HTTP response rather than only a listening TCP socket.

Example CLI shape:

```bash
koyeb app init mosdns \
  --git github.com/YOUR_USERNAME/YOUR_REPOSITORY \
  --git-branch main \
  --git-builder docker \
  --ports 8080:http \
  --routes /:8080 \
  --checks 8080:http:/health
```

Koyeb supports HTTP health checks directly in service configuration and the CLI.

## DoH endpoint

After deployment:

```text
https://YOUR-KOYEB-DOMAIN/dns-query
```

A custom path can be selected with:

```text
DOH_PATH=/your-path
```

## Runtime defaults

| Variable | Default |
|---|---:|
| `PORT` | `8080` |
| `MOSDNS_BACKEND_PORT` | `18080` |
| `DOH_PATH` | `/dns-query` |
| `HEALTH_PATH` | `/health` |
| `CACHE_SIZE` | `4096` |
| `CACHE_DUMP_INTERVAL` | `1800` |
| `DOH_RATE_LIMIT` | `5/s per IP` |
| `DOH_RATE_BURST` | `12` |
| `DOH_RATE_MAX_IPS` | `512` |
| `GLOBAL_RATE_LIMIT` | `40/s` |
| `GLOBAL_RATE_BURST` | `80` |
| `GLOBAL_CONN_LIMIT` | `128` |
| `DOH_MAX_BODY_BYTES` | `4096` |
| `IP_CONN_LIMIT` | `0` |
| `UPSTREAM_IDLE_TIMEOUT` | `30s` |
| `DOH_IDLE_TIMEOUT` | `90s` |
| `SERVER_TIMEOUT` | `5s` |
| `HEALTH_CHECK` | `true` |
| `HAGEZI_UPSTREAM` | `health` |
| `GOMEMLIMIT` | `384MiB` |

`IP_CONN_LIMIT=0` is intentional. Persistent DoH clients such as Firefox/Fennec benefit from reusing a small number of long-lived connections; request-rate and global connection limits still provide abuse protection.

## Upstream policy

Only HTTPS DoH upstreams are configured:

```text
https://root.hagezi.org/dns-query
https://wurzn.hagezi.org/dns-query
https://juuri.hagezi.org/dns-query
```

The Go startup probe measures those three endpoints once, orders them by smoothed latency/failure score, and MosDNS then provides fast fallback. `HAGEZI_UPSTREAM=random` randomizes only near-equal healthy choices. The probe is not on the steady-state request path.

Pinned dial addresses are retained to avoid needing a plaintext DNS bootstrap lookup for the upstream hostnames. They are only dial targets; the HTTPS hostname remains the TLS identity. HaGeZi currently documents these same three IPv4 addresses.

## Resource strategy

The 0.1-vCPU constraint makes avoiding unnecessary resident work more important than maximizing concurrency.

The project therefore intentionally does not include GeoIP/Geosite downloads, SQLite, dnsmasq, BIND, Nginx, or another resident cache service.

The native MosDNS cache is the only hot cache. Its dump is a restart optimization, not a correctness dependency; Koyeb storage may be replaced when an Instance is recreated.

## Verification

The two helper programs use only the Go standard library. A local source check can be performed with:

```bash
gofmt -w content/*.go
go test ./...
```

The actual MosDNS binary is supplied by the upstream v5.3.4 image rather than rebuilt in this repository.

## Current upstream references

- [MosDNS GitHub](https://github.com/IrineSistiana/mosdns)
- [MosDNS v5.3.4 release](https://github.com/IrineSistiana/mosdns/releases/tag/v5.3.4)
- [Official MosDNS Docker image](https://hub.docker.com/r/irinesistiana/mosdns)
- [Koyeb environment variables](https://www.koyeb.com/docs/build-and-deploy/environment-variables)
- [Koyeb health checks](https://www.koyeb.com/docs/run-and-scale/health-checks)
