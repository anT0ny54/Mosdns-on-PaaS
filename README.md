# MosDNS Public DoH for Koyeb (v4.5.3)

Lightweight public DNS-over-HTTPS resolver optimized for a small Koyeb instance (512 MB RAM, 0.1 vCPU, 2 GB SSD).

## Design

- MosDNS **v4.5.3**
- Public DoH endpoint; no authentication
- 32,768-entry RAM cache by default
- Native MosDNS cache snapshot to local SSD every 5 minutes
- Three redundant HaGeZi DoH upstreams
- Per-client abuse guard: 20 QPS per client IP
- No SQLite, BIND, dnsmasq, nginx, or additional resident service

The SSD cache is a **warm-start snapshot**, not durable storage. If Koyeb replaces the instance and the file disappears, MosDNS simply starts with an empty RAM cache and continues normally.

## Public resolver abuse protection

The endpoint is intentionally public. The `client_limiter` plugin is only a lightweight resource-protection mechanism. It is not DDoS protection.

MosDNS v4.5.3 provides `client_limiter` with `max_qps`, `v4_mask`, and `v6_mask`; requests over the configured limit are returned as `REFUSED`. The v4.5.3 implementation is the reason this project uses `client_limiter` rather than the newer v5 `rate_limiter` plugin.

Default:

```yaml
max_qps: 20
v4_mask: 32
v6_mask: 48
```

If your public traffic is legitimate but gets limited, increase `max_qps` carefully. If the service receives abusive traffic, use Koyeb/network-level controls as the stronger mitigation.

## Environment variables

- `PORT` — HTTP listener port; default `8080`
- `DOH_PATH` — public DoH path; default `/dns-query`
- `CACHE_SIZE` — RAM cache entries; default `32768`
- `CACHE_DUMP_FILE` — local snapshot path; default `/var/cache/mosdns/cache.dump`
- `CACHE_DUMP_INTERVAL` — snapshot interval in seconds; default `300`

## Why v4.5.3?

This project intentionally stays on MosDNS v4.5.3 because the original deployment configuration is written for the v4 plugin/configuration model. MosDNS v5 changes several plugin names and configuration structures. Mixing v5 configuration into a v4 image can cause startup failures.

## Deployment

Build/deploy the repository with the included Dockerfile. Koyeb should provide `PORT` automatically; leave it unset unless you need a custom port.

After deployment, the public endpoint is:

`https://YOUR-KOYEB-DOMAIN${DOH_PATH}`

## Notes

This service is deliberately public. Do not treat an obscure URL path as authentication.

The cache snapshot is best-effort. Local PaaS storage can disappear when an instance is replaced.
