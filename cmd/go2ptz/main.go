package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"code.neffi.fr/Neffi42/go2ptz/internal/ptz"
	"code.neffi.fr/Neffi42/go2ptz/internal/web"
)

type config struct {
	listen  string
	dataDir string
	go2rtc  *url.URL
	stream  string
	onvif   ptz.Config
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	cfg := config{
		listen:  env("LISTEN_ADDR", ":8080"),
		dataDir: env("DATA_DIR", "/data"),
		stream:  os.Getenv("GO2RTC_STREAM"),
		onvif: ptz.Config{
			Endpoint: os.Getenv("ONVIF_ADDRESS"),
			Username: os.Getenv("ONVIF_USERNAME"),
			Password: os.Getenv("ONVIF_PASSWORD"),
			Profile:  os.Getenv("ONVIF_PROFILE"),
		},
	}
	var errs []error
	if cfg.onvif.Endpoint == "" {
		errs = append(errs, errors.New("ONVIF_ADDRESS is required"))
	} else if err := ptz.CheckEndpoint(cfg.onvif.Endpoint); err != nil {
		errs = append(errs, fmt.Errorf("ONVIF_ADDRESS: %w", err))
	}
	if cfg.stream == "" {
		errs = append(errs, errors.New("GO2RTC_STREAM is required"))
	}
	u, err := url.Parse(env("GO2RTC_URL", "http://127.0.0.1:1984"))
	switch {
	case err != nil || u.Host == "":
		errs = append(errs, errors.New("GO2RTC_URL: invalid URL"))
	case u.Scheme != "http":
		// The image ships no CA bundle.
		errs = append(errs, fmt.Errorf("GO2RTC_URL: unsupported scheme %q, only http", u.Scheme))
	}
	cfg.go2rtc = u
	return cfg, errors.Join(errs...)
}

func run(cfg config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}

	srv := web.New(web.Config{Go2RTC: cfg.go2rtc, Stream: cfg.stream, DataDir: cfg.dataDir})
	go connect(ctx, cfg.onvif, srv)

	hs := &http.Server{Addr: cfg.listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.listen, "stream", cfg.stream, "onvif", cfg.onvif.Endpoint)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	return hs.Shutdown(shutdown)
}

// connect dials the camera with backoff; PTZ endpoints return 503 until it succeeds.
func connect(ctx context.Context, cfg ptz.Config, srv *web.Server) {
	backoff := 2 * time.Second
	for {
		cam, err := ptz.Dial(ctx, cfg)
		if err == nil {
			slog.Info("camera connected", "profile", cam.ProfileToken())
			srv.SetCamera(cam)
			return
		}
		slog.Warn("camera connect failed", "err", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// healthcheck backs the container HEALTHCHECK: scratch has no curl.
func healthcheck() int {
	_, port, err := net.SplitHostPort(env("LISTEN_ADDR", ":8080"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}
