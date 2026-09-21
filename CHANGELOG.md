# Changelog

## 8.2.14

Audit of the runtime path (Koyeb edge -> `ip-conn-proxy` -> MosDNS `forward`
sequence -> `mem_cache` -> `sequential_failover`) and its configuration.

### Fixed
- `entrypoint.sh`: `render_config` runs inside `if ! render_config`, where
  `set -e` is disabled. A failed or partial `sed` render (for example a full
  `/tmp`) could therefore still be checked for placeholders and installed as
  the runtime config. The render and the `dial_addr` cleanup are now checked
  explicitly and the candidate file is discarded on failure.
- `ip_conn_proxy.go`: the proxy kept idle backend connections for a fixed 90 s
  while the MosDNS listener idle timeout is configurable (`DOH_IDLE_TIMEOUT`).
  With a value below 90 s MosDNS could close a pooled connection first, and a
  reused connection then failed non-replayable DoH POST requests with 502.
  The proxy now receives `DOH_IDLE_TIMEOUT` and keeps idle backend connections
  between 1 s and 90 s, always 5 s shorter than the MosDNS timeout.

### Changed
- `entrypoint.sh`: the startup upstream selection now logs a warning when the
  health probe produced no usable result and the default order is used, instead
  of falling back silently.
- `entrypoint.sh`: the supervisor loop ticks every 5 s instead of 2 s. This
  removes about 60% of the idle `sleep`/`kill -0`/`date` process spawns on the
  0.25 vCPU instance; crash detection is still bounded by a few seconds and
  `SIGTERM` handling stays immediate because the wait is interruptible.
- `README.md`: documented the `DOH_IDLE_TIMEOUT` / proxy relationship and
  corrected the validation summary (idle timeouts and rate limits may be 0).

### Removed
- `ip_conn_proxy.go`: three dead `r.Close = true` assignments. `net/http`
  ignores `Request.Close` on the server side; the `Connection: close` response
  header that sits next to each one is what actually closes the connection.

### Reviewed, no change needed
- `sequential_forward.go`, `warm_backend.go`, `upstream_probe.go`,
  `config.yaml`, `Dockerfile`: no functional defects found. Dockerfile `ENV`
  defaults match the `entrypoint.sh` defaults and the README table.

### Notes / follow-ups (not changed)
- `sequentialForward.closers` duplicates `upstreams`; it can be merged once the
  `upstream.Upstream` interface in the pinned MosDNS v4.5.3 is confirmed to
  include `io.Closer`.
- Failover order is fixed per process. A dead first upstream costs one
  per-attempt timeout on every uncached query until the supervisor reorders
  (about `HEALTH_FAILS_TO_SWITCH` x `HEALTH_INTERVAL`). Passive failure
  memory in `sequential_forward` would remove that delay.
- README still contains unrelated sections (free DNS list, Bandwidth Hero,
  donation address) that contradict "Repository scope".
- Go changes were not compiled in this review environment (no Go toolchain);
  build with `docker build` before deploying.

## 8.2.13

Full review of every file in the archive (Dockerfile, config.yaml, entrypoint.sh,
ip_conn_proxy.go, sequential_forward.go, upstream_probe.go, warm_backend.go,
README.md). Runtime path: entrypoint -> ip-conn-proxy -> MosDNS -> cache ->
sequential_forward -> upstreams. Only the files listed under Changed below were
modified. The shell changes were exercised with stubbed `mosdns`, `mosdns-probe`
and `ip-conn-proxy` binaries under `dash` (startup order, runtime recovery and
evacuation, `rotate`/`random`, `HEALTH_CHECK=false`); the Go sources were read
but not compiled here (no Go toolchain in the review environment).

### Fixed

- **entrypoint.sh: a fixed `HAGEZI_UPSTREAM=https://...` was not honored.** The
  startup probe sorted all candidates purely by latency with no active upstream,
  and a custom endpoint has no pinned IP, so it was normally placed *last*
  (reproduced: custom endpoint ended up as failover slot 3). The runtime
  supervisor could likewise swap it out for a merely faster fallback. This
  contradicted the README ("use the custom endpoint as the first candidate").
  In fixed mode the preferred endpoint now stays first while it passes its
  probe; only the fallbacks are ordered by measurement. If it fails its startup
  probe the measured order is used, the supervisor evacuates it after
  `HEALTH_FAILS_TO_SWITCH` consecutive failures, and moves back to it when it
  recovers (restart cooldown still applies). `rotate` and `random` are unchanged.

### Changed

- **entrypoint.sh:** a `SERVER_TIMEOUT` above 12 s now logs a warning. The proxy
  gives up on MosDNS after 12 s (`ResponseHeaderTimeout`), so a larger value
  could never take effect and clients would get `502` first.
- **entrypoint.sh:** the supervisor loop forked `date` every 2 s even with
  `HEALTH_CHECK=false`; it is now only called when health checking is enabled.
- **README.md:** documented fixed-endpoint behavior, the `SERVER_TIMEOUT`/proxy
  interaction, and the startup validation limits that were enforced but
  undocumented.
- **VERSION:** 8.2.13.

### Reviewed, no change

- `Dockerfile`, `config.yaml`, `sequential_forward.go`, `warm_backend.go`,
  `upstream_probe.go`, `ip_conn_proxy.go`: no bug found that could be fixed
  safely without a compiler. Defaults are consistent across the Dockerfile
  `ENV`, entrypoint fallbacks, and Go defaults.
- **README.md** still contains an unrelated "Bandwidth Hero Server" section that
  contradicts its own "Repository scope" paragraph. It was left as-is because it
  is the maintainer's content; remove it if it does not belong here.
- Rate limiting keys on the exact client IP, so an IPv6 client can rotate
  addresses inside its /64 to get fresh buckets. Aggregating IPv6 by /64 would
  change limiter behavior and was left for a deliberate decision.

## 8.2.12

Follow-up review of the runtime path (entrypoint -> ip-conn-proxy -> MosDNS ->
sequential_forward -> upstreams). `Dockerfile`, `config.yaml`,
`sequential_forward.go` and `warm_backend.go` were re-read and needed no change.

### Fixed

- **entrypoint.sh: placeholder check could never fire.** 8.2.11 reduced it to
  the literal string `__NAME__`, which appears in no template, so a template
  placeholder missing from the `sed` list would have shipped to MosDNS
  unnoticed. `render_config` now takes every `__PLACEHOLDER__` found in the
  template and fails (naming them) if any survives rendering.
- **ip_conn_proxy.go: one client could drain the global DoH budget.** The global
  bucket was consumed before the per-IP bucket, so a single flooding client
  spent global tokens on requests its own limit would have rejected and made
  other clients get `429`. The per-IP limit is now applied first (matching the
  `/health` path).
- **entrypoint.sh: `HEALTH_RATE_BURST` / `GLOBAL_HEALTH_RATE_BURST`** were only
  compared numerically, so a non-numeric value produced a raw shell
  "integer expression expected" error. They now go through `validate_uint`.
- **entrypoint.sh: health rate settings** (`HEALTH_RATE_LIMIT`,
  `HEALTH_RATE_BURST`, `GLOBAL_HEALTH_RATE_LIMIT`, `GLOBAL_HEALTH_RATE_BURST`)
  are validated but were not passed to the proxy explicitly; they only reached
  it because the Dockerfile happens to `ENV`-export them. They are now forwarded
  like every other proxy setting.

### Changed

- **entrypoint.sh:** `probe_candidate_set` and `set_default_order` carried the
  same four-way `case` (fixed vs. rotate/random vs. custom). Both now use one
  `set_candidate_order`, which writes only `CAND_0..2` and so cannot disturb the
  live `ORDER_*`. Behavior is unchanged.
- **entrypoint.sh:** removed the plain-DNS guard that tested three hard-coded
  `https://` constants (it could not trigger; custom endpoints are already
  restricted to `https://` by the `HAGEZI_UPSTREAM` check).
- **entrypoint.sh:** collapsed the two-branch `switch=1` decision in
  `health_check_once` into one condition; logic is identical.
- **upstream_probe.go:** `HEALTH_TIMEOUT_MS` parsing duplicated
  `envIntOrDefault`, and `HEALTH_FAILS_TO_SWITCH` was re-read from the
  environment inside the result loop. Both now use `envIntOrDefault`, read once.
- **README.md:** documented the per-IP-before-global rate-limit order.
- **VERSION:** 8.2.12.

## 8.2.12

Follow-up review of the runtime path (entrypoint -> ip-conn-proxy -> MosDNS ->
sequential_forward -> upstreams). `Dockerfile`, `config.yaml`,
`sequential_forward.go` and `warm_backend.go` were re-read and needed no change.

### Fixed

- **entrypoint.sh: placeholder check could never fire.** 8.2.11 reduced it to
  the literal string `__NAME__`, which appears in no template, so a template
  placeholder missing from the `sed` list would have shipped to MosDNS
  unnoticed. `render_config` now takes every `__PLACEHOLDER__` found in the
  template and fails (naming them) if any survives rendering.
- **ip_conn_proxy.go: one client could drain the global DoH budget.** The global
  bucket was consumed before the per-IP bucket, so a single flooding client
  spent global tokens on requests its own limit would have rejected and made
  other clients get `429`. The per-IP limit is now applied first (matching the
  `/health` path).
- **entrypoint.sh: `HEALTH_RATE_BURST` / `GLOBAL_HEALTH_RATE_BURST`** were only
  compared numerically, so a non-numeric value produced a raw shell
  "integer expression expected" error. They now go through `validate_uint`.
- **entrypoint.sh: health rate settings** (`HEALTH_RATE_LIMIT`,
  `HEALTH_RATE_BURST`, `GLOBAL_HEALTH_RATE_LIMIT`, `GLOBAL_HEALTH_RATE_BURST`)
  are validated but were not passed to the proxy explicitly; they only reached
  it because the Dockerfile happens to `ENV`-export them. They are now forwarded
  like every other proxy setting.

### Changed

- **entrypoint.sh:** `probe_candidate_set` and `set_default_order` carried the
  same four-way `case` (fixed vs. rotate/random vs. custom). Both now use one
  `set_candidate_order`, which writes only `CAND_0..2` and so cannot disturb the
  live `ORDER_*`. Behavior is unchanged.
- **entrypoint.sh:** removed the plain-DNS guard that tested three hard-coded
  `https://` constants (it could not trigger; custom endpoints are already
  restricted to `https://` by the `HAGEZI_UPSTREAM` check).
- **entrypoint.sh:** collapsed the two-branch `switch=1` decision in
  `health_check_once` into one condition; logic is identical.
- **upstream_probe.go:** `HEALTH_TIMEOUT_MS` parsing duplicated
  `envIntOrDefault`, and `HEALTH_FAILS_TO_SWITCH` was re-read from the
  environment inside the result loop. Both now use `envIntOrDefault`, read once.
- **README.md:** documented the per-IP-before-global rate-limit order.
- **VERSION:** 8.2.12.

## 8.2.11

Review pass over the whole archive (Dockerfile, entrypoint, config, all four Go
sources, README). `config.yaml`, `upstream_probe.go` and `warm_backend.go` were
read in full and needed no change.

### Fixed

- **entrypoint.sh: SIGTERM/SIGINT handling.** The trap handler only ran
  `cleanup` and then returned, so the supervisor loop resumed, saw the
  just-stopped MosDNS and logged "MosDNS exited; restarting instance via Koyeb"
  with exit status 1 on every normal stop. TERM/INT now `exit 143`/`exit 130`
  and `cleanup` runs from the EXIT trap. The loop's `sleep 2` is now
  `sleep 2 & wait $!`, so a signal is handled immediately rather than after up
  to 2 seconds.
- **entrypoint.sh: runtime upstream switch.** `set_failover_order` rebuilt the
  order from a fixed permutation and threw away the measured order for
  positions 2 and 3. The supervisor now adopts the probe's full order, exactly
  as startup does. `set_failover_order` (about 40 lines) is removed.
- **entrypoint.sh: state drift on failed render.** A failed `render_config`
  during a switch left `ORDER_0..2` changed while the old config kept running;
  the previous order is now restored.
- **entrypoint.sh: replacement PID tracking.** `MOSDNS_PID` is set immediately
  after the replacement `mosdns start`, closing a window where a signal could
  leave the new process untracked.
- **entrypoint.sh: IPv4 validation** now rejects leading-zero octets
  (`010.1.1.1`). Go's `net.ParseIP` rejects them, so the probe silently stopped
  pinning while MosDNS was handed an unusable `dial_addr`.
- **entrypoint.sh: `HEALTH_TIMEOUT_MS=0`** is now rejected; previously the probe
  silently substituted its default.
- **entrypoint.sh: cache directory.** `mkdir -p` failure no longer aborts
  startup (the warm cache is an optimization); it logs a warning.
- **sequential_forward.go: stalled upstream defeated failover.** Every attempt
  shared the single query deadline (`SERVER_TIMEOUT`, 8 s), so a black-holed
  first upstream consumed all of it and the fallbacks never ran. Each attempt is
  now bounded to an equal share of the time remaining; the last upstream keeps
  the remainder. Contexts without a deadline behave as before.
- **ip_conn_proxy.go: `X-Forwarded-For` spoofing.** `Header.Get` returns the
  first header line, which is client-controlled when a client sends several
  `X-Forwarded-For` lines. The last header line is now used.
- **ip_conn_proxy.go: hung backend.** Added `ResponseHeaderTimeout` (12 s) on the
  proxy-to-MosDNS transport so a hung MosDNS cannot pin all `MaxConnsPerHost`
  slots until each client gives up. It stays below the 15 s `WriteTimeout` so
  the 502 can still be written.

### Changed

- **Dockerfile:** the probe helper and DoH proxy were two `package main` files
  in one directory (`/src/probe`), which only built because each was compiled
  by file name. They now live in `/src/probe` and `/src/proxy` and are built as
  packages.
- **Dockerfile:** build now fails if the `sed` patch of `cache.go` does not
  apply exactly once (previously it could silently no-op and ship a build with
  the warm cache disabled).
- **entrypoint.sh:** unresolved-placeholder check reduced to `__NAME__`; the
  `BACKEND_[0-9]+`, `PORT_PLACEHOLDER`, `BACKEND_PORT_PLACEHOLDER` and
  `PATH_PLACEHOLDER` patterns matched nothing in any template.
- **ip_conn_proxy.go:** two near-identical startup `log.Printf` calls merged
  into one.
- **README.md:** documented the per-attempt timeout, full-order switching,
  `X-Forwarded-For` handling, the backend response-header timeout, the
  `HEALTH_TIMEOUT_MS > 0` rule and the cache-directory warning.
- **VERSION:** 8.2.11.
