# MosDNS on Koyeb

Minimal Koyeb-ready MosDNS v4 deployment.

## Features

- MosDNS v4.5.3
- DNS-over-HTTPS (DoH) service
- Koyeb `PORT` is used automatically
- Custom `DOH_PATH` can be set at runtime
- No GeoIP/Geosite downloads
- No geodata files
- No Heroku/Fly/Railway-specific deployment files
- Hagezi DoH upstreams with pipelining enabled
- Small in-memory cache

## Koyeb deployment

Deploy this repository with the **Dockerfile** builder.

Expose the container port as HTTP. Koyeb Web Services provide `PORT` automatically; if it is not set explicitly, Koyeb uses the lowest exposed port. The Dockerfile exposes port 8080, so the service normally uses port 8080.

Recommended environment variable:

```text
DOH_PATH=/dns-query
```

For a custom path, for example:

```text
DOH_PATH=/my-secret-dns
```

Keep the path private to reduce abuse of a public DoH endpoint.

### Koyeb CLI example

```bash
koyeb app init mosdns   --git github.com/YOUR_USERNAME/YOUR_REPOSITORY   --git-branch main   --git-builder docker   --ports 8080:http   --routes /:8080   --env DOH_PATH=/dns-query   --checks 8080:tcp
```

Koyeb's default TCP health check is appropriate for this service because the DoH endpoint is not a normal web page.

## DoH endpoint

After deployment:

```text
https://YOUR-KOYEB-DOMAIN/dns-query
```

## Upstreams

The configuration uses:

- `https://root.hagezi.org/dns-query`
- `https://wurzn.hagezi.org/dns-query`
- `https://juuri.hagezi.org/dns-query`

All are configured as trusted upstreams with `enable_pipeline: true`.

## Notes

This project is intentionally kept focused on Koyeb. The original multi-PaaS deployment files and GeoIP/Geosite installation logic have been removed.
