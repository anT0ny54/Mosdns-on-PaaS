# Changelog

## 0.5.15

#### Changed
- **Prevent HTTP/1 cleanup from waiting on unread uploads.** The previous
  `Connection: close` response header marked the connection for closure, but
  Go's HTTP/1 server could still try to read part of an unread request body
  while closing it. Early rejection paths now also set the request read
  deadline to the current time through `http.ResponseController`. This makes
  the server's post-handler body cleanup stop promptly without changing the
  response write deadline. The behavior is limited to HTTP/1; HTTP/2
  connection handling remains unchanged.
- **Add a live HTTP/1 socket regression test.** It sends headers for a 1 KiB
  upload without sending the body, verifies that the gateway returns `415`
  promptly, and checks that the connection reaches EOF. A bodyless health
  request is also checked to ensure it does not get an unnecessary close.
- **Clarify the README's unread-body policy** for unknown routes, read-only
  endpoints, rejected methods, rate/concurrency limits, unsupported media
  types, and oversized uploads, while retaining the HTTP/1 versus HTTP/2
  distinction.

#### Reviewed, no further change required
- Re-read all 23 files in the merged project and cross-checked the production
  request paths, deployment/configuration files, scripts, tests, and docs.
  No further demonstrably dead production identifiers, duplicate code paths,
  or actionable configuration conflicts were found.

#### Verified
- The live HTTP/1 unread-body regression and bodyless health regression pass.
- `go test ./...`, `go test -race ./...`, `go vet ./...`, and `gofmt` pass
  using the installed Go 1.23.2 with an external temporary `go.mod` declaring
  Go 1.23. The repository still declares Go 1.27; that toolchain could not be
  downloaded because network access is unavailable, so Go 1.27 itself was not
  exercised.
- `sh check-config.sh mosdns.yaml` and shell syntax checks pass. The config
  regression script's YAML cases and initial Dockerfile cases pass; because
  the full script exceeded the execution limit, its remaining Dockerfile
  mutations (invalid port, user-info, fragments, empty port, control
  character, and version mismatch) were run separately and all passed.
- `sh test-entrypoint.sh` passes all supervision scenarios under POSIX `sh`
  and BusyBox `ash`.
- Docker image build remains unverified because Docker is unavailable.
  `VERSION` remains `0.5.15`; no project version bump was made.

