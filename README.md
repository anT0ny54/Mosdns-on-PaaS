# MosDNS on Koyeb

A lightweight Koyeb deployment of MosDNS v4.5.3 with DNS-over-HTTPS (DoH), three rotating HaGeZi upstreams, and a bounded warm-start cache.

## Optimized for Koyeb 512 MB / 0.1 vCPU

The configuration is intentionally conservative:

- MosDNS v4.5.3
- one active DoH upstream at a time
- per-query sequential failover across all three HaGeZi endpoints
- three HaGeZi endpoints rotated every 30 minutes by default
- deterministic round-robin rotation (`rotate`) so all three endpoints are used without random repeats
- DNS pipelining enabled
- 8,192-entry RAM cache by default
- bounded disk warm-cache snapshot
- warm cache survives graceful rotation/restart as long as Koyeb's local storage remains available
- no GeoIP/Geosite downloads
- no additional resident cache/database process
- non-root runtime user
- Go memory limit of 384 MiB
- 3-second DNS server timeout

The warm-cache file is only a warm-start optimization. Koyeb local storage is ephemeral, so a new/replaced instance may still start without the previous snapshot.

## Environment variables

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | Koyeb HTTP port |
| `DOH_PATH` | `/dns-query` | DoH endpoint path |
| `CACHE_SIZE` | `8192` | Maximum RAM cache entries |
| `CACHE_DUMP_FILE` | `/var/cache/mosdns/cache.dump` | Warm-cache snapshot |
| `CACHE_DUMP_INTERVAL` | `900` | Snapshot interval in seconds |
| `MAX_QPS` | `20` | Per-client QPS limit |
| `HAGEZI_UPSTREAM` | `rotate` | `rotate`, `random`, or one fixed `https://` endpoint |
| `ROTATE_INTERVAL` | `1800` | Rotation interval in seconds; minimum 60 |
| `GOMEMLIMIT` | `384MiB` | Go runtime memory limit |

## Sequential failover

Each running MosDNS instance has an ordered upstream chain:

```text
selected upstream -> second upstream -> third upstream
```

The selected upstream is tried first. The second upstream is started only when
the first fails, and the third is tried only when the second also fails. This is
implemented with nested v4.5.3 `fallback` execution blocks rather than sending
all three queries in parallel.

This matters because `fast_forward` with multiple upstreams is a parallel
mechanism in MosDNS v4.5.3; it is not a sequential failover mechanism. The
project therefore uses three one-upstream `fast_forward` plugins and composes
them with nested fallback nodes.

A successful upstream stops the chain, so healthy queries normally use only
one upstream. The failover path is primarily for transport/server failures.
A valid DNS response such as NXDOMAIN is not treated as a transport failure.

## Three-upstream rotation

`HAGEZI_UPSTREAM=rotate` uses these three endpoints in order:

1. `https://root.hagezi.org/dns-query`
2. `https://wurzn.hagezi.org/dns-query`
3. `https://juuri.hagezi.org/dns-query`

After the third endpoint, it returns to the first. This is preferable to pure random selection when the requirement is to continuously rotate among exactly three upstreams because random selection can choose the same endpoint repeatedly.

The active MosDNS process is gracefully stopped at each rotation. The next process rebuilds the failover order with the newly selected upstream first. The warm-cache backend writes a final bounded snapshot during shutdown and the next process reloads it. The RAM cache therefore does not intentionally start empty after a normal rotation, provided the local cache file is still present.

If you prefer random selection, set:

```text
HAGEZI_UPSTREAM=random
```

For no rotation, set one fixed endpoint and the supervisor exits after MosDNS starts normally:

```text
HAGEZI_UPSTREAM=https://root.hagezi.org/dns-query
```

## Warm cache

The hot path is always the RAM cache. The custom cache backend maintains a bounded second copy for warm starts.

Default:

```text
CACHE_SIZE=8192
CACHE_DUMP_INTERVAL=900
CACHE_DUMP_FILE=/var/cache/mosdns/cache.dump
```

The cache uses atomic replacement when writing the snapshot. Expired entries are not restored. The warm cache is deliberately bounded to the same configured maximum as the RAM cache so it cannot grow without limit.

## Why the configuration is small

On a 0.1-vCPU instance, avoiding unnecessary resident processes and large databases matters more than maximizing cache size. The deployment therefore does not install GeoIP/Geosite data, SQLite, dnsmasq, BIND, or another cache daemon.

8,192 entries is a good starting point for 512 MB RAM. If measurements show the cache is too small, increase `CACHE_SIZE`; otherwise leave it unchanged.

## DoH security

A public DoH service can be abused. `MAX_QPS` is rate limiting, not authentication. Keep the DoH URL private and consider additional access control if the endpoint is intended only for personal use.

The service expects Koyeb's proxy to provide the client address through `X-Forwarded-For` for the client limiter.


## Requirements

- A [Koyeb](https://www.koyeb.com/) account
- A GitHub repository containing this project
- A domain provided by Koyeb or a custom domain
- Dockerfile support enabled for the service

## Deploy to Koyeb

### Using the Koyeb dashboard

1. Sign in to your Koyeb account.
2. Create a new **Web Service**.
3. Select the GitHub repository containing this project.
4. Choose the **Dockerfile** builder.
5. Expose port `8080` using the HTTP protocol.
6. Add the following environment variable:

   ```text
   DOH_PATH=/dns-query
   ```

7. Deploy the service.

Koyeb provides the `PORT` environment variable automatically. If no port is configured explicitly, Koyeb uses the lowest port exposed by the Dockerfile.

This project exposes port `8080` by default.

### Using the Koyeb CLI

```bash
koyeb app init mosdns \
  --git github.com/YOUR_USERNAME/YOUR_REPOSITORY \
  --git-branch main \
  --git-builder docker \
  --ports 8080:http \
  --routes /:8080 \
  --env DOH_PATH=/dns-query \
  --checks 8080:tcp
```

Replace the following values:

- `YOUR_USERNAME` with your GitHub username
- `YOUR_REPOSITORY` with your repository name

The TCP health check is suitable for this service because the DoH endpoint is not a regular web page.

## Environment variables

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | Port used by Koyeb. Usually provided automatically. |
| `DOH_PATH` | `/dns-query` | Path used by the DNS-over-HTTPS endpoint. |


## Disk-assisted warm cache

The deployment keeps the hot DNS cache in RAM and maintains a bounded warm-start
snapshot on local disk. This is intentionally retained because the service uses
graceful restarts during upstream rotation.

The default settings are conservative for a Koyeb instance with 512 MB RAM,
0.1 vCPU, and 2 GB local SSD:

- `CACHE_SIZE=8192` — maximum in-memory cache entries.
- `CACHE_DUMP_FILE=/var/cache/mosdns/cache.dump` — local warm-cache snapshot.
- `CACHE_DUMP_INTERVAL=900` — snapshot every 15 minutes.
- `GOMEMLIMIT=384MiB` — Go runtime memory limit.

The RAM cache is always the hot path. The disk snapshot is only a warm-start
optimization and does not participate in normal query serving.

### Rotation and warm-cache behavior

With the default `HAGEZI_UPSTREAM=rotate`, the supervisor rotates through all
three configured HaGeZi endpoints every 30 minutes:

1. root.hagezi.org
2. wurzn.hagezi.org
3. juuri.hagezi.org

Rotation is deterministic round-robin, so the same endpoint is not selected
repeatedly by chance. Before each rotation, MosDNS is stopped gracefully so
the warm-cache backend can write its final bounded snapshot. The next MosDNS
process then reloads that snapshot.

This means a normal upstream rotation does not intentionally start from an
empty cache.

Koyeb local storage is ephemeral, however. A new or replaced instance may not
have the previous snapshot, so the service must never depend on the warm-cache
file for correctness.

The snapshot is written using atomic replacement, and expired entries are not
restored. The warm-cache copy is bounded to the configured cache size so it
cannot grow without limit.

### Recommended tuning

For this 512 MB / 0.1 vCPU profile, start with:

```text
CACHE_SIZE=8192
CACHE_DUMP_INTERVAL=900
MAX_QPS=20
GOMEMLIMIT=384MiB
```

If memory pressure is observed, reduce `CACHE_SIZE` to `4096` before increasing
other resource limits. If disk-write frequency is more important than restart
warmth, increase `CACHE_DUMP_INTERVAL` to `1800`.

## Configure a custom DoH path

The default DoH path is:

```text
/dns-query
```

To use a custom path, set the `DOH_PATH` environment variable:

```text
DOH_PATH=/my-secret-dns
```

For example:

```bash
koyeb app init mosdns \
  --git github.com/YOUR_USERNAME/YOUR_REPOSITORY \
  --git-branch main \
  --git-builder docker \
  --ports 8080:http \
  --routes /:8080 \
  --env DOH_PATH=/my-secret-dns \
  --checks 8080:tcp
```

After deployment, the endpoint will be available at:

```text
https://YOUR-KOYEB-DOMAIN/my-secret-dns
```

### Security recommendation

Keep your DoH path private and avoid sharing it publicly. A publicly accessible DoH resolver may be abused by third parties, which can increase bandwidth usage and service costs.

For stronger access control, consider placing the service behind an authentication layer or restricting access through a private network.

## DoH endpoint

With the default configuration, the endpoint is:

```text
https://YOUR-KOYEB-DOMAIN/dns-query
```

Replace `YOUR-KOYEB-DOMAIN` with the hostname assigned to your Koyeb service.

You can configure this endpoint in any DNS client that supports DNS-over-HTTPS.

### Example client configuration

```text
https://YOUR-KOYEB-DOMAIN/dns-query
```

If you selected a custom path, replace `/dns-query` with your configured path.

## Upstream resolvers

MosDNS uses the following HaGeZi DNS-over-HTTPS upstream resolvers:

- `https://root.hagezi.org/dns-query`
- `https://wurzn.hagezi.org/dns-query`
- `https://juuri.hagezi.org/dns-query`

The upstreams are configured as trusted resolvers with DNS pipelining enabled.

## Free DNS services

The following public DNS-over-HTTPS services use HaGeZi blocklists, including Multi Pro and TIF.

| Service | DNS-over-HTTPS endpoint |
| --- | --- |
| Recommended | `https://freedns.koyeb.app/dns-query` |
| Recommended | `https://freedns-six.vercel.app/api/doh/dns-query` |
| Alternative | `https://dnssix.netlify.app/api/doh/dns-query` |

Public services may have usage limits, performance differences, or availability changes. Use them at your own discretion.

## Health checks

Koyeb is configured to use a TCP health check on port `8080`:

```text
8080:tcp
```

This verifies that the service is accepting connections without requiring the DoH endpoint to behave like a normal web page.

## Project scope

This repository is intentionally focused on running MosDNS on Koyeb.

The following components are not included:

- GeoIP database installation
- Geosite database installation
- GeoIP or Geosite files
- External geodata downloads
- Deployment files for other PaaS providers
- Unnecessary build and runtime dependencies

## Bandwidth Hero Server

[Bandwidth Hero Server](https://github.com/ayastreb/bandwidth-hero) is a lightweight image optimization proxy designed to reduce bandwidth usage and improve browsing performance.

It fetches remote images, compresses them on the fly, and delivers optimized versions to clients.

**Live demo:** [bhserv.netlify.app](https://bhserv.netlify.app/)

## Supporting the project

If you find this project useful, donations are appreciated.

**Bitcoin:**

```text
1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP
```

## License

See the repository's license file for licensing information.


## Adaptive upstream health scoring

The optimized build adds a tiny, statically linked `mosdns-probe` helper. At
startup/rotation it sends a minimal DNS-over-HTTPS query to each configured
upstream in parallel and ranks them by a simple health score:

- healthy: measured request latency in milliseconds
- failed: score `10000`, which pushes the endpoint to the end of the failover chain
- in `rotate`/`random` mode, the selected healthy endpoint remains preferred so
  rotation semantics are preserved without ignoring a dead resolver
- in fixed-endpoint mode, the fixed endpoint remains first when healthy; if it
  is down, the two healthy HaGeZi endpoints are tried first and the failed
  fixed endpoint becomes the final fallback

This is deliberately lightweight: the probe runs only at configuration
rotation/restart rather than once per DNS query, so it adds no steady-state
CPU cost to the request path.

## Timeout and connection tuning

The optimized defaults are:

```text
UPSTREAM_TIMEOUT=2
UPSTREAM_IDLE_TIMEOUT=15
SERVER_TIMEOUT=5
HEALTH_TIMEOUT_MS=1200
```

The 2-second per-upstream timeout reduces long stalls while still allowing a
normal DoH connection to complete. The 5-second server budget leaves enough
time for sequential failover without making ordinary failures hang for the
previous 3-second default. HTTP connection reuse is retained with a short
15-second idle lifetime and one pooled connection per upstream, which keeps
resident memory and idle sockets small on a 0.1-vCPU instance.

## Cache / memory tuning

The default RAM cache is reduced from 8192 to 4096 entries. This is a safer
512-MiB baseline because DNS cache entries can vary substantially in memory
cost. The warm snapshot remains bounded to the same cache size. The lazy cache
window is 12 hours and the stale reply TTL is 20 seconds, reducing long-lived
cache metadata while retaining useful warm-start behavior.

Recommended starting environment:

```text
CACHE_SIZE=4096
CACHE_DUMP_INTERVAL=900
GOMEMLIMIT=384MiB
MAX_QPS=20
```

If measurements show high cache churn, increase `CACHE_SIZE` to 8192 before
increasing the Go memory limit. If memory pressure appears, keep 4096 or reduce
it to 2048.

## Adaptive v5

This build adds failure-aware, smoothed upstream selection while preserving strictly sequential failover.

- EWMA latency smoothing prevents one noisy probe from causing unnecessary switching.
- Consecutive failures add a strong score penalty and move failed endpoints behind healthy ones.
- Hysteresis prevents upstream churn unless the alternative is materially better.
- The selected failover order is now the same order actually written into the runtime MosDNS config.
- `rotate` remains adaptive: it rotates away from the current endpoint when another endpoint is meaningfully better, rather than blindly switching every interval.
- `random` only randomizes among near-equal healthy endpoints.
- Health state is kept in `/tmp/mosdns-upstream-state.tsv` and remains bounded to the small configured upstream set.
- No unsupported `timeout` field is added to `fast_forward`; MosDNS v4.5.3 uses its built-in transport timeout behavior.

Tunable environment variables: `HEALTH_EWMA_ALPHA` (default `0.35`), `HEALTH_FAILURE_PENALTY_MS` (default `1500`), `HEALTH_SWITCH_MARGIN_PCT` (default `0.20`), and `HEALTH_SWITCH_MARGIN_MS` (default `25`).

