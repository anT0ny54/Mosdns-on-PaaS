# MosDNS on Koyeb

A lightweight, Koyeb-ready deployment of [MosDNS](https://github.com/IrineSistiana/Mosdns) v4.5.3 with DNS-over-HTTPS (DoH) support.

The project is designed to be simple, fast, and easy to deploy. It uses HaGeZi DNS-over-HTTPS upstreams, a low-footprint RAM cache with native disk snapshots, and no GeoIP or Geosite database downloads.

## Features

- MosDNS v4.5.3
- DNS-over-HTTPS support
- Automatic Koyeb `PORT` detection
- Configurable DoH endpoint path
- HaGeZi DoH upstream resolvers
- DNS pipelining enabled
- RAM DNS cache with native on-disk warm-start cache snapshots
- No GeoIP or Geosite downloads
- No external geodata files
- Dockerfile-based deployment
- Focused specifically on Koyeb
- No unnecessary Heroku, Fly.io, or Railway configuration files

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


## Disk-assisted cache

The cache uses MosDNS's native cache dump support rather than adding dnsmasq, BIND, SQLite, or another resident process. Hot queries stay in RAM for low latency; MosDNS periodically writes a bounded cache snapshot to local disk and reloads it on startup.

The default settings are conservative for a Koyeb instance with 512 MB RAM, 0.1 vCPU, and 2 GB local SSD:

- `CACHE_SIZE=32768` — maximum in-memory cache entries.
- `CACHE_DUMP_FILE=/var/cache/mosdns/cache.dump` — local warm-cache snapshot.
- `CACHE_DUMP_INTERVAL=300` — snapshot every 5 minutes.

These snapshots are only a warm-start optimization. Koyeb local storage is ephemeral, so a replacement/redeployment can still start with an empty cache. DNS operation does not depend on the dump file.

You can override these values with environment variables. For example, `CACHE_SIZE=16384` reduces RAM usage further, while `CACHE_DUMP_INTERVAL=600` reduces disk-write frequency.

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

