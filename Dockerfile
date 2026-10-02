# Go stages cross-compile on the build platform: no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS source
WORKDIR /app
COPY go.* ./
RUN go mod download
COPY . .

# Only built with --target test.
FROM source AS test
RUN make fmt-check vet
RUN make test

FROM source AS builder
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/go2ptz ./cmd/go2ptz \
 && mkdir /out/data

FROM scratch

COPY --from=builder /out/go2ptz /go2ptz
# Named volumes inherit this ownership; bind mounts must be writable by 65532.
COPY --from=builder --chown=65532:65532 /out/data /data

ENV LISTEN_ADDR=:8080 \
    DATA_DIR=/data
USER 65532:65532
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/go2ptz", "healthcheck"]
ENTRYPOINT ["/go2ptz"]
