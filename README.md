# MosDNS on Koyeb

A lightweight [Koyeb](https://www.koyeb.com/) deployment of [MosDNS v4.5.3](https://github.com/IrineSistiana/mosdns) that serves public DNS-over-HTTPS (DoH), tuned to fit entirely inside Koyeb's **512 MB RAM / 0.1 vCPU / 2 GB SSD** Free Web Service.

- DoH-only. No plain UDP/TCP DNS listener is exposed.
- Three [HaGeZi](https://github.com/hagezi/dns-blocklists) DoH upstreams with strict sequential failover (never parallel fan-out).
- A small in-memory cache backed by a bounded, atomic-write disk snapshot for warm restarts.
- A lightweight Go reverse proxy sits in front of MosDNS and enforces per-IP + global rate limits, a global connection cap, and request-size limits before anything reaches MosDNS.
- A background health supervisor periodically re-probes the three upstreams and performs a graceful hot-swap restart only when it's clearly justified, preserving the warm cache across the swap.
- If MosDNS or the proxy ever exits, the container exits too, so Koyeb restarts the Instance.

## How it works

```
client --DoH--> ip-conn-proxy (:PORT, rate/conn limits) --http--> mosdns (127.0.0.1:MOSDNS_BACKEND_PORT)
                                                                        |
                                                          sequential failover: upstream_0 -> upstream_1 -> upstream_2
```

### Sequential failover

Each MosDNS process has an ordered upstream chain: the selected upstream is tried first, the second is contacted only if the first fails, and the third only if the second also fails. This is implemented with nested v4.5.3 `fallback` blocks rather than sending all three queries in parallel — `fast_forward` with multiple upstreams is a *parallel* mechanism in v4.5.3, so this project instead declares three single-upstream `fast_forward` plugins and composes them with nested fallback nodes.

A successful upstream stops the chain, so a healthy query normally only touches one upstream — failover exists for transport/server failures, not for valid responses like NXDOMAIN.

### Upstream selection and the runtime health supervisor

At startup, `HAGEZI_UPSTREAM` controls how the three HaGeZi endpoints are ordered:

| Value | Behavior |
| --- | --- |
| `rotate` (default) | Probes all three endpoints once and puts the healthiest first; falls back to a fixed order if the probe is unavailable. |
| `random` | Same probing, but randomizes among near-equally-healthy candidates instead of always picking the single best. |
| `https://...` (a fixed URL) | Prefers that endpoint first; the two built-in HaGeZi endpoints remain as sequential fallbacks. |

After startup, a lightweight background supervisor re-probes the three upstreams every `HEALTH_INTERVAL` seconds (default 300s — a single small 3-endpoint probe). It keeps an EWMA/failure score per endpoint and uses hysteresis before acting:

- A single transient failure does **not** trigger a restart.
- The active upstream is only swapped after `HEALTH_FAILS_TO_SWITCH` consecutive failures (default 2), or when a materially healthier endpoint is detected.
- Swaps are rate-limited by `HEALTH_RESTART_COOLDOWN` (default 900s) so the process can't churn.
- A swap starts a new MosDNS process with the new order and only kills the old one once the new one is confirmed alive, so a bad config can never take down a working resolver.
- The warm cache lives on disk, so a swap does not discard it.

There is no blind periodic restart — MosDNS is only ever restarted when the supervisor's health data actually justifies it.

### Warm cache

The hot path is always the in-memory cache. A custom cache backend (`content/warm_backend.go`, patched into MosDNS's `cache` plugin at build time — see the Dockerfile) additionally maintains a second, disk-backed copy bounded to the same size as the RAM cache, so it can never grow without limit. It's written atomically (write-then-rename) on `CACHE_DUMP_INTERVAL`, and again on graceful shutdown, so a process restart (including a health-supervisor swap) can reload it instead of starting cold. Expired entries are never restored.

Koyeb's local storage is ephemeral, though — a freshly created or replaced Instance may not have a prior snapshot, so the deployment must never depend on the warm cache for correctness. It's a startup optimization only.

### DoH anti-abuse proxy

A public DoH endpoint can be abused, so requests pass through a small Go reverse proxy (`content/ip_conn_proxy.go`) before reaching MosDNS:

```text
per-IP:      DOH_RATE_LIMIT req/s, burst DOH_RATE_BURST, up to DOH_RATE_MAX_IPS tracked IPs
global:      GLOBAL_RATE_LIMIT req/s, burst GLOBAL_RATE_BURST
connections: GLOBAL_CONN_LIMIT concurrent, global
request:     DOH_MAX_BODY_BYTES max POST body / GET dns= parameter
MosDNS:      MAX_QPS client-side ceiling
```

Only RFC 8484 GET/POST DoH requests are accepted (`application/dns-message` for POST); anything else is rejected before it reaches MosDNS. Per-IP connection limiting (`IP_CONN_LIMIT`) defaults to `0` (disabled) because Firefox/Fennec rely on persistent DoH connections — abuse control is rate-based instead. The proxy expects Koyeb's edge to provide the client address via `X-Real-IP`/`X-Forwarded-For`, and strips any client-supplied copies of those headers before forwarding.

## Why the configuration is deliberately small

On a 0.1-vCPU instance, avoiding unnecessary resident processes and large databases matters more than maximizing cache size. This deployment intentionally does **not** install:

- GeoIP / Geosite databases or downloads
- SQLite, dnsmasq, BIND, or any other resident cache/database process
- Deployment files for other PaaS providers

2,048 cache entries is a sane starting point for 512 MB RAM / 0.1 vCPU — increase it only after measuring actual memory and cache-hit behavior on your traffic.

## Environment variables

All of these have working defaults baked into the image; you only need to set `DOH_PATH` to deploy.

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | Public port Koyeb routes to (Koyeb sets this automatically). |
| `DOH_PATH` | `/dns-query` | Public DoH endpoint path. |
| `HEALTH_PATH` | `/health` | Path used for Koyeb's health check. |
| `MOSDNS_BACKEND_PORT` | `18080` | Internal loopback port between the proxy and MosDNS. |
| `HAGEZI_UPSTREAM` | `rotate` | `rotate`, `random`, or one fixed `https://` endpoint. |
| `CACHE_SIZE` | `2048` | Max in-memory (and warm-disk) cache entries. |
| `CACHE_DUMP_FILE` | `/var/cache/mosdns/cache.dump` | Warm-cache snapshot path. |
| `CACHE_DUMP_INTERVAL` | `3300` | Snapshot interval in seconds (55 min). |
| `MAX_QPS` | `15` | MosDNS client-side QPS ceiling. |
| `SERVER_TIMEOUT` | `8` | MosDNS per-query server timeout, seconds. |
| `UPSTREAM_IDLE_TIMEOUT` | `30` | Upstream connection idle timeout, seconds. |
| `UPSTREAM_MAX_CONNS` | `2` | Max concurrent connections per upstream. |
| `DOH_IDLE_TIMEOUT` | `120` | Local DoH listener idle timeout, seconds. |
| `DOH_RATE_LIMIT` | `5` | Per-IP DoH request rate (req/s). |
| `DOH_RATE_BURST` | `12` | Per-IP burst allowance. |
| `DOH_RATE_MAX_IPS` | `512` | Max tracked client IPs for per-IP limiting. |
| `GLOBAL_RATE_LIMIT` | `40` | Global DoH request rate (req/s). |
| `GLOBAL_RATE_BURST` | `80` | Global burst allowance. |
| `GLOBAL_CONN_LIMIT` | `128` | Global concurrent connection ceiling. |
| `IP_CONN_LIMIT` | `0` | Per-IP connection limit; `0` preserves Firefox/Fennec reuse. |
| `DOH_MAX_BODY_BYTES` | `4096` | Max DoH POST body / GET `dns` parameter size. |
| `HEALTH_CHECK` | `true` | Enables startup probing and the runtime health supervisor. |
| `HEALTH_TIMEOUT_MS` | `1200` | Per-probe timeout, milliseconds. |
| `HEALTH_INTERVAL` | `300` | Runtime re-probe interval, seconds (min 30). |
| `HEALTH_FAILS_TO_SWITCH` | `2` | Consecutive failures before the active upstream is swapped. |
| `HEALTH_RESTART_COOLDOWN` | `900` | Minimum seconds between supervisor-triggered restarts. |
| `HEALTH_EWMA_ALPHA` | `0.35` | Smoothing factor for the latency EWMA (0–1]. |
| `HEALTH_FAILURE_PENALTY_MS` | `1500` | Score penalty added per probe failure. |
| `HEALTH_SWITCH_MARGIN_PCT` | `0.20` | Relative score improvement required to switch without a failure. |
| `HEALTH_SWITCH_MARGIN_MS` | `25` | Absolute score improvement required to switch without a failure. |
| `HEALTH_STATE_FILE` | `/tmp/mosdns-upstream-state.tsv` | Where probe EWMA/failure state persists between probes. |
| `HEALTH_BACKEND_TIMEOUT_MS`¹ | `1000` | Timeout for the proxy's own `/health` TCP check against MosDNS. |
| `UPSTREAM_0_IP` / `_1_IP` / `_2_IP` | *(pinned HaGeZi IPs)* | `dial_addr` pins for the three built-in HaGeZi endpoints; unused for a custom `HAGEZI_UPSTREAM` endpoint. |
| `GOMEMLIMIT` | `320MiB` | Go runtime soft memory limit. |
| `GOMAXPROCS` | `1` | Go runtime CPU limit, matched to the 0.1 vCPU Instance. |

¹ Not wired into `entrypoint.sh`; set it directly as a Koyeb environment variable if you need to change it — the Go binary reads it straight from its environment.

## Requirements

- A [Koyeb](https://www.koyeb.com/) account
- A GitHub repository containing this project
- A domain provided by Koyeb, or a custom domain
- Dockerfile builder enabled for the service

## Deploy to Koyeb

### Using the Koyeb dashboard

1. Sign in to your Koyeb account.
2. Create a new **Web Service**.
3. Select the GitHub repository containing this project.
4. Choose the **Dockerfile** builder.
5. Expose port `8080` using the HTTP protocol.
6. Add the environment variable `DOH_PATH=/dns-query` (or your own custom path — see below).
7. Deploy the service.

Koyeb provides the `PORT` environment variable automatically; if none is configured explicitly, Koyeb uses the lowest port exposed by the Dockerfile (`8080` here).

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

Replace `YOUR_USERNAME` and `YOUR_REPOSITORY` with your GitHub username and repository name. A TCP health check on `8080` is used because the DoH endpoint itself isn't a normal web page.

## Configure a custom DoH path

```text
DOH_PATH=/my-secret-dns
```

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

The endpoint is then available at `https://YOUR-KOYEB-DOMAIN/my-secret-dns`.

**Keep your DoH path private.** A publicly known DoH resolver can be abused by third parties, increasing bandwidth usage and cost. For stronger access control, consider placing the service behind an authentication layer or a private network.

## DoH endpoint

With the default configuration:

```text
https://YOUR-KOYEB-DOMAIN/dns-query
```

Use this URL in any DNS client that supports DNS-over-HTTPS. Replace `/dns-query` with your custom path if you set one.

## Upstream resolvers

- `https://root.hagezi.org/dns-query`
- `https://wurzn.hagezi.org/dns-query`
- `https://juuri.hagezi.org/dns-query`

All three are configured as trusted resolvers with DNS pipelining enabled and are always kept in the sequential failover chain, regardless of which one is currently first.

## Other free HaGeZi-backed DoH services

A few other public DoH endpoints that also apply HaGeZi blocklists (Multi Pro / TIF), for reference or as a fallback client configuration:

| Service | DNS-over-HTTPS endpoint |
| --- | --- |
| `https://freedns.koyeb.app/dns-query` |
| `https://freedns-six.vercel.app/api/doh/dns-query` |
| `https://dnssix.netlify.app/api/doh/dns-query` |

These are third-party/community services outside this project — expect usage limits and availability to vary.

## Health checks

Koyeb is configured with a TCP health check on port `8080`. Koyeb documents that liveness health-check failures can trigger an Instance restart. Koyeb Free Instances can also scale to zero after roughly one hour without traffic — the next request cold-starts a new Instance.

## Project scope

This repository is intentionally focused on running MosDNS v4.5.3 on Koyeb's free tier. Deliberately **not** included: GeoIP/Geosite databases or downloads, a resident cache/database process, and deployment files for other PaaS providers.

## 🚀 Bandwidth Hero Server

A lightweight image proxy designed to slash bandwidth usage and accelerate your browsing experience. 

Bandwidth Hero Server fetches remote images, compresses them on the fly, and delivers optimized versions to your device for faster loading and lower data consumption.

🖥️ **Try it out:** [Bandwidth Hero](https://bhserv.netlify.app/)

## Supporting the project

If you find this project useful, donations are appreciated.

**Bitcoin:** `1HntwKxyqGCfnSGvGLMUTRAqLnTvLarAQP`

## License

See the repository's [LICENSE](LICENSE) file.

## Changelog

**8.2.1**
- Reverted the build-stage Go bump from 8.2.0 (`golang:1.19-alpine3.17` → `golang:1.26-alpine3.24`). It broke the build: mosdns v4.5.3 transitively depends on `github.com/lucas-clemente/quic-go v0.30.0` (pulled in by the built-in `forward` plugin even though this config only uses `fast_forward`), and that quic-go version has a deliberate compile-time guard refusing to build on Go 1.20+. The build stage is back on `golang:1.19-alpine3.17`; upgrading past Go 1.19 here would need replacing or vendoring that dependency first, which is out of scope for this pass. The runtime stage (`alpine:3.24`, no Go toolchain) is unaffected and stays current.

**8.2.0**
- Consolidated this README into a single, internally-consistent document (it previously carried several superseded revisions side by side, with conflicting env-var defaults and an inaccurate description of upstream rotation).
- Bumped the runtime-stage Alpine image (`alpine:3.22` → `alpine:3.24`).
- De-duplicated the config-rendering logic in `entrypoint.sh` into a single `render_config()` function used by both the startup path and the runtime health-supervisor swap path. This also fixes a latent bug: the swap path previously skipped the blank-`dial_addr` cleanup step, so a live upstream swap involving a custom (non-pinned-IP) `HAGEZI_UPSTREAM` endpoint could have rendered an invalid `dial_addr:` field into the runtime config.
- No functional/plugin changes to the MosDNS configuration itself.
