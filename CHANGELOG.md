# Changelog

## 0.5.21

#### Fixed
- Static URL validation now rejects malformed percent escapes throughout the
  complete URL, including the path and authority. Python's `urlsplit` does not
  perform this validation, while Go's `url.Parse` does; the static checker now
  matches gateway startup behavior for upstream URLs and the Dockerfile backend
  URL.

#### Changed
- README: documented full-URL percent-escape validation.
- Bumped `VERSION` and the Dockerfile's matching `GATEWAY_VERSION` default to
  0.5.21.

#### Verified
- Config-validator regression suite: 62 cases, including malformed percent
  escapes in upstream authority/path and Dockerfile backend path, plus valid
  percent-encoded path acceptance.
- Go formatting, vet, race-enabled tests, and build passed using the installed
  Go 1.23.2 compiler in a temporary validation copy. The project's Go 1.27
  requirement and `go.mod` remain unchanged.
- Entrypoint regression suites passed under POSIX `sh` and BusyBox `ash`.
- Docker image build not run: Docker is unavailable in this environment.
