# MosDNS on Koyeb v7.8.7

This revision is tuned for a Koyeb 512 MB / 0.1 vCPU / 2 GB SSD Free Instance serving **public DoH** while aggressively limiting abuse.

- MosDNS remains a long-running process; if MosDNS or the public proxy exits, the container exits so Koyeb can restart the Instance.
- All three HaGeZi DoH endpoints remain available through **strict sequential failover**.
- The bounded warm cache is retained and snapshots every **55 minutes (3300 seconds)**.
- Public requests pass through a lightweight Go proxy before reaching MosDNS.
- Abuse protection uses **per-IP rate limiting + global rate limiting + global connection limiting + request-size limits**.
- Per-IP connection limiting remains disabled for Firefox/Fennec compatibility; the global connection ceiling remains enabled.
- Plain UDP/TCP DNS is not exposed; upstream resolution is **DoH-only**.
- Koyeb health checks can restart unhealthy Instances. Koyeb documents that liveness health-check failures can cause an Instance restart.
- Koyeb Free Instances are limited to 512 MB RAM, 0.1 vCPU and 2 GB SSD and can scale to zero after one hour without traffic.

# MosDNS on Koyeb

A lightweight Koyeb deployment of MosDNS v4.5.3 with DNS-over-HTTPS (DoH), three HaGeZi upstreams with sequential failover, and a bounded warm-start cache.

## Optimized for Koyeb 512 MB / 0.1 vCPU

The configuration is intentionally conservative:

- MosDNS v4.5.3
- one active DoH upstream at a time
- per-query sequential failover across all three HaGeZi endpoints
- three HaGeZi endpoints kept available through sequential failover
- health-aware startup ordering; all three endpoints remain in the failover chain
- DNS pipelining enabled
- 2,048-entry RAM cache by default
- bounded disk warm-cache snapshot
- warm cache survives graceful rotation/restart as long as Koyeb's local storage remains available
- no GeoIP/Geosite downloads
- no additional resident cache/database process
- non-root runtime user
- Go memory limit of 384 MiB
- 8-second DNS server timeout

The warm-cache file is only a warm-start optimization. Koyeb local storage is ephemeral, so a new/replaced instance may still start without the previous snapshot.

## Environment variables

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | Koyeb HTTP port |
| `DOH_PATH` | `/dns-query` | DoH endpoint path |
| `CACHE_SIZE` | `2048` | Maximum RAM cache entries |
| `CACHE_DUMP_FILE` | `/var/cache/mosdns/cache.dump` | Warm-cache snapshot |
| `CACHE_DUMP_INTERVAL` | `3300` | Snapshot interval: 55 minutes |
| `MAX_QPS` | `15` | MosDNS client QPS ceiling |
| `DOH_RATE_LIMIT` | `5` | Per-IP public DoH request rate |
| `DOH_RATE_BURST` | `12` | Per-IP burst allowance |
| `DOH_RATE_MAX_IPS` | `512` | Maximum tracked client IPs |
| `GLOBAL_RATE_LIMIT` | `40` | Global public DoH request rate |
| `GLOBAL_RATE_BURST` | `80` | Global burst allowance |
| `GLOBAL_CONN_LIMIT` | `128` | Global concurrent connection ceiling |
| `DOH_MAX_BODY_BYTES` | `4096` | Maximum DoH POST body / GET dns parameter |
| `IP_CONN_LIMIT` | `0` | Per-IP connection limit; 0 preserves Firefox/Fennec reuse |
| `UPSTREAM_IDLE_TIMEOUT` | `60` | Upstream idle timeout in seconds |
| `DOH_IDLE_TIMEOUT` | `120` | Local DoH listener idle timeout |
| `HAGEZI_UPSTREAM` | `rotate` | `rotate`, `random`, or one fixed `https://` endpoint |
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

`HAGEZI_UPSTREAM=rotate` selects among these three endpoints at startup; the selected endpoint is then first in the sequential failover chain:

1. `https://root.hagezi.org/dns-query`
2. `https://wurzn.hagezi.org/dns-query`
3. `https://juuri.hagezi.org/dns-query`

The selected endpoint is preferred initially, while the other two remain immediate sequential fallbacks. There is no periodic process restart.

The active MosDNS process is not periodically restarted. The initial order is health-scored once at boot, then MosDNS continuously handles sequential failover. The warm-cache backend writes a final bounded snapshot during shutdown and the next process reloads it. The RAM cache therefore does not intentionally start empty after a normal rotation, provided the local cache file is still present.

If you prefer random ordering at startup, set:

```text
HAGEZI_UPSTREAM=random
```

For a fixed preferred endpoint, set one fixed endpoint; the two HaGeZi endpoints remain fallbacks:

```text
HAGEZI_UPSTREAM=https://root.hagezi.org/dns-query
```

## Warm cache

The hot path is always the RAM cache. The custom cache backend maintains a bounded second copy for warm starts.

Recommended v7.8.7 defaults:

```text
CACHE_SIZE=2048
CACHE_DUMP_INTERVAL=3300
CACHE_DUMP_FILE=/var/cache/mosdns/cache.dump
```

The cache uses atomic replacement when writing the snapshot. Expired entries are not restored. The warm cache is deliberately bounded to the same configured maximum as the RAM cache so it cannot grow without limit.

## Why the configuration is small

On a 0.1-vCPU instance, avoiding unnecessary resident processes and large databases matters more than maximizing cache size. The deployment therefore does not install GeoIP/Geosite data, SQLite, dnsmasq, BIND, or another cache daemon.

2,048 entries is the recommended starting point for 512 MB RAM / 0.1 vCPU. Increase it only after measuring memory and cache-hit behavior.

## DoH security

A public DoH service can be abused, so v7.8.7 uses layered controls before traffic reaches MosDNS:

```text
per-IP:   5 req/s, burst 12, max 512 tracked IPs
global:  40 req/s, burst 80
connections: 128 global
request body / dns parameter: 4096 bytes
MosDNS: 15 QPS client ceiling
```

Per-IP connection limiting remains `0` because Firefox/Fennec benefit from persistent DoH connections. The global connection ceiling still protects the tiny Koyeb instance from connection floods. POST requests must use `application/dns-message`; only GET and POST are accepted.

The service expects Koyeb's proxy to provide the client address through `X-Forwarded-For`/`X-Real-IP` for the client limiter.


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


## Runtime health supervisor (v8.1)

The health scorer now has two layers: startup scoring and a lightweight runtime supervisor. By default it probes the three configured DoH upstreams every 300 seconds (one small 3-endpoint probe cycle), keeps EWMA/failure state, and uses hysteresis before switching. A transient single failure does not restart MosDNS. The active upstream is switched only after repeated failure (`HEALTH_FAILS_TO_SWITCH=2`) or a materially better healthy endpoint is detected; restarts are rate-limited by `HEALTH_RESTART_COOLDOWN=900` seconds. The warm cache remains on disk so switching does not discard the warm-start cache.

Runtime health settings: `HEALTH_INTERVAL`, `HEALTH_FAILS_TO_SWITCH`, `HEALTH_RESTART_COOLDOWN`. For the 0.1-vCPU profile, the defaults intentionally favor low probe overhead and stability over aggressive failover.
