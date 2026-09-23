# MosDNS on Koyeb

A small, DoH-only [MosDNS v4.5.3](https://github.com/IrineSistiana/mosdns/tree/v4.5.3) service for a Koyeb Web Service. The image is tuned for a deployment budget of **512 MB RAM, 0.25 vCPU, and 2 GB SSD**.

The container keeps the public surface deliberately narrow:

- only HTTP DoH and the minimal health endpoint are exposed publicly;
- three HaGeZi DoH endpoints are used in strict sequential order;
- the upstream health probe uses the same pinned IPv4 addresses as the MosDNS `dial_addr` configuration for the built-in endpoints;
- the DNS cache is bounded in memory and has a bounded warm-start snapshot;
- a small Go proxy applies accept-time and source-aware request-rate, connection, and request-size limits before MosDNS;
- MosDNS and the proxy are supervised so a failed process causes the container to exit and lets Koyeb restart the Instance.

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

The custom `sequential_forward` plugin tries one upstream at a time. The next upstream is contacted only when the previous `ExchangeContext` returns an error. Each attempt gets an equal share of the time left before the `SERVER_TIMEOUT` deadline (the last upstream keeps the remainder), so a stalled upstream cannot consume the whole deadline and prevent failover. A valid DNS response, including a DNS error response such as `NXDOMAIN` or `SERVFAIL`, is returned immediately rather than causing another upstream query.

This is intentionally different from MosDNS's parallel `fast_forward` behavior: normal traffic should produce one upstream request, with later endpoints reserved for transport/upstream exchange failures.

## Upstream selection and health checks

`HAGEZI_UPSTREAM` controls the initial and runtime ordering:

| Value | Behavior |
| --- | --- |
| `rotate` | Probe all three built-ins and place the healthiest result first. |
| `random` | Probe all three built-ins and randomly rotate among near-equal healthy candidates. |
| `https://...` | Prefer this endpoint (a custom one takes the first built-in slot; a built-in URL is simply moved first) and keep the other two as fallbacks. It stays first while it passes its health probe; the probe only reorders the fallbacks. |

The probe helper maintains an EWMA latency score and consecutive-failure count in `HEALTH_STATE_FILE`. Runtime checks happen every `HEALTH_INTERVAL` seconds. Hysteresis prevents a small latency difference from causing repeated upstream swaps, while repeated failures can trigger a reorder and MosDNS restart. When a switch happens, the complete measured order from the probe (not just the new first entry) becomes the new failover order. With a fixed `https://...` endpoint, latency never demotes it: it is moved down only after it has failed `HEALTH_FAILS_TO_SWITCH` probes in a row, and the supervisor moves it back to first (subject to `HEALTH_RESTART_COOLDOWN`) once it probes healthy again.

The three built-in IP variables are validated as IPv4 addresses before configuration is rendered. A custom `HAGEZI_UPSTREAM` is not pinned by these variables and is resolved normally.

## Warm cache

The normal query path uses MosDNS's memory cache. A small custom backend keeps a second, bounded copy for warm starts and writes it atomically to disk at `CACHE_DUMP_INTERVAL`. The warm copy is limited by both `CACHE_SIZE` and an 8 KiB per-entry cap, which keeps duplicate cached response data bounded on the 512 MB instance. Warm metadata keeps insertion/restore recency for snapshot eviction and restoration; ordinary hot-cache hits stay on the fast inner-cache path and do not take the warm-metadata lock. The snapshot is limited to the same warm-entry set, and expired or oversized entries are discarded before restore.

Koyeb local storage is ephemeral, so the warm cache is an optimization only. DNS correctness does not depend on the snapshot being present after an Instance replacement. If the directory of `CACHE_DUMP_FILE` cannot be created, startup logs a warning and continues instead of aborting.

## DoH proxy

`content/ip_conn_proxy.go` sits in front of MosDNS and enforces:

```text
per-IP rate:      DOH_RATE_LIMIT / second, burst DOH_RATE_BURST
tracked peers:    DOH_RATE_MAX_IPS
service rate:     GLOBAL_RATE_LIMIT / second, burst GLOBAL_RATE_BURST
health rate:      HEALTH_RATE_LIMIT / second, burst HEALTH_RATE_BURST
health aggregate: GLOBAL_HEALTH_RATE_LIMIT / second, burst GLOBAL_HEALTH_RATE_BURST
connections:      GLOBAL_CONN_LIMIT (accept-time atomic cap)
per-IP conn:      IP_CONN_LIMIT (bound to the client IP on the first DoH/health request)
source state:     DOH_RATE_MAX_IPS (fixed-size sharded table, hard cap 512)
request size:     DOH_MAX_BODY_BYTES
```

Each DoH request is checked against the per-client rate limit first and the global limit second, so one abusive client cannot use up the shared budget. Only RFC 8484-style GET and POST requests are accepted at `DOH_PATH`. POST requests require `Content-Type: application/dns-message`. Requests that are too large, malformed, or use another method are rejected before being sent to MosDNS.

The proxy uses the final element of the last `X-Forwarded-For` header line for client limiting and removes client-controlled forwarding headers before proxying to MosDNS. The global TCP connection cap is enforced by an atomic accept-and-close listener, while the per-source-IP cap is bound to the first relevant request on the connection so a shared Koyeb edge IP is not treated as every client. Source-IP rate/connection state is held in a fixed sharded table rather than an attacker-growable map. Waiting for MosDNS response headers is capped at 12 seconds (returned to the client as `502`), so a hung backend cannot hold every proxy-to-MosDNS connection slot.

The health endpoint is a separate `GET`/`HEAD` path. It checks that the MosDNS backend TCP listener is reachable and returns `200 OK` when it is. Health requests use their own per-client and aggregate rate budgets so public DoH traffic cannot starve platform health checks, while repeated health polling is still bounded.

The repository is intentionally DoH-only and does not expose a raw DNS UDP/TCP listener. The public proxy accepts only the DoH HTTP surface and keeps MosDNS on loopback; raw DNS framing helpers are not carried in the runtime build because they are unused by this deployment. Koyeb's current public Web Service exposure supports HTTP/HTTP2, while public TCP is provided separately through TCP Proxy; public UDP is not a Web Service protocol.


## Environment variables

The image has working defaults; no environment variable is required for the default deployment. The public proxy is the externally reachable rate limiter; MosDNS is kept behind its loopback listener and does not duplicate that per-client limiter.

| Variable | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | Public Koyeb service port. |
| `DOH_PATH` | `/dns-query` | Public DoH path. |
| `HEALTH_PATH` | `/health` | Proxy health path for an HTTP health check. |
| `MOSDNS_BACKEND_PORT` | `18080` | Loopback port between the proxy and MosDNS. |
| `HAGEZI_UPSTREAM` | `rotate` | `rotate`, `random`, or a fixed `https://` endpoint. |
| `UPSTREAM_0_IP` | `188.34.161.210` | Built-in `root.hagezi.org` dial pin. |
| `UPSTREAM_1_IP` | `159.69.155.94` | Built-in `wurzn.hagezi.org` dial pin. |
| `UPSTREAM_2_IP` | `95.217.163.17` | Built-in `juuri.hagezi.org` dial pin. |
| `CACHE_SIZE` | `4096` | Maximum cache entries in RAM and warm snapshot. |
| `CACHE_DUMP_FILE` | `/var/cache/mosdns/cache.dump` | Warm-cache snapshot path. |
| `CACHE_DUMP_INTERVAL` | `3300` | Warm-cache snapshot interval, seconds. |
| `SERVER_TIMEOUT` | `8` | MosDNS query timeout, seconds. The proxy stops waiting for MosDNS after 12 s, so values above 12 only produce a startup warning. |
| `UPSTREAM_IDLE_TIMEOUT` | `30` | Upstream idle connection timeout, seconds. |
| `UPSTREAM_MAX_CONNS` | `2` | Maximum upstream connections per endpoint. |
| `DOH_IDLE_TIMEOUT` | `120` | Internal MosDNS DoH listener idle timeout, seconds. `0` uses MosDNS v4.5.3's 10 s default. Valid explicit values are 6-3600 s. The proxy keeps pooled backend connections at least 5 s shorter, capped at 90 s. |
| `DOH_RATE_LIMIT` | `10` | Per-client request rate, requests/second. Sized to tolerate normal browser bursts while retaining abuse protection. |
| `DOH_RATE_BURST` | `24` | Per-client burst allowance. |
| `DOH_RATE_MAX_IPS` | `512` | Maximum client buckets retained. |
| `GLOBAL_RATE_LIMIT` | `80` | Global request rate, requests/second. |
| `GLOBAL_RATE_BURST` | `160` | Global DoH burst allowance. |
| `HEALTH_RATE_LIMIT` | `2` | Per-client health request rate, requests/second. |
| `HEALTH_RATE_BURST` | `4` | Per-client health burst allowance. |
| `GLOBAL_HEALTH_RATE_LIMIT` | `10` | Aggregate health request rate, requests/second. |
| `GLOBAL_HEALTH_RATE_BURST` | `20` | Aggregate health burst allowance. |
| `GLOBAL_CONN_LIMIT` | `96` | Maximum concurrent public TCP connections; rejected sockets are closed at accept time. |
| `IP_CONN_LIMIT` | `12` | Per-client concurrent connection limit; `0` disables it. The first relevant request binds the connection to its client IP. |
| `DOH_MAX_BODY_BYTES` | `4096` | Maximum DoH POST body / GET `dns` parameter size; capped at 65535 bytes. |
| `HEALTH_CHECK` | `true` | Enables startup and runtime upstream probing. |
| `HEALTH_TIMEOUT_MS` | `1200` | Upstream probe timeout, milliseconds (must be > 0). |
| `HEALTH_INTERVAL` | `300` | Runtime probe interval, seconds. |
| `HEALTH_FAILS_TO_SWITCH` | `2` | Consecutive active-upstream failures before switching. |
| `HEALTH_RESTART_COOLDOWN` | `900` | Minimum seconds between supervisor-triggered restarts. |
| `HEALTH_EWMA_ALPHA` | `0.35` | Probe-latency EWMA smoothing factor. |
| `HEALTH_FAILURE_PENALTY_MS` | `1500` | Score penalty per failed probe. |
| `HEALTH_SWITCH_MARGIN_PCT` | `0.20` | Relative score improvement needed for a healthy switch. |
| `HEALTH_SWITCH_MARGIN_MS` | `25` | Absolute score improvement needed for a healthy switch. |
| `HEALTH_STATE_FILE` | `/tmp/mosdns-upstream-state.tsv` | Probe state file. |
| `HEALTH_BACKEND_TIMEOUT_MS` | `1000` | Proxy health-check TCP timeout, milliseconds. |
| `GOMEMLIMIT` | `256MiB` | Go memory soft limit for the main MosDNS process. The DoH proxy is launched with `64MiB`, and the health-probe helper uses `32MiB`. |
| `GOMAXPROCS` | `1` | Go runtime CPU setting. |

Startup rejects out-of-range values: `PORT` and `MOSDNS_BACKEND_PORT` must be 1024-65535 and differ, `DOH_MAX_BODY_BYTES` 512-65535, `DOH_RATE_MAX_IPS` 1-512, `CACHE_SIZE` at least 1024, `HEALTH_INTERVAL` at least 30, and `HEALTH_RESTART_COOLDOWN` at least `HEALTH_INTERVAL`. Burst values, `GLOBAL_CONN_LIMIT`, `UPSTREAM_MAX_CONNS`, `SERVER_TIMEOUT`, `HEALTH_TIMEOUT_MS`, `HEALTH_BACKEND_TIMEOUT_MS` and `HEALTH_FAILS_TO_SWITCH` must be greater than 0. `DOH_IDLE_TIMEOUT` must be `0` or 6-3600 (`0` selects MosDNS v4.5.3's 10 s default). `IP_CONN_LIMIT` and `CACHE_DUMP_INTERVAL` may be `0` to disable, and a rate limit of `0` disables that rate limiter.

## Deploy to Koyeb

### Dashboard

Create a **Web Service** from the repository and use the Dockerfile builder. Expose port `8080` over HTTP.

For readiness checking, configure an HTTP health check for:

```text
port: 8080
path: /health
```

Koyeb supplies the `PORT` environment variable automatically. With the image defaults, the public DoH endpoint is:

```text
https://YOUR-KOYEB-DOMAIN/dns-query
```

### CLI

```bash
koyeb app init mosdns \
  --git github.com/YOUR_USERNAME/YOUR_REPOSITORY \
  --git-branch main \
  --git-builder docker \
  --ports 8080:http \
  --routes /:8080 \
  --checks 8080:http:/health
```

For a different public DoH path, set for example:

```text
DOH_PATH=/my-secret-dns
```

A custom path is obscurity, not authentication. The built-in rate and connection limits remain active regardless of the path.

## Resource profile

The configuration is intentionally small for a low-CPU instance:

- no GeoIP/Geosite downloads;
- no SQLite, Redis, dnsmasq, BIND, or separate cache process;
- one MosDNS process, one small proxy process, and one health-probe helper invoked when checks run;
- bounded RAM cache, an 8 KiB-per-entry warm snapshot, and bounded public request/connection state.

For a small instance, `CACHE_SIZE` and the rate limits should be changed only after observing actual memory, CPU, latency, and query volume. The default DoH profile is intentionally more tolerant of normal client bursts than a strict anti-abuse profile to reduce false-positive throttling on browser DNS startup bursts.

## Repository scope

This repository is focused on this Koyeb MosDNS deployment. Unrelated application components and deployment material are not included.

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

## License

See the repository's [LICENSE](LICENSE) file.

## Changelog

See [CHANGELOG.md](CHANGELOG.md) for release notes.
