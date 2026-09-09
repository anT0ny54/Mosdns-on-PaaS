# MosDNS v4.5.3 — Koyeb Public DoH

Lightweight public DNS-over-HTTPS resolver designed for Koyeb's small instance limits.

## Current deployment profile

- MosDNS: **v4.5.3**
- Docker image: pinned to the official linux/amd64 v4.5.3 digest
- DoH: `/dns-query`
- Port: Koyeb `$PORT`
- Cache: 32,768 entries in RAM
- Cache persistence: **disabled in this build**
- Public resolver protection: `client_limiter`, 20 QPS/client
- Upstreams: HaGeZi Root / Wurzn / Juuri DoH
- No SQLite
- No BIND
- No dnsmasq
- No nginx
- No additional resident services

## Why disk cache is disabled

The deployed service previously failed during cache initialization with:

`invalid keys: dump_file, dump_interval`

Although MosDNS v4.5.3 documentation/source examples include these cache options, this build intentionally removes them so the first deployment can be validated cleanly. The entrypoint prints `mosdns version` before starting.

After a successful deployment, disk-backed cache persistence can be tested separately without mixing it with the initial startup problem.

## Environment variables

- `PORT` — defaults to `8080`; Koyeb should provide its assigned port.
- `DOH_PATH` — defaults to `/dns-query`.
- `CACHE_SIZE` — defaults to `32768`.

## Public resolver warning

This is intentionally public DoH. `client_limiter` is basic abuse protection and is not DDoS protection, authentication, or a substitute for upstream/network controls.

## Health check

Use the Koyeb HTTP health check against `/dns-query` only if your Koyeb setup supports the required DoH request semantics. Otherwise use the service's TCP/HTTP listener check as appropriate.
