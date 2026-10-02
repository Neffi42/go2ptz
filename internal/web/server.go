// Package web serves the viewer page, the PTZ/snapshot JSON API and a
// restricted proxy to go2rtc for the video stream.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"code.neffi.fr/Neffi42/go2ptz/internal/go2rtc"
	"code.neffi.fr/Neffi42/go2ptz/internal/ptz"
)

//go:embed static
var static embed.FS

// moveTimeout bounds each continuous move so a lost Stop is harmless; the page
// re-sends while a control is held.
const moveTimeout = 2 * time.Second

// PTZ is the subset of *ptz.Camera the server needs.
type PTZ interface {
	Move(ctx context.Context, v ptz.Velocity, d time.Duration) error
	Stop(ctx context.Context) error
	Presets(ctx context.Context) ([]ptz.Preset, error)
	GotoPreset(ctx context.Context, token string) error
	SavePreset(ctx context.Context, name, token string) (string, error)
	RemovePreset(ctx context.Context, token string) error
}

type Config struct {
	Go2RTC  *url.URL // go2rtc API root, reachable from this process
	Stream  string   // go2rtc stream name
	DataDir string   // snapshot directory
}

type Server struct {
	cfg  Config
	cam  atomic.Pointer[PTZ]
	http *http.Client
	mux  *http.ServeMux
}

func New(cfg Config) *Server {
	s := &Server{
		cfg: cfg,
		http: &http.Client{
			Timeout: 15 * time.Second,
			// go2rtc never redirects; a redirect means an auth layer in front of it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		mux: http.NewServeMux(),
	}

	page, _ := fs.Sub(static, "static")
	s.mux.Handle("GET /{$}", http.FileServerFS(page))

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /readyz", s.ready)

	s.mux.HandleFunc("POST /api/move", s.withCam(s.move))
	s.mux.HandleFunc("POST /api/stop", s.withCam(func(w http.ResponseWriter, r *http.Request, c PTZ) {
		reply(w, nil, c.Stop(r.Context()))
	}))
	s.mux.HandleFunc("GET /api/presets", s.withCam(func(w http.ResponseWriter, r *http.Request, c PTZ) {
		p, err := c.Presets(r.Context())
		reply(w, p, err)
	}))
	s.mux.HandleFunc("POST /api/presets", s.withCam(s.savePreset))
	s.mux.HandleFunc("POST /api/presets/{token}/goto", s.withCam(func(w http.ResponseWriter, r *http.Request, c PTZ) {
		reply(w, nil, c.GotoPreset(r.Context(), r.PathValue("token")))
	}))
	s.mux.HandleFunc("DELETE /api/presets/{token}", s.withCam(func(w http.ResponseWriter, r *http.Request, c PTZ) {
		reply(w, nil, c.RemovePreset(r.Context(), r.PathValue("token")))
	}))

	s.mux.HandleFunc("POST /api/snapshots", s.snapshot)
	s.mux.HandleFunc("GET /api/snapshots", s.listSnapshots)
	s.mux.HandleFunc("DELETE /api/snapshots/{name}", s.deleteSnapshot)
	s.mux.HandleFunc("GET /snapshots/{name}", s.serveSnapshot)

	// Only the player and its websocket: the rest of the go2rtc API can edit config.
	proxy := s.go2rtcProxy()
	for _, p := range []string{"GET /go2rtc/video-stream.js", "GET /go2rtc/video-rtc.js", "GET /go2rtc/api/ws"} {
		s.mux.Handle(p, proxy)
	}
	return s
}

// SetCamera makes PTZ endpoints available; until then they return 503.
func (s *Server) SetCamera(c PTZ) { s.cam.Store(&c) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// sameOrigin is the only CSRF barrier: the API is unauthenticated.
func sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet && r.Header.Get("Upgrade") == "" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "" || r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	ok := s.cam.Load() != nil
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(map[string]bool{"camera": ok})
}

func (s *Server) withCam(h func(http.ResponseWriter, *http.Request, PTZ)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := s.cam.Load()
		if c == nil {
			http.Error(w, "camera not connected", http.StatusServiceUnavailable)
			return
		}
		h(w, r, *c)
	}
}

func (s *Server) move(w http.ResponseWriter, r *http.Request, c PTZ) {
	var v struct{ Pan, Tilt float64 }
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// One axis at a time: the Tapo C200 rejects combined pan+tilt moves.
	if v.Pan != 0 && v.Tilt != 0 {
		http.Error(w, "move one axis at a time", http.StatusBadRequest)
		return
	}
	reply(w, nil, c.Move(r.Context(), ptz.Velocity{Pan: v.Pan, Tilt: v.Tilt}, moveTimeout))
}

func (s *Server) savePreset(w http.ResponseWriter, r *http.Request, c PTZ) {
	var req struct{ Name, Token string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tok, err := c.SavePreset(r.Context(), strings.TrimSpace(req.Name), req.Token)
	reply(w, map[string]string{"token": tok}, err)
}

var snapshotName = regexp.MustCompile(`^snapshot-\d{8}T\d{6}(\.\d{3})?Z\.jpg$`)

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	jpeg, err := go2rtc.Frame(r.Context(), s.http, s.cfg.Go2RTC.String(), s.cfg.Stream)
	if err != nil {
		reply(w, nil, err)
		return
	}
	// UTC keeps names sortable and avoids needing tzdata in the scratch image.
	name := "snapshot-" + time.Now().UTC().Format("20060102T150405.000Z") + ".jpg"
	if err := writeAtomic(filepath.Join(s.cfg.DataDir, name), jpeg); err != nil {
		slog.Error("snapshot write", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("snapshot saved", "name", name, "bytes", len(jpeg))
	reply(w, map[string]string{"name": name, "url": "snapshots/" + name}, nil)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// listSnapshots returns a page of snapshot names, newest first:
// ?offset=N&limit=M (default 0 and 5, limit capped at 100).
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := queryInt(r, "limit", 5)
	if err != nil || limit < 1 {
		http.Error(w, "limit: must be a positive integer", http.StatusBadRequest)
		return
	}
	limit = min(limit, 100)

	entries, err := os.ReadDir(s.cfg.DataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	names := []string{}
	for _, e := range entries {
		if e.Type().IsRegular() && snapshotName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Reverse(names) // ReadDir sorts ascending; newest first

	start := min(offset, len(names))
	end := min(start+limit, len(names))
	reply(w, map[string]any{"names": names[start:end], "more": end < len(names)}, nil)
}

func queryInt(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: must be a non-negative integer", key)
	}
	return n, nil
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !snapshotName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	if err := os.Remove(filepath.Join(s.cfg.DataDir, name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("snapshot deleted", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !snapshotName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(".jpg"))
	// iOS Safari ignores <a download>; the header makes "save" work there too.
	if r.URL.Query().Has("download") {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	http.ServeFile(w, r, filepath.Join(s.cfg.DataDir, name))
}

func (s *Server) go2rtcProxy() http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(s.cfg.Go2RTC)
			pr.Out.URL.Path = strings.TrimSuffix(s.cfg.Go2RTC.Path, "/") + strings.TrimPrefix(pr.In.URL.Path, "/go2rtc")
			pr.Out.URL.RawPath = ""
			// Pin the stream: clients cannot pick other go2rtc streams or sources.
			pr.Out.URL.RawQuery = url.Values{"src": {s.cfg.Stream}}.Encode()
			// go2rtc checks Origin against its Host; ServeHTTP already checked it.
			pr.Out.Header.Del("Origin")
		},
		// A redirect means an auth layer; the browser would run the login page as a script.
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				return fmt.Errorf("go2rtc redirected to %s: GO2RTC_URL must reach go2rtc without interactive auth", resp.Header.Get("Location"))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("go2rtc proxy", "path", r.URL.Path, "err", err)
			http.Error(w, "go2rtc unreachable", http.StatusBadGateway)
		},
	}
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		var fault *ptz.FaultError
		if errors.As(err, &fault) {
			// Some cameras (Tapo) send an empty Reason; fall back to the codes.
			msg := fault.Reason
			if msg == "" {
				msg = strings.Trim(fault.Code+" "+fault.Subcode, " ")
			}
			if msg == "" {
				msg = fmt.Sprintf("fault (http %d)", fault.Status)
			}
			err = fmt.Errorf("camera: %s", msg)
		}
		slog.Warn("request failed", "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
