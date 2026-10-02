# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

It is also exposed as `AGENTS.md` (symlink) and applies to any AI coding agent working here.

## Project

go2ptz is a lightweight Go sidecar for [go2rtc](https://github.com/AlexxIT/go2rtc): ONVIF PTZ control plus a minimal web viewer for camera streams.

Single binary, single camera. `cmd/go2ptz` reads env config, serves `internal/web`, and dials the camera in the background with backoff so the page and stream work while the camera is down (PTZ endpoints return 503 until connected).

Env: `ONVIF_ADDRESS` (required; Tapo cameras use port 2020), `ONVIF_USERNAME`, `ONVIF_PASSWORD`, `ONVIF_PROFILE`, `GO2RTC_STREAM` (required), `GO2RTC_URL` (default `http://127.0.0.1:1984`, plain http only, must be reachable from the sidecar, not the browser), `LISTEN_ADDR` (`:8080`), `DATA_DIR` (`/data`).

- Module path: `code.neffi.fr/Neffi42/go2ptz`
- Go 1.27.1 (pinned in both `go.mod` and `mise.toml`; keep them in sync with the `golang` image tag in `Dockerfile`)

## ONVIF

No ONVIF library: `internal/ptz` speaks SOAP 1.2 directly over `net/http` and is the only place ONVIF lives. Requests are hand-built XML strings (escape text with `el()`); responses decode with `encoding/xml` on local names, ignoring namespaces.

- `ptz.Dial` measures device clock skew (unauthenticated `GetSystemDateAndTime`), reads Media/PTZ XAddrs from `GetCapabilities`, and binds the `Camera` to one PTZ-capable media profile. The WS-Security `Created` timestamp is shifted by that skew; devices reject skewed tokens. On an auth fault (`isAuthFailure`) `call` re-measures the skew and retries once, only if the device clock moved by at least `skewTolerance`; a wrong password is not retried.
- Advertised XAddr hosts are replaced with the dialed host (cameras behind NAT advertise unreachable addresses); only the path is kept.
- Auth is WS-Security UsernameToken PasswordDigest only; HTTP Digest is not implemented.
- No zoom or home position: `Move` sends only `PanTilt` (fixed-lens cameras can fault on a `Zoom` element). `Stop` keeps `<Zoom>true</Zoom>`, which the C200 accepts.
- Device faults surface as `*ptz.FaultError` (e.g. `Subcode == "ter:NotAuthorized"`).
- Tests run against an in-process fake camera (`fakeCamera` in `camera_test.go`) that verifies the digest; extend it when adding operations.

Snapshots are not done over ONVIF: `internal/go2rtc.Frame` fetches go2rtc's `GET /api/frame.jpeg?src=<stream>`.

## Web

`internal/web` serves the embedded `static/index.html` (no build step, vanilla JS module), the JSON API under `/api/`, `/healthz` (liveness) and `/readyz` (camera connected).

- `GO2RTC_URL` must not sit behind interactive auth. The proxy turns any upstream 3xx into a 502 instead of forwarding it, the snapshot client never follows redirects, and `go2rtc.Frame` rejects non-`image/jpeg` responses so a login page is never saved as a snapshot.
- The browser never talks to go2rtc directly. `/go2rtc/` is a reverse proxy allowlisted to `video-stream.js`, `video-rtc.js` and `api/ws`, with `src` overwritten to `GO2RTC_STREAM` and `Origin` stripped (go2rtc compares it to its own Host). Do not widen it to the whole go2rtc API: that exposes config editing and arbitrary sources. WebRTC media still flows browser to go2rtc directly; MSE goes through the proxy.
- The API is unauthenticated; `Server.ServeHTTP` rejects cross-origin non-GET and websocket requests as the CSRF barrier.
- Target camera is a Tapo C200: no zoom, no working home position, and combined pan+tilt moves fail, so the UI exposes only single-axis pan/tilt and `/api/move` rejects diagonals.
- Continuous moves are always sent with a 2s ONVIF timeout; the page re-sends every 1s while a control is held, so a lost Stop is bounded.
- Snapshots are written atomically to `DATA_DIR` as `snapshot-<UTC>.jpg`; only names matching `snapshotName` are listed, served or deleted. `GET /api/snapshots` is paged (`offset`/`limit`, returns `{names, more}`); the page loads 5 at a time.
- Page styling uses CSS custom properties on `:root`: dark by default, light via `prefers-color-scheme` unless `data-theme` forces one (stored in `localStorage` only when not "system"). New colors must be added as tokens to all three blocks, not hardcoded.
- No browser tests in the repo. UI changes were verified with Playwright in Docker (`mcr.microsoft.com/playwright/python`) against a go2rtc container serving `ffmpeg:virtual?video&size=720#video=h264`.

## Tooling

Toolchain is managed by mise (`mise.toml`): go, gopls, docker-language-server.

```sh
mise install                 # install pinned tools
go build ./cmd/go2ptz        # build binary (entrypoint location expected by Dockerfile)
make fmt                     # gofmt -w
make check                   # fmt-check + vet + test (-race when supported; RACE=- disables); also fmt-check, vet, test targets
go test ./internal/ptz -run TestCamera  # single test
ONVIF_ADDRESS=cam.lan GO2RTC_STREAM=cam1 DATA_DIR=./data go run ./cmd/go2ptz
docker build --target test .  # what CI runs: `make check` steps in the container
docker build -t go2ptz .     # container image
```

## Build constraints

- `Dockerfile` stages: `source` (digest-pinned `golang`, deps + code), `test` (runs the `Makefile` check targets, only built with `--target test`; keep the Makefile and this stage in sync), `builder` (`CGO_ENABLED=0`), then `FROM scratch`. The binary must be fully static and must not depend on anything from the filesystem (no tzdata, shell, or `/tmp`) unless it is explicitly copied into the runner stage or embedded (e.g. `embed` for web viewer assets, `time/tzdata`). There is no CA bundle: both upstreams are plain http, and config parsing rejects other schemes for `ONVIF_ADDRESS` (`ptz.CheckEndpoint`) and `GO2RTC_URL`. Adding https support means copying a CA bundle back in.
- The runner stage uses `USER 65532:65532` (numeric, as `scratch` has no `/etc/passwd`). The process cannot bind ports below 1024 or write anywhere except `/data`, which is pre-created with that owner so named volumes inherit it.
- `HEALTHCHECK` runs `/go2ptz healthcheck` (GETs `/healthz` on `LISTEN_ADDR`'s port), since scratch has no curl.
- The main package must live at `cmd/go2ptz`.
- No third-party deps, so there is no `go.sum`; the Dockerfile copies `go.*` so it works either way.
- `.dockerignore` excludes repo metadata, docs, CI and agent files, and `mise.toml`; it does not need updating for new source directories.

## CI

`.forgejo/workflows/ci.yaml`, one job on the `docker` runner label: a forgejo-runner k8s plugin pod (`ghcr.io/bjw-s-labs/forgejo-runner:ubuntu-24.04`, ships docker CLI + buildx) with a privileged `docker:29-dind` sidecar. Constraints that shape the workflow:

- `DOCKER_HOST=tcp://localhost:2376` with TLS is preset; never override it or use a socket. Wait for `docker info` before the first docker call. `docker buildx create` refuses TLS env vars, so the builder is created on an explicit `docker context` built from `DOCKER_HOST`/`DOCKER_CERT_PATH`.
- No job-level `container:`/`services:`; bind mounts resolve on the sidecar, so all Go work happens inside `docker buildx build` (the `test` target).
- Node arch varies (arm64 usually, amd64 on failover): always pass `--platform`. Multi-arch works without QEMU because the Go stages use `--platform=$BUILDPLATFORM` and cross-compile; keep any `RUN` out of target-platform stages.
- Some arm64 nodes have 39-bit-VA kernels where ThreadSanitizer cannot start (`FATAL: ThreadSanitizer: unsupported VMA range`), so `make test` probes and drops `-race` with a warning there. Race coverage comes from amd64/48-bit nodes and local runs.
- No persistent Docker storage; cache goes to the registry (`$IMAGE:buildcache`). Job container 1 CPU/2Gi, sidecar 2 CPU/2Gi.
- Published `v*` releases (the `release` event, not tag pushes) push `<registry>/<owner lowercased>/go2ptz:<version>` (plus `latest` for plain `vX.Y.Z` non-prereleases) with the `REGISTRY_TOKEN` secret.
