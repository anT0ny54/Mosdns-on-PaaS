# MosDNS on PaaS

A small, DoH-only [MosDNS v4.5.3](https://github.com/IrineSistiana/mosdns/tree/v4.5.3) service for an HTTP Web Service. The image is tuned for a deployment budget of **512 MB RAM, 0.25 vCPU, and 2 GB SSD**.

The container keeps the public surface deliberately narrow:

- only HTTP DoH and the minimal health endpoint are exposed publicly;
- three HaGeZi DoH endpoints are used in strict sequential order;
- the upstream health probe uses the same pinned IPv4 addresses as the MosDNS `dial_addr` configuration for the built-in endpoints;
- the DNS cache is bounded in memory and has a separate bounded warm-start snapshot;
- a small Go proxy applies accept-time and source-aware request-rate, connection, and request-size limits before MosDNS;
- MosDNS and the proxy are supervised so a failed process causes the container to exit and lets the hosting platform restart the service.

## Request path

```text
client
  │ DoH GET/POST
  ▼
ip-conn-proxy :8080
  │ rate / connection / size limits
  ▼
127.0.0.1:18080
  │
  ├─ cache
  └─ sequential failover
       ├─ upstream 1
       ├─ upstream 2
       └─ upstream 3
```

## Upstreams

The built-in endpoints are:

| Endpoint | Pinned IPv4 |
| --- | --- |
| `https://root.hagezi.org/dns-query` | `188.34.161.210` |
| `https://wurzn.hagezi.org/dns-query` | `159.69.155.94` |
| `https://juuri.hagezi.org/dns-query` | `95.217.163.17` |

The pins are used as MosDNS `dial_addr` values while TLS still uses the hostname from the DoH URL. The health-probe helper applies the same mapping, so startup/runtime measurements exercise the same destinations that MosDNS will dial.

Source: [HaGeZi DNS servers](https://github.com/hagezi/dns-servers).

### Sequential failover

The sequential forwarder also has a small in-memory circuit breaker: two consecutive failures temporarily cool an upstream for 15 seconds. If all three are cooling down at once, the next query still tries the full configured set, so the breaker cannot create a total outage. This reduces repeated connection timeouts and CPU/network waste when one upstream is unreachable, while keeping the configured failover order intact.


## Upstream selection and health checks

`HAGEZI_UPSTREAM` controls the initial and runtime ordering:

| Value | Behavior |
| --- | --- |
| `rotate` | Probe all three built-ins and place the healthiest result first. |
| `random` | Probe all three built-ins and randomly rotate among near-equal healthy candidates. |
| `https://...` | Prefer this endpoint (a custom one takes the first built-in slot; a built-in URL is simply moved first) and keep the other two as fallbacks. It stays first while it passes its health probe; the probe only reorders the fallbacks. |

The probe helper maintains an EWMA latency score and consecutive-failure count in `HEALTH_STATE_FILE`. Runtime checks happen every `HEALTH_INTERVAL` seconds. Hysteresis prevents a small latency difference from causing repeated upstream swaps, while repeated failures can trigger a reorder and MosDNS restart. When a switch happens, the complete measured order from the probe (not just the new first entry) becomes the new failover order. With a fixed `https://...` endpoint, latency never demotes it: it is moved down only after it has failed `HEALTH_FAILS_TO_SWITCH` probes in a row, and the supervisor moves it back to first (subject to `HEALTH_RESTART_COOLDOWN`) once it probes healthy again.

The three built-in IP variables are validated as IPv4 addresses before configuration is rendered. A custom `HAGEZI_UPSTREAM` is not pinned by these variables and is resolved normally.

The persisted probe state is intentionally bounded. Oversized or malformed state files are ignored, and only a small number of valid entries are loaded, so a damaged state file cannot consume unbounded startup memory.

## Warm cache

The normal query path uses MosDNS's memory cache. A small custom backend keeps a second, bounded copy for warm starts and writes it atomically to disk at `CACHE_DUMP_INTERVAL`. Hot-cache inserts are capped by `CACHE_MAX_ENTRY_BYTES` (8 KiB by default), and the warm copy uses the same 8 KiB per-entry ceiling. This prevents unusually large DNS responses from multiplying across the cache and keeps duplicate cached response data bounded on the 512 MB instance (typical answers are a few hundred bytes, so the default 8192 entries use roughly 6-16 MB across the hot and warm copies). Warm metadata keeps insertion/restore recency for snapshot eviction and restoration; ordinary hot-cache hits stay on the fast inner-cache path and do not take the warm-metadata lock. The snapshot is limited to the same warm-entry set, and expired or oversized entries are discarded before restore.

Local service storage may be ephemeral, so the warm cache is an optimization only. DNS correctness does not depend on the snapshot being present after a service replacement. If the directory of `CACHE_DUMP_FILE` cannot be created, startup logs a warning and continues instead of aborting.

With the default 512 MB / 0.25 vCPU profile, `CACHE_SIZE=8192`, `CACHE_MAX_ENTRY_BYTES=8192`, `DOH_RATE_MAX_IPS=4096`, `GLOBAL_CONN_LIMIT=256`, `IP_CONN_LIMIT=16`, `UPSTREAM_MAX_CONNS=8`, and the public DoH limiter at 5 requests/second (300/minute) sustained per client IP with a 100-request burst, under a service-wide budget of 80 requests/second with a 160-request burst. The service-wide budget is sized so that cache hits and misses together stay well inside a 0.25 vCPU CPU quota (roughly 0.2-0.5 ms of CPU per request) with headroom left for garbage collection, snapshots and health probes; that is enough for several hundred regular users, since a typical user averages well under one query per second. The proxy is kept at `GOMEMLIMIT=80MiB` while MosDNS gets `GOMEMLIMIT=288MiB`; these are soft Go heap targets, not hard container-memory caps, so real headroom also depends on runtime/native memory and upstream latency. Successful backend DoH responses are buffered and DNS-validated before forwarding, with a 65535-byte response ceiling, so an unknown-length or prematurely terminated backend body cannot reach the client as a partial HTTP 200. Multiple devices behind one public IP share the same source-IP bucket; the burst absorbs short browser startup bursts, but sustained traffic above the quota will still receive HTTP 429 responses.

## DoH proxy

`content/ip_conn_proxy.go` sits in front of MosDNS and enforces:

```text
per-IP rate:        DOH_RATE_LIMIT / second (default 5/s = 300 requests/minute), burst DOH_RATE_BURST (default 100)
tracked IP buckets:  DOH_RATE_MAX_IPS
service rate:     GLOBAL_RATE_LIMIT / second, burst GLOBAL_RATE_BURST (default 80/s, burst 160)
health rate:      HEALTH_RATE_LIMIT / second, burst HEALTH_RATE_BURST
health aggregate: GLOBAL_HEALTH_RATE_LIMIT / second, burst GLOBAL_HEALTH_RATE_BURST
connections:      GLOBAL_CONN_LIMIT (accept-time atomic cap; default 256)
per-IP conn:      IP_CONN_LIMIT (follows the client IP of the connection's latest DoH/health request; default 16)
source state:     DOH_RATE_MAX_IPS (fixed-size sharded table, hard cap 4096)
request size:     DOH_MAX_BODY_BYTES
```

Each DoH request is checked against the per-client rate limit first and the global limit second, so one abusive client cannot use up the shared budget. Only RFC 8484-style GET and POST requests are accepted at `DOH_PATH`. POST requests require `Content-Type: application/dns-message`. Requests that are too large, malformed, or use another method are rejected before being sent to MosDNS.

The proxy uses the final element of the last `X-Forwarded-For` header line for client limiting, canonicalizes IPv4-mapped addresses, rejects requests when no usable source identity exists, and removes client-controlled forwarding headers before proxying to MosDNS. Rate limits are keyed only by source IP; the `Host` header never creates a separate bucket. This identity model requires a trusted reverse proxy or edge in front of the service that appends the real client address to `X-Forwarded-For` and prevents direct untrusted access to the proxy. Do not expose the proxy directly and then rely on a client-supplied `X-Forwarded-For` value for identity. The global TCP connection cap is enforced by an atomic accept-and-close listener, while the per-source-IP cap is charged from the request's client IP rather than the socket peer, so multiple clients sharing one public edge address are not treated as the same client. If the edge reuses one keep-alive connection for requests from different clients, the connection's slot moves to the client of the latest request (it is only refused when that client is already at `IP_CONN_LIMIT`); a request whose client identity cannot be determined is rejected rather than pooled into a shared bucket. Source-IP rate/connection state is held in a fixed sharded table rather than an attacker-growable map. Waiting for MosDNS response headers is capped at 12 seconds (returned to the client as `502`), so a hung backend cannot hold every proxy-to-MosDNS connection slot.

The health endpoint is a separate `GET`/`HEAD` path. It checks that the MosDNS backend TCP listener is reachable and returns `200 OK` when it is. Health requests use their own per-client and aggregate rate budgets so public DoH traffic cannot starve service health checks, while repeated health polling is still bounded.

The repository is intentionally DoH-only and does not expose a raw DNS UDP/TCP listener. The public proxy accepts only the DoH HTTP surface and keeps MosDNS on loopback; raw DNS framing helpers are not carried in the runtime build because they are unused by this deployment.

## Environment variables

The image has working defaults (defined once, in `content/entrypoint.sh`); no environment variable is required for the default deployment. The public proxy is the externally reachable rate limiter; MosDNS is kept behind its loopback listener and does not duplicate that per-client limiter. The build also patches the v4.5.3 DoH client context boundary so request cancellation reaches the underlying transport during sequential failover. The three HaGeZi candidates are the complete policy path; there is no separate emergency endpoint.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | Public service port. |
| `DOH_PATH` | `/dns-query` | Public DoH path. |
| `HEALTH_PATH` | `/health` | Proxy health path for an HTTP health check. |
| `MOSDNS_BACKEND_PORT` | `18080` | Loopback port between the proxy and MosDNS. |
| `HAGEZI_UPSTREAM` | `rotate` | `rotate`, `random`, or a fixed `https://` endpoint. |
| `UPSTREAM_0_IP` | `188.34.161.210` | Built-in `root.hagezi.org` dial pin. |
| `UPSTREAM_1_IP` | `159.69.155.94` | Built-in `wurzn.hagezi.org` dial pin. |
| `UPSTREAM_2_IP` | `95.217.163.17` | Built-in `juuri.hagezi.org` dial pin. |
| `CACHE_SIZE` | `8192` | Maximum cache entries in the hot cache and warm snapshot. Valid range is 1024-8192 for the 512 MiB profile. |
| `CACHE_MAX_ENTRY_BYTES` | `8192` | Maximum packed DNS response size eligible for the hot cache. Larger responses are served normally but are not cached. |
| `CACHE_DUMP_FILE` | `/var/cache/mosdns/cache.dump` | Warm-cache snapshot path. |
| `CACHE_DUMP_INTERVAL` | `3300` | Warm-cache snapshot interval, seconds. |
| `SERVER_TIMEOUT` | `10` | MosDNS query timeout, seconds. The proxy stops waiting for MosDNS after 12 s, so values above 12 only produce a startup warning. The sequential failover budget is divided across remaining upstream attempts. |
| `UPSTREAM_IDLE_TIMEOUT` | `60` | Upstream idle connection timeout, seconds. |
| `UPSTREAM_MAX_CONNS` | `8` | Maximum upstream connections per endpoint. Higher concurrency reduces head-of-line blocking when a sequentially preferred DoH endpoint has moderate latency. |
| `DOH_IDLE_TIMEOUT` | `120` | Internal MosDNS DoH listener idle timeout, seconds. `0` uses MosDNS v4.5.3's 10 s default. Valid explicit values are 6-3600 s. The proxy keeps pooled backend connections at least 5 s shorter, capped at 90 s. |
| `DOH_RATE_LIMIT` | `5` | Per-client source-IP request rate, requests/second (300 requests/minute sustained). |
| `DOH_RATE_BURST` | `100` | Per-client source-IP burst allowance (up to 100 DNS lookups in a short burst). |
| `DOH_RATE_MAX_IPS` | `4096` | Maximum source-IP rate buckets retained; hard-capped at 4096. |
| `GLOBAL_RATE_LIMIT` | `80` | Global request rate, requests/second. |
| `GLOBAL_RATE_BURST` | `160` | Global DoH burst allowance. |
| `HEALTH_RATE_LIMIT` | `2` | Per-client health request rate, requests/second. |
| `HEALTH_RATE_BURST` | `4` | Per-client health burst allowance. |
| `GLOBAL_HEALTH_RATE_LIMIT` | `10` | Aggregate health request rate, requests/second. |
| `GLOBAL_HEALTH_RATE_BURST` | `20` | Aggregate health burst allowance. |
| `GLOBAL_CONN_LIMIT` | `256` | Maximum concurrent public TCP connections; rejected sockets are closed at accept time. |
| `IP_CONN_LIMIT` | `16` | Per-client concurrent connection limit; `0` disables it. The first relevant request binds the connection to its client IP. |
| `DOH_MAX_BODY_BYTES` | `4096` | Maximum DoH POST body / GET `dns` parameter size; capped at 65535 bytes. |
| `HEALTH_CHECK` | `true` | Enables startup and runtime upstream probing. |
| `HEALTH_TIMEOUT_MS` | `2000` | Upstream probe timeout, milliseconds (must be > 0). A probe opens a cold TLS connection, so leave room for a few round trips from distant regions. |
| `HEALTH_INTERVAL` | `60` | Runtime probe interval, seconds. Three cold DoH probes run once per interval and keep the state bounded. |
| `HEALTH_FAILS_TO_SWITCH` | `2` | Consecutive active-upstream failures before switching. |
| `HEALTH_RESTART_COOLDOWN` | `120` | Minimum seconds between supervisor-triggered upstream-order restarts. An actively failed upstream is still evacuated without waiting for this cooldown. |
| `HEALTH_EWMA_ALPHA` | `0.35` | Probe-latency EWMA smoothing factor. |
| `HEALTH_FAILURE_PENALTY_MS` | `1500` | Score penalty per failed probe. |
| `HEALTH_SWITCH_MARGIN_PCT` | `0.20` | Relative score improvement needed for a healthy switch. |
| `HEALTH_SWITCH_MARGIN_MS` | `25` | Absolute score improvement needed for a healthy switch. |
| `HEALTH_STATE_FILE` | `/tmp/mosdns-upstream-state.tsv` | Probe state file. |
| `HEALTH_BACKEND_TIMEOUT_MS` | `1000` | Proxy health-check TCP timeout, milliseconds. |
| `GOMEMLIMIT` | `288MiB` | Go memory soft limit for the main MosDNS process. The DoH proxy is launched with `80MiB`, and the health-probe helper uses `32MiB`. |
| `GOMAXPROCS` | `1` | Go runtime CPU setting. |

Startup rejects out-of-range values: `PORT` and `MOSDNS_BACKEND_PORT` must be 1024-65535 and differ, `DOH_MAX_BODY_BYTES` 512-65535, `DOH_RATE_MAX_IPS` 1-4096, `CACHE_SIZE` 1024-8192, `CACHE_MAX_ENTRY_BYTES` 512-65535, `IP_CONN_LIMIT` and `GLOBAL_CONN_LIMIT` at most 65535, burst values at most 1000000, rate limits at most 1000000000, `HEALTH_INTERVAL` at least 30, and `HEALTH_RESTART_COOLDOWN` at least `HEALTH_INTERVAL`. Burst values, `GLOBAL_CONN_LIMIT`, `UPSTREAM_MAX_CONNS`, `SERVER_TIMEOUT`, `HEALTH_TIMEOUT_MS`, `HEALTH_BACKEND_TIMEOUT_MS` and `HEALTH_FAILS_TO_SWITCH` must be greater than 0. `DOH_IDLE_TIMEOUT` must be `0` or 6-3600 (`0` selects MosDNS v4.5.3's 10 s default). `IP_CONN_LIMIT` and `CACHE_DUMP_INTERVAL` may be `0` to disable, and a rate limit of `0` disables that rate limiter. Floating-point environment values accept ordinary decimal and exponent notation; integer environment values must be decimal integers. `DOH_PATH`, `CACHE_DUMP_FILE` and `HAGEZI_UPSTREAM` must not contain `__UPPERCASE__` placeholder-like text, because they are substituted into the config template.

## Deploy as an HTTP service

Build the repository with the included Dockerfile and deploy the resulting image as an HTTP-capable container service. The exact service type, health-check UI, and port configuration names vary by hosting provider; use the equivalent settings for your platform.

Configure the service to:

```text
public port: 8080 (or set PORT)
health path: /health (or set HEALTH_PATH)
DoH path:    /dns-query (or set DOH_PATH)
```

The application listens on the value of `PORT`. The MosDNS backend remains bound to loopback and must not be exposed directly.

For a different public DoH path, set for example:

```text
DOH_PATH=/my-secret-dns
```

A custom path is obscurity, not authentication. The built-in rate and connection limits remain active regardless of the path.

## Build compatibility

The project intentionally pins the MosDNS v4.5.3 build to Go 1.19.x because that MosDNS release pulls `quic-go` v0.30.0, whose build guard rejects newer Go toolchains. The runtime image is separate and contains no Go toolchain. Upgrading MosDNS or the build toolchain should be treated as a compatibility change, not a routine version bump.

## Resource profile

The configuration is intentionally small for a low-CPU instance:

- no GeoIP/Geosite downloads;
- no SQLite, Redis, dnsmasq, BIND, or separate cache process;
- one MosDNS process, one small proxy process, and one health-probe helper invoked when checks run;
- bounded RAM cache, an 8 KiB-per-entry warm snapshot, and bounded public request/connection state.

For a small instance, `CACHE_SIZE` and the rate limits should be changed only after observing actual memory, CPU, latency, and query volume. The default DoH profile is intentionally more tolerant of normal client bursts than a strict anti-abuse profile to reduce false-positive throttling on browser DNS startup bursts.

Rough memory budget on the 512 MB instance (soft Go heap targets, not hard caps): MosDNS `GOMEMLIMIT=288MiB` (about 40 MB baseline plus the cache), proxy `80MiB` (normally 10-20 MB), and a short-lived probe helper at `32MiB`. Doubling `CACHE_SIZE` roughly doubles the cache share of MosDNS memory; keep the total comfortably below the container limit. The service-wide `GLOBAL_RATE_LIMIT` is the main CPU protection: raise it only if CPU stays well below the 0.25 vCPU quota under real traffic. The default concurrency limits deliberately leave CPU headroom for MosDNS, the proxy, garbage collection, and periodic health checks rather than trading that headroom for a higher request cap.

## Repository scope

This repository is focused on a small containerized MosDNS deployment. Provider-specific deployment configuration is intentionally not included so the image can be used with different HTTP container platforms.

## License

See the repository's [LICENSE](LICENSE) file.

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for release notes.

## 🌐 Free DNS Services

High-performance DNS utilizing HaGeZi Blocklists (Multi Pro + TIF).

| Blocklist | DNS-over-HTTPS (DoH) |
| :--- | :--- |
| Multi Pro + TIF | `https://freedns.koyeb.app/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dns-pi.vercel.app/api/doh/dns-query` (Recommended) |
| Multi Pro + TIF | `https://dnssix.netlify.app/api/doh/dns-query` |
| Multi Pro + TIF | `https://dns-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not use in 15 minute) |
| Multi Pro + TIF | `https://doh-93aca.containers.snapdeploy.app/dns-query` (Recommended, but will sleep if not use in 15 minute) |

---

# ⚡ Bandwidth Hero Server

A lightweight image optimization proxy designed to slash bandwidth usage and accelerate web browsing.

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to the client. This significantly reduces data consumption while improving page load performance.

🖥️ **Live Demo:** [Bandwidth Hero](https://bhserv.netlify.app/).

## Supporting the Project

If you find this project useful, donations are appreciated:
- **Bitcoin**: `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`
