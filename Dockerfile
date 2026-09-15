# Builder
FROM golang:1.27.1@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS builder
ARG CGO_ENABLED=0
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build /app/cmd/go2ptz

# Runner
FROM scratch

COPY --from=builder /app/go2ptz /go2ptz
ENTRYPOINT ["/go2ptz"]
