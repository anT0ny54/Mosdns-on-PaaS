# MosDNS v5.3.4 — Koyeb Optimized DoH

A minimal MosDNS v5.3.4 DNS-over-HTTPS forwarder optimized for Koyeb.

## Features

- MosDNS v5.3.4
- No GeoIP/Geosite downloads
- No unnecessary PaaS-specific files
- Three Hagezi DoH upstreams:
  - `https://root.hagezi.org/dns-query`
  - `https://wurzn.hagezi.org/dns-query`
  - `https://juuri.hagezi.org/dns-query`
- `trusted: true` and `enable_pipeline: true` on every upstream
- 40,960-entry in-memory cache
- Lazy cache for up to 3 days
- Koyeb `PORT` support
- Configurable `DOH_PATH`

## Koyeb deployment

Deploy the repository with the **Dockerfile** builder.

Expose one public web port:

- Port: `8080` (or the value you set as `PORT`)
- Protocol: `HTTP`
- Route: `/`

Koyeb automatically provides the `PORT` environment variable for Web Services. The container also defaults to `8080` if `PORT` is not supplied.

Recommended environment variable:

```text
DOH_PATH=/dns-query
```

For additional protection against unwanted public use, choose a private/random path, for example:

```text
DOH_PATH=/dns-query
```

## Health check

The default Koyeb TCP health check is sufficient because MosDNS listens on the exposed HTTP port. No separate health endpoint is required.

## DoH endpoint

After deployment, the DoH endpoint is:

```text
https://YOUR-SERVICE.koyeb.app/dns-query
```

If you changed `DOH_PATH`, replace `/dns-query` with your configured path.

## Project files

```text
.
├── .dockerignore
├── .gitignore
├── Dockerfile
├── LICENSE
├── README.md
└── content
    ├── config.yaml
    └── entrypoint.sh
```
