package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"code.neffi.fr/Neffi42/go2ptz/internal/ptz"
)

type fakePTZ struct{ moves []ptz.Velocity }

func (f *fakePTZ) Move(_ context.Context, v ptz.Velocity, d time.Duration) error {
	f.moves = append(f.moves, v)
	return nil
}
func (f *fakePTZ) Stop(context.Context) error { return nil }
func (f *fakePTZ) Presets(context.Context) ([]ptz.Preset, error) {
	return []ptz.Preset{{Token: "1", Name: "door"}}, nil
}
func (f *fakePTZ) GotoPreset(context.Context, string) error { return nil }
func (f *fakePTZ) SavePreset(_ context.Context, name, _ string) (string, error) {
	return "7", nil
}
func (f *fakePTZ) RemovePreset(context.Context, string) error { return nil }

func setup(t *testing.T) (*Server, *httptest.Server, *[]string) {
	var hits []string
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.RequestURI()+" origin="+r.Header.Get("Origin"))
		if r.URL.Path == "/api/frame.jpeg" {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("\xff\xd8jpeg"))
		}
	}))
	t.Cleanup(g.Close)
	u, _ := url.Parse(g.URL)
	s := New(Config{Go2RTC: u, Stream: "cam1", DataDir: t.TempDir()})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv, &hits
}

func do(t *testing.T, method, u string, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, u, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHealthAndReady(t *testing.T) {
	s, srv, _ := setup(t)
	if r := do(t, "GET", srv.URL+"/healthz", "", nil); r.StatusCode != 200 {
		t.Errorf("healthz = %d", r.StatusCode)
	}
	if r := do(t, "GET", srv.URL+"/readyz", "", nil); r.StatusCode != 503 {
		t.Errorf("readyz before camera = %d", r.StatusCode)
	}
	if r := do(t, "POST", srv.URL+"/api/stop", "", nil); r.StatusCode != 503 {
		t.Errorf("stop before camera = %d", r.StatusCode)
	}
	s.SetCamera(&fakePTZ{})
	if r := do(t, "GET", srv.URL+"/readyz", "", nil); r.StatusCode != 200 {
		t.Errorf("readyz after camera = %d", r.StatusCode)
	}
}

func TestPTZ(t *testing.T) {
	s, srv, _ := setup(t)
	cam := &fakePTZ{}
	s.SetCamera(cam)

	r := do(t, "POST", srv.URL+"/api/move", `{"pan":0.5}`, map[string]string{"Origin": srv.URL})
	if r.StatusCode != 204 || len(cam.moves) != 1 || cam.moves[0] != (ptz.Velocity{Pan: 0.5}) {
		t.Errorf("move = %d, %+v", r.StatusCode, cam.moves)
	}
	if r := do(t, "POST", srv.URL+"/api/move", `{"pan":1,"tilt":1}`, nil); r.StatusCode != 400 || len(cam.moves) != 1 {
		t.Errorf("diagonal move = %d, moves %+v", r.StatusCode, cam.moves)
	}
	if r := do(t, "POST", srv.URL+"/api/home", "", nil); r.StatusCode != 404 && r.StatusCode != 405 {
		t.Errorf("home still routed: %d", r.StatusCode)
	}
	if r := do(t, "POST", srv.URL+"/api/stop", "", map[string]string{"Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Errorf("cross-origin stop = %d", r.StatusCode)
	}
	r = do(t, "POST", srv.URL+"/api/presets", `{"name":"yard"}`, nil)
	var saved map[string]string
	json.NewDecoder(r.Body).Decode(&saved)
	if saved["token"] != "7" {
		t.Errorf("save preset = %v", saved)
	}
	if r := do(t, "DELETE", srv.URL+"/api/presets/1", "", nil); r.StatusCode != 204 {
		t.Errorf("delete preset = %d", r.StatusCode)
	}
}

func TestSnapshots(t *testing.T) {
	s, srv, _ := setup(t)
	r := do(t, "POST", srv.URL+"/api/snapshots", "", nil)
	var shot map[string]string
	json.NewDecoder(r.Body).Decode(&shot)
	if r.StatusCode != 200 || !snapshotName.MatchString(shot["name"]) {
		t.Fatalf("snapshot = %d %v", r.StatusCode, shot)
	}
	if b, _ := os.ReadFile(s.cfg.DataDir + "/" + shot["name"]); string(b) != "\xff\xd8jpeg" {
		t.Errorf("file content = %q", b)
	}

	var page snapshotPage
	json.NewDecoder(do(t, "GET", srv.URL+"/api/snapshots", "", nil).Body).Decode(&page)
	if len(page.Names) != 1 || page.Names[0] != shot["name"] || page.More {
		t.Errorf("list = %+v", page)
	}
	r = do(t, "GET", srv.URL+"/"+shot["url"], "", nil)
	if b, _ := io.ReadAll(r.Body); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/jpeg" || len(b) == 0 {
		t.Errorf("serve = %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := do(t, "GET", srv.URL+"/"+shot["url"]+"?download", "", nil); !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("download Content-Disposition = %q", r.Header.Get("Content-Disposition"))
	}
	if r := do(t, "GET", srv.URL+"/snapshots/..%2Fetc%2Fpasswd", "", nil); r.StatusCode != 404 {
		t.Errorf("traversal = %d", r.StatusCode)
	}

	if r := do(t, "DELETE", srv.URL+"/api/snapshots/"+shot["name"], "", map[string]string{"Origin": "https://evil.example"}); r.StatusCode != 403 {
		t.Errorf("cross-origin delete = %d", r.StatusCode)
	}
	if r := do(t, "DELETE", srv.URL+"/api/snapshots/"+shot["name"], "", nil); r.StatusCode != 204 {
		t.Errorf("delete = %d", r.StatusCode)
	}
	if _, err := os.Stat(s.cfg.DataDir + "/" + shot["name"]); !os.IsNotExist(err) {
		t.Errorf("file still present: %v", err)
	}
	if r := do(t, "DELETE", srv.URL+"/api/snapshots/"+shot["name"], "", nil); r.StatusCode != 404 {
		t.Errorf("delete missing = %d", r.StatusCode)
	}
	if r := do(t, "DELETE", srv.URL+"/api/snapshots/go2ptz.db", "", nil); r.StatusCode != 404 {
		t.Errorf("delete non-snapshot = %d", r.StatusCode)
	}
}

func TestProxyPinsStream(t *testing.T) {
	_, srv, hits := setup(t)
	do(t, "GET", srv.URL+"/go2rtc/video-stream.js", "", nil)
	do(t, "GET", srv.URL+"/go2rtc/api/ws?src=other&src=rtsp://x", "", map[string]string{"Origin": srv.URL})
	want := []string{"/video-stream.js?src=cam1 origin=", "/api/ws?src=cam1 origin="}
	if strings.Join(*hits, "|") != strings.Join(want, "|") {
		t.Errorf("go2rtc saw %q, want %q", *hits, want)
	}
	if r := do(t, "GET", srv.URL+"/go2rtc/api/config", "", nil); r.StatusCode != 404 {
		t.Errorf("go2rtc config exposed: %d", r.StatusCode)
	}
}

func TestIndex(t *testing.T) {
	_, srv, _ := setup(t)
	r := do(t, "GET", srv.URL+"/", "", nil)
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(b), "video-stream.js") {
		t.Errorf("index = %d", r.StatusCode)
	}
}

func TestGo2RTCBehindAuth(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>login</html>"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	t.Cleanup(auth.Close)
	u, _ := url.Parse(auth.URL)
	s := New(Config{Go2RTC: u, Stream: "cam1", DataDir: t.TempDir()})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	r := do(t, "GET", srv.URL+"/go2rtc/video-stream.js", "", nil)
	if r.StatusCode != http.StatusBadGateway || r.Header.Get("Location") != "" {
		t.Errorf("proxy forwarded auth redirect: %d Location=%q", r.StatusCode, r.Header.Get("Location"))
	}

	r = do(t, "POST", srv.URL+"/api/snapshots", "", nil)
	if r.StatusCode != http.StatusBadGateway {
		t.Errorf("snapshot through auth redirect = %d", r.StatusCode)
	}
	if entries, _ := os.ReadDir(s.cfg.DataDir); len(entries) != 0 {
		t.Errorf("login page saved as snapshot: %v", entries)
	}
}

func TestSnapshotRejectsNonJPEG(t *testing.T) {
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>error</html>"))
	}))
	t.Cleanup(g.Close)
	u, _ := url.Parse(g.URL)
	s := New(Config{Go2RTC: u, Stream: "cam1", DataDir: t.TempDir()})
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	if r := do(t, "POST", srv.URL+"/api/snapshots", "", nil); r.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d", r.StatusCode)
	}
	if entries, _ := os.ReadDir(s.cfg.DataDir); len(entries) != 0 {
		t.Errorf("non-JPEG saved: %v", entries)
	}
}

type snapshotPage struct {
	Names []string `json:"names"`
	More  bool     `json:"more"`
}

func TestSnapshotPagination(t *testing.T) {
	s, srv, _ := setup(t)
	var all []string
	for i := range 12 {
		name := fmt.Sprintf("snapshot-20260101T0000%02dZ.jpg", i)
		os.WriteFile(s.cfg.DataDir+"/"+name, []byte("x"), 0o644)
		all = append([]string{name}, all...) // newest first
	}
	os.WriteFile(s.cfg.DataDir+"/other.txt", []byte("x"), 0o644)

	get := func(q string) (snapshotPage, int) {
		r := do(t, "GET", srv.URL+"/api/snapshots"+q, "", nil)
		var p snapshotPage
		json.NewDecoder(r.Body).Decode(&p)
		return p, r.StatusCode
	}
	for _, c := range []struct {
		q     string
		names []string
		more  bool
	}{
		{"", all[:5], true},
		{"?offset=5", all[5:10], true},
		{"?offset=10", all[10:], false},
		{"?offset=50", []string{}, false},
		{"?limit=100", all, false},
	} {
		p, code := get(c.q)
		if code != 200 || strings.Join(p.Names, ",") != strings.Join(c.names, ",") || p.More != c.more {
			t.Errorf("%q = %d %+v, want %v more=%v", c.q, code, p, c.names, c.more)
		}
	}
	for _, q := range []string{"?offset=-1", "?limit=0", "?limit=x"} {
		if _, code := get(q); code != 400 {
			t.Errorf("%q = %d, want 400", q, code)
		}
	}
}

func TestFaultMessage(t *testing.T) {
	for _, c := range []struct {
		fault *ptz.FaultError
		want  string
	}{
		{&ptz.FaultError{Reason: "busy"}, "camera: busy"},
		{&ptz.FaultError{Code: "env:Receiver", Subcode: "ter:ActionNotSupported"}, "camera: env:Receiver ter:ActionNotSupported"},
		{&ptz.FaultError{Status: 500}, "camera: fault (http 500)"},
	} {
		w := httptest.NewRecorder()
		reply(w, nil, c.fault)
		if got := strings.TrimSpace(w.Body.String()); got != c.want {
			t.Errorf("reply(%+v) = %q, want %q", c.fault, got, c.want)
		}
	}
}
