# MosDNS on PaaS

A small, DoH-only [MosDNS v4.5.3](https://github.com/IrineSistiana/mosdns/tree/v4.5.3) service for an HTTP Web Service. The image is tuned for a deployment budget of **512 MB RAM, 0.1 vCPU, and 2 GB SSD**.

The container keeps the public surface deliberately narrow:

- only HTTP DoH and the minimal health endpoint are exposed publicly;
- three HaGeZi DoH endpoints are used in strict sequential order;
- the upstream health probe uses the same pinned IPv4 addresses as the MosDNS `dial_addr` configuration for the built-in endpoints;
- the DNS cache is bounded in memory and has a separate bounded warm-start snapshot;
- a small Go proxy applies accept-time/source-aware connection ceilings, request-size bounds, and I/O timeouts before MosDNS;
- MosDNS and the proxy are supervised so a failed process causes the container to exit and lets the hosting platform restart the service.

## Request path

```text
client
  │ DoH GET/POST
  ▼
ip-conn-proxy :8080
  │ connection / size / timeout guards
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

The probe helper maintains an EWMA latency score and consecutive-failure count in `HEALTH_STATE_FILE`. Runtime checks happen every `HEALTH_INTERVAL` seconds. Hysteresis prevents a small latency difference from causing repeated upstream swaps, while repeated failures can trigger a reorder and MosDNS restart. Each successful probe must contain a complete, structurally valid DNS response for the probe question; header-only or truncated HTTP bodies are rejected. When a switch happens, the complete measured order from the probe (not just the new first entry) becomes the new failover order. With a fixed `https://...` endpoint, latency never demotes it: it is moved down only after it has failed `HEALTH_FAILS_TO_SWITCH` probes in a row, and the supervisor moves it back to first (subject to `HEALTH_RESTART_COOLDOWN`) once it probes healthy again.

The three built-in IP variables are validated as IPv4 addresses before configuration is rendered. A custom `HAGEZI_UPSTREAM` is not pinned by these variables and is resolved normally. The bundled pins currently correspond to the three configured HaGeZi resolver hostnames.

## Browser strict-DoH profile

The default profile is tuned for Chrome and Firefox strict DoH/maximum-protection modes, where browsers may create short connection bursts, open or close connections during navigation, and cancel in-flight requests when a page or network state changes. The defaults are:

```text
GLOBAL_CONN_LIMIT=128
IP_CONN_LIMIT=64
DOH_MAX_BODY_BYTES=4096
UPSTREAM_MAX_CONNS=4
SERVER_TIMEOUT=6
```

There is deliberately no per-request token-bucket rate limiter. Request-rate buckets add per-request locking and state while also throttling legitimate bursts when many devices share one public IP. Resource protection instead comes from a global accept-time connection ceiling, a source-IP concurrency ceiling, strict request-size limits, bounded backend connection pools, timeouts, and bounded DNS response validation. This keeps burst handling responsive without allowing unbounded socket or in-flight work.

A normal client disconnect is expected behavior and should not be logged or treated as an upstream failure. A backend timeout or invalid DNS response is different: it is converted to `502 Bad Gateway`, allowing the browser's strict DoH implementation to retry or report the lookup failure rather than receiving a truncated or malformed `200 OK` response. `SERVER_TIMEOUT` is the MosDNS query deadline; the sequential failover plugin shares that deadline across the remaining configured upstream attempts.

The source-IP connection guard is keyed only by the trusted client identity from the final `X-Forwarded-For` value (or the direct peer when no forwarding header is supplied), so arbitrary `Host` headers cannot create additional state. Multiple devices behind the same public IP intentionally share the connection ceiling. The default of 64 is below the global 128-connection ceiling, so one shared-IP group cannot consume the entire service.

The persisted probe state is intentionally bounded. Oversized or malformed state files are ignored, and only a small number of valid entries are loaded, so a damaged state file cannot consume unbounded startup memory.

## Warm cache

The normal query path uses MosDNS's memory cache. A small custom backend keeps a second, bounded copy for warm starts and writes it atomically to disk at `CACHE_DUMP_INTERVAL`. Hot-cache inserts are capped by `CACHE_MAX_ENTRY_BYTES` (4 KiB by default). The warm copy has its own fixed 4 KiB per-entry ceiling that is never raised above 4 KiB even if `CACHE_MAX_ENTRY_BYTES` is configured higher; a larger `CACHE_MAX_ENTRY_BYTES` only widens what the in-memory hot cache accepts; entries between 4 KiB and the configured value are still served normally but are never written to the warm snapshot. This keeps the on-disk snapshot small and prevents unusually large DNS responses from multiplying across the cache on the 512 MB instance. The default 8192 entries are retained because the per-entry ceiling, not the entry count alone, is the main protection against large-response amplification. Warm metadata keeps insertion/restore recency for snapshot eviction and restoration; ordinary hot-cache hits stay on the fast inner-cache path and do not take the warm-metadata lock. The snapshot is limited to the same warm-entry set, and expired or oversized entries are discarded before restore.

Local service storage may be ephemeral, so the warm cache is an optimization only. DNS correctness does not depend on the snapshot being present after a service replacement. If the directory of `CACHE_DUMP_FILE` cannot be created, startup logs a warning and continues instead of aborting. `CACHE_DUMP_INTERVAL=0` disables snapshot writes, including the shutdown snapshot, while existing snapshot data can still be restored at startup.

With the default 512 MB / 0.1 vCPU profile, `CACHE_SIZE=8192`, `CACHE_MAX_ENTRY_BYTES=4096`, `GLOBAL_CONN_LIMIT=128`, `IP_CONN_LIMIT=64`, and `UPSTREAM_MAX_CONNS=4`. No request-rate bucket is allocated for each client or globally. The proxy instead limits concurrent sockets/in-flight backend work, keeps the backend on loopback, bounds request and response sizes, and uses short connection/read/write timeouts. The proxy is kept at `GOMEMLIMIT=64MiB` while MosDNS gets `GOMEMLIMIT=288MiB`; these are soft Go heap targets, not hard container-memory caps, so actual RSS also includes runtime/native memory and buffers. Successful backend DoH responses are buffered and DNS-validated before forwarding, with a 65535-byte response ceiling, so an unknown-length or prematurely terminated backend body cannot reach the client as a partial HTTP 200.

## DoH proxy

`content/ip_conn_proxy.go` sits in front of MosDNS and enforces:

```text
connections:      GLOBAL_CONN_LIMIT (accept-time atomic cap; default 128)
per-IP conn:      IP_CONN_LIMIT (connection/accounting key follows the request client IP; default 64)
request size:     DOH_MAX_BODY_BYTES (default 4096 bytes)
backend pool:     fixed loopback connection pool (16 max per backend)
response wait:    8 seconds before returning 502
```

There is no request-rate token bucket. The fixed sharded connection table has bounded spare slots (up to 2x the global connection ceiling, hard-capped at 65535), reducing same-shard collisions without maintaining an attacker-growable rate-state table. The global TCP connection cap is enforced at accept time; the per-source-IP cap is charged from the request's trusted client IP rather than the socket peer, so many clients behind one shared edge address are deliberately counted together. Koyeb's Edge Network sets the standard `x-forwarded-for` header and appends the address used to connect to Koyeb; its documentation identifies the final IP as the only certifiable client address, which matches this proxy's identity rule. If a keep-alive edge connection changes client identity, its single slot is rebound to the latest client and is refused only when that client is already at `IP_CONN_LIMIT`. Invalid or missing client identity is rejected rather than pooled into a shared bucket. Waiting for MosDNS response headers is capped at 8 seconds, so a hung backend cannot occupy a proxy backend connection indefinitely.

Only RFC 8484-style GET and POST requests are accepted at `DOH_PATH`. POST requests require `Content-Type: application/dns-message`. Requests that are too large, malformed, or use another method are rejected before being sent to MosDNS. The proxy removes client-controlled forwarding headers before proxying.

The health endpoint is a separate `GET`/`HEAD` path. It checks that the MosDNS backend TCP listener is reachable and returns `200 OK` when it is. Health checks have no separate rate bucket; they are cheap socket checks, inherit the same global connection protection, and use a bounded 1-second default backend dial timeout.

The repository is intentionally DoH-only and does not expose a raw DNS UDP/TCP listener. The public proxy accepts only the DoH HTTP surface and keeps MosDNS on loopback; raw DNS framing helpers are not carried in the runtime build because they are unused by this deployment.

## Environment variables

The image has working defaults (defined once, in `content/entrypoint.sh`); no environment variable is required for the default deployment. The public proxy is the externally reachable resource guard; MosDNS is kept behind its loopback listener. The proxy backend is forced to `127.0.0.1:<MOSDNS_BACKEND_PORT>` so deployment variables cannot introduce a hostname-based system-DNS path. The build also patches the v4.5.3 DoH client context boundary so request cancellation reaches the underlying transport during sequential failover. The three HaGeZi candidates are the complete policy path; there is no separate emergency endpoint.

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
| `CACHE_MAX_ENTRY_BYTES` | `4096` | Maximum packed DNS response size eligible for the hot cache. Larger responses are served normally but are not cached. |
| `CACHE_DUMP_FILE` | `/var/cache/mosdns/cache.dump` | Warm-cache snapshot path. |
| `CACHE_DUMP_INTERVAL` | `3300` | Warm-cache snapshot interval, seconds. |
| `SERVER_TIMEOUT` | `6` | MosDNS query timeout, seconds. Valid range is 1-10 s. The sequential failover budget is divided across the remaining upstream attempts, so 6 s bounds the total DNS wait while still allowing all three configured upstreams to be tried. |
| `UPSTREAM_IDLE_TIMEOUT` | `60` | Upstream idle connection timeout, seconds. |
| `UPSTREAM_MAX_CONNS` | `4` | Maximum upstream connections per endpoint. Kept small for 0.1 vCPU while still allowing a few concurrent misses and retries. |
| `DOH_IDLE_TIMEOUT` | `120` | Internal MosDNS DoH listener idle timeout, seconds. `0` uses MosDNS v4.5.3's 10 s default. Valid explicit values are 6-3600 s. The proxy keeps pooled backend connections at least 5 s shorter, capped at 90 s. |
| `GLOBAL_CONN_LIMIT` | `128` | Maximum concurrent public TCP connections; rejected sockets are closed at accept time. The fixed per-source connection table has bounded spare metadata and is hard-capped at 65535 slots. |
| `IP_CONN_LIMIT` | `64` | Per-client concurrent connection limit; `0` disables it. The bounded value protects the 0.1 vCPU instance from one source monopolizing connection/in-flight resources while allowing larger NAT/CGNAT groups to connect concurrently. |
| `DOH_MAX_BODY_BYTES` | `4096` | Maximum DoH POST body / GET `dns` parameter size; capped at 65535 bytes. |
| `HEALTH_CHECK` | `true` | Enables startup and runtime upstream probing. |
| `HEALTH_TIMEOUT_MS` | `2000` | Upstream probe timeout, milliseconds (must be > 0). A probe opens a cold TLS connection, so leave room for a few round trips from distant regions. |
| `HEALTH_INTERVAL` | `300` | Runtime probe interval, seconds. Three cold DoH probes run once every five minutes to avoid recurring CPU/TLS spikes on the 0.1 vCPU instance. |
| `HEALTH_FAILS_TO_SWITCH` | `2` | Consecutive active-upstream failures before switching. |
| `HEALTH_RESTART_COOLDOWN` | `600` | Minimum seconds between supervisor-triggered upstream-order restarts. This reduces restart churn on a very small CPU quota while preserving failover on repeated active failures. |
| `HEALTH_EWMA_ALPHA` | `0.35` | Probe-latency EWMA smoothing factor. |
| `HEALTH_FAILURE_PENALTY_MS` | `1500` | Score penalty per failed probe. |
| `HEALTH_SWITCH_MARGIN_PCT` | `0.20` | Relative score improvement needed for a healthy switch. |
| `HEALTH_SWITCH_MARGIN_MS` | `25` | Absolute score improvement needed for a healthy switch. |
| `HEALTH_STATE_FILE` | `/tmp/mosdns-upstream-state.tsv` | Probe state file. |
| `HEALTH_BACKEND_TIMEOUT_MS` | `1000` | Proxy health-check TCP timeout, milliseconds. |
| `GOMEMLIMIT` | `288MiB` | Go memory soft limit for the main MosDNS process. The DoH proxy is launched with `64MiB`, and the health-probe helper uses `32MiB`. |
| `GOMAXPROCS` | `1` | Go runtime CPU setting. |

Startup rejects out-of-range values: `PORT` and `MOSDNS_BACKEND_PORT` must be 1024-65535 and differ, `DOH_MAX_BODY_BYTES` 512-65535, `CACHE_SIZE` 1024-8192, `CACHE_MAX_ENTRY_BYTES` 512-65535, `IP_CONN_LIMIT` and `GLOBAL_CONN_LIMIT` at most 65535, `SERVER_TIMEOUT` 1-10, `GOMAXPROCS` 1-2, `HEALTH_INTERVAL` at least 30, and `HEALTH_RESTART_COOLDOWN` at least `HEALTH_INTERVAL`. `GLOBAL_CONN_LIMIT`, `UPSTREAM_MAX_CONNS`, `HEALTH_TIMEOUT_MS`, `HEALTH_BACKEND_TIMEOUT_MS` and `HEALTH_FAILS_TO_SWITCH` must be greater than 0. `DOH_IDLE_TIMEOUT` must be `0` or 6-3600 (`0` selects MosDNS v4.5.3's 10 s default). `IP_CONN_LIMIT` and `CACHE_DUMP_INTERVAL` may be `0` to disable. Floating-point environment values accept ordinary decimal and exponent notation; integer environment values must be decimal integers. `DOH_PATH`, `HEALTH_PATH`, `CACHE_DUMP_FILE`, `HEALTH_STATE_FILE`, and `GOMEMLIMIT` are validated as printable text, and the template-substituted values must not contain `__UPPERCASE__` placeholder-like text.

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

A custom path is obscurity, not authentication. The built-in connection, body-size, timeout, and response-validation guards remain active regardless of the path.

## Build compatibility

The project intentionally pins the MosDNS v4.5.3 build to Go 1.19.x because that MosDNS release pulls `quic-go` v0.30.0, whose build guard rejects newer Go toolchains. The runtime image is separate and contains no Go toolchain. Upgrading MosDNS or the build toolchain should be treated as a compatibility change, not a routine version bump.

## Resource profile

The configuration is intentionally small for a low-CPU instance:

- no GeoIP/Geosite downloads;
- no SQLite, Redis, dnsmasq, BIND, or separate cache process;
- one MosDNS process, one small proxy process, and one health-probe helper invoked when checks run;
- bounded RAM cache, a 4 KiB-per-entry warm snapshot, and bounded public connection state.

For a small instance, `CACHE_SIZE`, connection ceilings, and timeouts should be changed only after observing actual memory, CPU, latency, and query volume. The default DoH profile is intentionally tolerant of normal client bursts and connection churn: `GLOBAL_CONN_LIMIT=128` and `IP_CONN_LIMIT=64` provide bounded concurrency without a request-rate quota that would reject short browser bursts. `SERVER_TIMEOUT=6` bounds a stalled DNS lookup; sequential failover shares that budget across the remaining configured upstreams. Strict DoH clients should still be expected to cancel in-flight requests during normal navigation or network changes; those client disconnects are not treated as upstream failures.

Rough memory budget on the 512 MB instance (soft Go heap targets, not hard caps): MosDNS `GOMEMLIMIT=288MiB` (about 40 MB baseline plus the cache), proxy `64MiB` (normally substantially below its soft limit), and a short-lived probe helper at `32MiB`. Doubling `CACHE_SIZE` roughly doubles the cache share of MosDNS memory; keep the total comfortably below the container limit. With no request-rate buckets, the main CPU controls are bounded connection/in-flight counts, short I/O deadlines, small upstream pools, and infrequent health probes. These defaults leave room for MosDNS, the proxy, garbage collection, and platform overhead while allowing many clients—including many devices behind the same public/NAT IP—to share the service.

## Repository scope

This repository is focused on a small containerized MosDNS deployment. Provider-specific deployment configuration is intentionally not included so the image can be used with different HTTP container platforms.

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
