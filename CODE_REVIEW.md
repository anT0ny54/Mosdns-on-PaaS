# Code Review — 2026-09-28

Full-repository read-through of every file (Go sources, tests, Dockerfile,
entrypoint, mosdns config, config-check script, Makefile, workflow, and
docs) covering the gateway version tagged `0.4.0` / MosDNS `v4.5.3`.

## Architecture recap

- `entrypoint.sh` starts two children under a tiny supervisor loop: MosDNS
  (resolving via two racing HaGeZi DoH upstreams, with a third as
  `fast_fallback` after 400ms — see `mosdns.yaml`) on loopback `:8081`, and
  the Go `doh-gateway` (`main.go`) as the sole public listener on `$PORT`.
  If either child dies, both are killed and the container exits `1`
  (correct fail-fast behavior for a single-process-per-instance PaaS).
- `main.go` implements: per-client fixed-window rate limiting, a two-stage
  admission control scheme (a large "active request" burst ceiling backed
  by a smaller "processing slot" concurrency queue with a bounded wait),
  strict DoH GET/POST validation (RFC 8484 unpadded base64url, exact
  `application/dns-message` content type, 4096-byte message cap), hop-by-hop
  and `Connection`-listed header stripping, a live DNS readiness probe
  through MosDNS, and a hand-rolled Prometheus `/metrics` exposition.

## Bugs found and fixed

1. **`main_test.go: TestDefaultCapacityConfiguration`** — the `t.Fatalf`
   message read `"...want 32"` but the assertion it guards actually checks
   `defaultConcurrency != 20`. Harmless in practice (the constant is 20 and
   the check itself was correct), but the message would have misled anyone
   reading a failure. **Fix:** the message now says `"want 20"`, matching
   the assertion.

2. **`main.go: backendReady` / `isUsableDNSProbeResponse`** — the synthetic
   readiness query embedded a transaction ID (`buildReadinessDNSQuery`),
   but the response validator never checked that the reply's ID matched
   the query's ID; it only checked the QR bit and RCODE. Not exploitable in
   the shipped deployment (it's a direct 1:1 HTTP round trip over loopback),
   but the code implied a correlation check that wasn't actually performed,
   and a future change (e.g. a shared/multiplexed connection to a different
   backend) could have made that gap matter. **Fix:**
   - `buildReadinessDNSQuery` now returns the transaction ID it embedded,
     alongside the query bytes.
   - `isUsableDNSProbeResponse` takes that ID and rejects any response
     whose first two wire bytes don't echo it.
   - `backendReady` threads the ID through from build to check.
   - `TestReadyEndpointTracksBackend` and `TestReadyEndpointRejectsServfail`
     were updated to echo the *real* transaction ID (decoded from the
     probe's `dns` query parameter) instead of a hardcoded guess — the old
     hardcoded value only worked because each test's probe happened to be
     the first one issued by a freshly constructed `gateway`, which is a
     hidden coupling worth removing regardless of the new check.
   - Added `TestReadyEndpointRejectsMismatchedID`, which returns a
     well-formed, non-SERVFAIL response with the wrong transaction ID and
     asserts `/readyz` still reports not-ready.

## Minor gap found and fixed

- `/metrics`'s response-class counters (`doh_gateway_responses_total{class=...}`)
  only branched on 2xx/4xx/5xx. A 3xx from the backend would have been
  proxied to the client untouched but wasn't counted anywhere (not in the
  class counter, not in `backend_error_total`). MosDNS's `fast_forward`/HTTP
  listener shouldn't produce 3xx today, so impact was low, but it was a
  blind spot. **Fix:** added a `responses3xxTotal` counter, a matching
  branch in `serveDNS`'s status-class switch, and a `class="3xx"` line in
  `serveMetrics`'s exposition output.

## Redundant / defensive-only code (left as-is — not dead, just unreachable today)

- `isValidDoHGetParameter`'s `maxBytes > maxDNSMessageBytes` guard: `main()`
  always passes `maxDNSMessageBytes` itself as `g.maxRequestBytes`, so this
  only matters if that field is ever made configurable via an env var.
- `backendReady`'s `if client == nil { client = g.client }` fallback:
  `main()` always sets `readyClient`, so this only fires for hand-built
  `gateway{}` literals in tests.

Both are exercised by the test suite and are cheap, so there's no reason to
remove them — just noting they're not reachable from the shipped binary.

## Consistency check (docs vs. code vs. config)

No drift found. Specifically verified:

- `go.mod` (`go 1.19`) vs. Dockerfile `ARG GO_VERSION=1.19.13` — consistent.
- `VERSION` (`0.4.0`) vs. Dockerfile `ARG GATEWAY_VERSION=0.4.0` and the
  `org.opencontainers.image.version` label — consistent.
- Every numeric default documented in `README.md` (rate limit/window,
  rate-limit client cap, active-request cap, processing concurrency, queue
  wait, upstream timeout, HTTP timeouts, `GOMAXPROCS`/`GOGC`/`GOMEMLIMIT`,
  MosDNS cache size/TTLs) matches the Dockerfile `ENV` block and
  `mosdns.yaml` exactly.
- `check-config.sh`'s schema checks match `mosdns.yaml`'s actual shape
  field-for-field (cache args, the 2-primary + 1-fallback upstream
  topology, the `fast_fallback` chain, the single HTTP listener) — running
  it against the shipped config passes cleanly.

## Optimization notes (no action taken — cosmetic only)

- The DoH GET path's base64url validator (`isValidDoHGetParameter`) decodes
  into a pooled scratch array purely to reject invalid characters/padding;
  the decoded bytes are never forwarded anywhere (the original query string
  is reused as-is for the upstream request). This is intentional and
  already allocation-free, enforced by `TestDoHGETParameterValidationAllocs`.
- The mosdns-facing HTTP transport (`newHTTPClient`) carries a
  `TLSHandshakeTimeout` even though that connection is always plaintext
  loopback HTTP — dead weight, but negligible at this scale.
- `entrypoint.sh` polls child liveness with `sleep 2` in a loop rather than
  `wait -n`; this is the more portable choice for Alpine's `busybox ash` and
  is an intentional trade-off, not something to change.

## Verification

No Go toolchain or network access was available in this environment, so
the changes were not compiled or run here. They were kept small and
mechanical (a message string, a counter/branch/output triple that mirrors
the existing 2xx/4xx/5xx pattern exactly, and a function-signature change
threaded through its one call site and its three test call sites) and
reviewed line-by-line against the existing style. **Before merging, run
`make test` (which runs `check-config.sh`, `go vet ./...`, and `go test
./...`) to confirm.**

## Bottom line

No functional bugs affect the shipped gateway's steady-state request path
at the resource limits (0.1 vCPU / 512 MiB) this project targets. The two
issues found were in the readiness probe's response validation and the
metrics/test code, both now fixed; the documentation was and remains
tightly synchronized with the actual defaults in the Dockerfile and
`mosdns.yaml`.
