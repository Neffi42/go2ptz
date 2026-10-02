package ptz

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestXSDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Second:         "PT2S",
		1500 * time.Millisecond: "PT1.5S",
		90 * time.Second:        "PT90S",
	} {
		if got := xsDuration(d); got != want {
			t.Errorf("xsDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestClamp(t *testing.T) {
	for in, want := range map[float64]float64{-3: -1, -0.5: -0.5, 0: 0, 2: 1} {
		if got := clamp(in); got != want {
			t.Errorf("clamp(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestDeviceURL(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.5":                          "http://10.0.0.5/onvif/device_service",
		"10.0.0.5:8000":                     "http://10.0.0.5:8000/onvif/device_service",
		"http://cam.lan/":                   "http://cam.lan/onvif/device_service",
		"http://cam.lan:80/onvif/device_ws": "http://cam.lan:80/onvif/device_ws",
	} {
		u, err := deviceURL(in)
		if err != nil || u.String() != want {
			t.Errorf("deviceURL(%q) = %v, %v; want %q", in, u, err, want)
		}
	}
	for _, in := range []string{"", "https://cam.lan/", "rtsp://cam.lan"} {
		if _, err := deviceURL(in); err == nil {
			t.Errorf("deviceURL(%q) want error", in)
		}
	}
}

func TestPickProfile(t *testing.T) {
	ptz := &struct {
		Token string `xml:"token,attr"`
	}{}
	profiles := []profile{
		{Token: "main", Name: "MainStream"},
		{Token: "ptz1", Name: "PTZ", PTZ: ptz},
		{Token: "ptz2", Name: "Sub", PTZ: ptz},
	}
	cases := []struct {
		want, token string
		err         error
	}{
		{"", "ptz1", nil},
		{"ptz2", "ptz2", nil},
		{"Sub", "ptz2", nil},
		{"main", "", ErrNoPTZProfile},
		{"missing", "", ErrNoPTZProfile},
	}
	for _, c := range cases {
		got, err := pickProfile(profiles, c.want)
		if got != c.token || !errors.Is(err, c.err) {
			t.Errorf("pickProfile(%q) = %q, %v; want %q, %v", c.want, got, err, c.token, c.err)
		}
	}
}

// fakeCamera is a minimal ONVIF device that checks WS-Security digests and
// records the last request body per action.
type fakeCamera struct {
	t        *testing.T
	user     string
	pass     string
	skew     atomic.Int64 // device clock minus real time
	requests map[string]string
	calls    map[string]int
}

type reqEnvelope struct {
	Header struct {
		Username string `xml:"Security>UsernameToken>Username"`
		Password string `xml:"Security>UsernameToken>Password"`
		Nonce    string `xml:"Security>UsernameToken>Nonce"`
		Created  string `xml:"Security>UsernameToken>Created"`
	} `xml:"Header"`
	Body struct {
		Action struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"Body"`
}

const respEnv = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope" xmlns:tt="http://www.onvif.org/ver10/schema"` +
	` xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:trt="http://www.onvif.org/ver10/media/wsdl"` +
	` xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl"><env:Body>%s</env:Body></env:Envelope>`

func (f *fakeCamera) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var env reqEnvelope
	if err := xml.Unmarshal(raw, &env); err != nil {
		f.t.Errorf("bad request xml: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	action := env.Body.Action.XMLName.Local
	f.requests[action] = string(raw)
	f.calls[action]++

	reply := func(status int, body string) {
		w.Header().Set("Content-Type", "application/soap+xml")
		w.WriteHeader(status)
		io.WriteString(w, strings.Replace(respEnv, "%s", body, 1))
	}

	if action == "GetSystemDateAndTime" {
		c := f.now().UTC()
		reply(200, `<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime><tt:UTCDateTime>`+
			`<tt:Time><tt:Hour>`+itoa(c.Hour())+`</tt:Hour><tt:Minute>`+itoa(c.Minute())+`</tt:Minute><tt:Second>`+itoa(c.Second())+`</tt:Second></tt:Time>`+
			`<tt:Date><tt:Year>`+itoa(c.Year())+`</tt:Year><tt:Month>`+itoa(int(c.Month()))+`</tt:Month><tt:Day>`+itoa(c.Day())+`</tt:Day></tt:Date>`+
			`</tt:UTCDateTime></tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`)
		return
	}

	if !f.validAuth(env) {
		reply(400, `<env:Fault><env:Code><env:Value>env:Sender</env:Value><env:Subcode><env:Value>ter:NotAuthorized</env:Value></env:Subcode></env:Code>`+
			`<env:Reason><env:Text xml:lang="en">Sender not Authorized</env:Text></env:Reason></env:Fault>`)
		return
	}

	switch action {
	case "GetCapabilities":
		// Deliberately advertise an unreachable host to exercise rebase.
		reply(200, `<tds:GetCapabilitiesResponse><tds:Capabilities>`+
			`<tt:Media><tt:XAddr>http://192.0.2.1/onvif/Media</tt:XAddr></tt:Media>`+
			`<tt:PTZ><tt:XAddr>http://192.0.2.1/onvif/PTZ</tt:XAddr></tt:PTZ>`+
			`</tds:Capabilities></tds:GetCapabilitiesResponse>`)
	case "GetProfiles":
		if r.URL.Path != "/onvif/Media" {
			f.t.Errorf("GetProfiles sent to %s", r.URL.Path)
		}
		reply(200, `<trt:GetProfilesResponse>`+
			`<trt:Profiles token="Profile_1"><tt:Name>main</tt:Name></trt:Profiles>`+
			`<trt:Profiles token="Profile_2"><tt:Name>ptz</tt:Name><tt:PTZConfiguration token="PTZ_1"/></trt:Profiles>`+
			`</trt:GetProfilesResponse>`)
	case "GetPresets":
		reply(200, `<tptz:GetPresetsResponse>`+
			`<tptz:Preset token="1"><tt:Name>door</tt:Name></tptz:Preset>`+
			`<tptz:Preset token="2"><tt:Name>yard</tt:Name></tptz:Preset>`+
			`</tptz:GetPresetsResponse>`)
	case "SetPreset":
		reply(200, `<tptz:SetPresetResponse><tptz:PresetToken>3</tptz:PresetToken></tptz:SetPresetResponse>`)
	case "ContinuousMove", "Stop", "GotoPreset", "RemovePreset":
		if r.URL.Path != "/onvif/PTZ" {
			f.t.Errorf("%s sent to %s", action, r.URL.Path)
		}
		reply(200, `<tptz:`+action+`Response/>`)
	default:
		reply(400, `<env:Fault><env:Code><env:Value>env:Receiver</env:Value></env:Code><env:Reason><env:Text>unknown `+action+`</env:Text></env:Reason></env:Fault>`)
	}
}

func (f *fakeCamera) validAuth(env reqEnvelope) bool {
	h := env.Header
	nonce, err := base64.StdEncoding.DecodeString(h.Nonce)
	if err != nil || h.Username != f.user {
		return false
	}
	created, err := time.Parse(time.RFC3339, h.Created)
	if err != nil || created.Sub(f.now()).Abs() > 5*time.Second {
		return false
	}
	sum := sha1.Sum([]byte(string(nonce) + h.Created + f.pass))
	return h.Password == base64.StdEncoding.EncodeToString(sum[:])
}

func itoa(i int) string { return strconv.Itoa(i) }

func (f *fakeCamera) now() time.Time { return time.Now().Add(time.Duration(f.skew.Load())) }

func newFake(t *testing.T) (*fakeCamera, *httptest.Server) {
	f := &fakeCamera{
		t: t, user: "admin", pass: "s3cr<t",
		requests: map[string]string{},
		calls:    map[string]int{},
	}
	// Camera clock is an hour ahead: the token must follow it.
	f.skew.Store(int64(time.Hour))
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestCamera(t *testing.T) {
	f, srv := newFake(t)
	ctx := context.Background()

	cam, err := Dial(ctx, Config{Endpoint: srv.URL, Username: f.user, Password: f.pass})
	if err != nil {
		t.Fatal(err)
	}
	if cam.ProfileToken() != "Profile_2" {
		t.Errorf("profile = %q", cam.ProfileToken())
	}

	if err := cam.Move(ctx, Velocity{Pan: 2, Tilt: -0.25}, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	mv := f.requests["ContinuousMove"]
	for _, want := range []string{`<tt:PanTilt x="1" y="-0.25"/>`, `<tptz:Timeout>PT1.5S</tptz:Timeout>`, `<tptz:ProfileToken>Profile_2</tptz:ProfileToken>`} {
		if !strings.Contains(mv, want) {
			t.Errorf("ContinuousMove missing %s:\n%s", want, mv)
		}
	}
	if strings.Contains(mv, "Zoom") {
		t.Errorf("ContinuousMove sent a Zoom axis:\n%s", mv)
	}

	if err := cam.Move(ctx, Velocity{Pan: 0.5}, 0); err != nil {
		t.Fatal(err)
	}
	if mv := f.requests["ContinuousMove"]; strings.Contains(mv, "Timeout") {
		t.Errorf("move without duration sent a Timeout:\n%s", mv)
	}

	for name, fn := range map[string]func() error{
		"Stop":         func() error { return cam.Stop(ctx) },
		"GotoPreset":   func() error { return cam.GotoPreset(ctx, "2") },
		"RemovePreset": func() error { return cam.RemovePreset(ctx, "2") },
	} {
		if err := fn(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	presets, err := cam.Presets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 2 || presets[1] != (Preset{Token: "2", Name: "yard"}) {
		t.Errorf("presets = %+v", presets)
	}

	tok, err := cam.SavePreset(ctx, "a&b", "")
	if err != nil || tok != "3" {
		t.Errorf("SavePreset = %q, %v", tok, err)
	}
	if sp := f.requests["SetPreset"]; !strings.Contains(sp, "<tptz:PresetName>a&amp;b</tptz:PresetName>") || strings.Contains(sp, "PresetToken") {
		t.Errorf("SetPreset body:\n%s", sp)
	}
}

func TestDialBadCredentials(t *testing.T) {
	_, srv := newFake(t)
	_, err := Dial(context.Background(), Config{Endpoint: srv.URL, Username: "admin", Password: "wrong"})
	var fault *FaultError
	if !errors.As(err, &fault) || fault.Subcode != "ter:NotAuthorized" {
		t.Fatalf("err = %v, want NotAuthorized fault", err)
	}
}

func TestClockJumpRetries(t *testing.T) {
	f, srv := newFake(t)
	ctx := context.Background()
	cam, err := Dial(ctx, Config{Endpoint: srv.URL, Username: f.user, Password: f.pass})
	if err != nil {
		t.Fatal(err)
	}

	// Device resyncs NTP after Dial: its clock jumps back to real time.
	f.skew.Store(0)
	before := f.calls["GetSystemDateAndTime"]
	if err := cam.Stop(ctx); err != nil {
		t.Fatalf("Stop after clock jump: %v", err)
	}
	if got := f.calls["GetSystemDateAndTime"] - before; got != 1 {
		t.Errorf("re-measured clock %d times, want 1", got)
	}
	if f.calls["Stop"] != 2 {
		t.Errorf("Stop sent %d times, want 2 (rejected + retry)", f.calls["Stop"])
	}

	// Next call uses the new offset directly.
	if err := cam.GotoPreset(ctx, "1"); err != nil || f.calls["GotoPreset"] != 1 {
		t.Errorf("GotoPreset = %v after %d calls", err, f.calls["GotoPreset"])
	}
}

func TestWrongPasswordNoRetry(t *testing.T) {
	f, srv := newFake(t)
	ctx := context.Background()
	cam, err := Dial(ctx, Config{Endpoint: srv.URL, Username: f.user, Password: f.pass})
	if err != nil {
		t.Fatal(err)
	}
	f.pass = "rotated"
	err = cam.Stop(ctx)
	var fault *FaultError
	if !errors.As(err, &fault) {
		t.Fatalf("err = %v, want fault", err)
	}
	if f.calls["Stop"] != 1 {
		t.Errorf("Stop sent %d times, want 1: unchanged clock must not retry", f.calls["Stop"])
	}
}

func TestIsAuthFailure(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{ErrUnauthorized, true},
		{&FaultError{Subcode: "ter:NotAuthorized"}, true},
		{&FaultError{Code: "wsse:FailedAuthentication"}, true},
		{&FaultError{Subcode: "InvalidSecurity"}, true},
		{&FaultError{Status: 401}, true},
		{&FaultError{Subcode: "ter:InvalidArgVal"}, false},
		{errors.New("dial tcp: timeout"), false},
	} {
		if got := isAuthFailure(c.err); got != c.want {
			t.Errorf("isAuthFailure(%v) = %v", c.err, got)
		}
	}
}
