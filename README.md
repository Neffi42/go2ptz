# go2ptz

Lightweight Go sidecar for [go2rtc](https://github.com/AlexxIT/go2rtc): ONVIF PTZ control and a web viewer for one camera stream. Live view, pan/tilt, presets, snapshots. Built for a Tapo C200.

## Run

```yaml
services:
  go2rtc:
    image: alexxit/go2rtc
    restart: unless-stopped
    configs:
      - source: go2rtc
        target: /config/go2rtc.yaml
    ports:
      - "8555:8555/tcp" # WebRTC media, browser -> go2rtc directly
      - "8555:8555/udp"

  go2ptz:
    image: code.neffi.fr/neffi42/go2ptz:latest
    restart: unless-stopped
    environment:
      ONVIF_ADDRESS: 192.168.1.50:2020
      ONVIF_USERNAME: camera-user
      ONVIF_PASSWORD: camera-password
      GO2RTC_URL: http://go2rtc:1984
      GO2RTC_STREAM: camera1
    volumes:
      - snapshots:/data
    ports:
      - "8080:8080"

configs:
  go2rtc:
    content: |
      streams:
        camera1: rtsp://camera-user:camera-password@192.168.1.50:554/stream1
      webrtc:
        candidates:
          - 192.168.1.10:8555 # Docker host IP the browser can reach

volumes:
  snapshots:
```

go2rtc's API (1984) stays unpublished: the browser only talks to go2ptz, except for WebRTC media on 8555. Without a reachable candidate, playback falls back to MSE through go2ptz. Inline `configs.content` needs Docker Compose 2.23+.

| Variable                            | Default                 |                                                                        |
| ----------------------------------- | ----------------------- | ---------------------------------------------------------------------- |
| `ONVIF_ADDRESS`                     | required                | `host`, `host:port` or `http://` URL                                   |
| `ONVIF_USERNAME` / `ONVIF_PASSWORD` |                         |                                                                        |
| `ONVIF_PROFILE`                     | first PTZ profile       | profile token or name                                                  |
| `GO2RTC_URL`                        | `http://127.0.0.1:1984` | go2rtc API, plain `http`, reachable from go2ptz, no login/forward auth |
| `GO2RTC_STREAM`                     | required                | go2rtc stream name                                                     |
| `LISTEN_ADDR`                       | `:8080`                 |                                                                        |
| `DATA_DIR`                          | `/data`                 | snapshot directory                                                     |

`/healthz` is liveness, `/readyz` returns 503 until the camera is connected.

## Notes

- Runs as UID `65532`: bind-mounted `/data` must be writable by it.
- ONVIF auth is WS-Security digest only; HTTP Digest-only cameras are unsupported.

## Development

```sh
mise install
make fmt                       # gofmt -w
make check                     # gofmt check, vet, tests with -race when the host supports it (what CI runs)
docker build --target test .   # same, in the CI container
```

## License

MIT
