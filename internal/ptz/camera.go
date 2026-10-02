// Package ptz is a minimal ONVIF client covering PTZ movement and presets,
// speaking SOAP directly over net/http. It is bound to a single PTZ-capable
// media profile.
package ptz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrNoPTZProfile is returned by Dial when the device exposes no media
// profile with a PTZ configuration (or the requested one is missing).
var ErrNoPTZProfile = errors.New("no PTZ-capable media profile")

type Config struct {
	Endpoint string
	Username string
	Password string
	// Profile selects a media profile by token or name. Empty picks the
	// first profile that has a PTZ configuration.
	Profile string
	Timeout time.Duration
}

// Velocity components are normalized to [-1, 1]; out-of-range values are clamped.
type Velocity struct {
	Pan, Tilt float64
}

type Preset struct {
	Token string
	Name  string
}

type Camera struct {
	http       *http.Client
	user, pass string
	device     string
	// clockOffset is device minus local time (time.Duration), re-measured on auth failures.
	clockOffset atomic.Int64
	ptzURL      string
	profile     string
}

func (c *Camera) offset() time.Duration { return time.Duration(c.clockOffset.Load()) }

// Dial measures clock skew, discovers the Media and PTZ service URLs and
// resolves the PTZ profile.
func Dial(ctx context.Context, cfg Config) (*Camera, error) {
	device, err := deviceURL(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("ptz: endpoint %q: %w", cfg.Endpoint, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	c := &Camera{
		http:   &http.Client{Timeout: timeout},
		user:   cfg.Username,
		pass:   cfg.Password,
		device: device.String(),
	}

	// Best effort: devices without a usable clock still work if ours is close.
	if offset, err := c.clockSkew(ctx); err == nil {
		c.clockOffset.Store(int64(offset))
	}

	mediaURL, ptzURL, err := c.capabilities(ctx, device)
	if err != nil {
		return nil, fmt.Errorf("ptz: %s: get capabilities: %w", cfg.Endpoint, err)
	}
	if ptzURL == "" {
		return nil, fmt.Errorf("ptz: %s: device has no PTZ service", cfg.Endpoint)
	}
	c.ptzURL = ptzURL

	profiles, err := c.profiles(ctx, mediaURL)
	if err != nil {
		return nil, fmt.Errorf("ptz: %s: get profiles: %w", cfg.Endpoint, err)
	}
	if c.profile, err = pickProfile(profiles, cfg.Profile); err != nil {
		return nil, fmt.Errorf("ptz: %s: %w", cfg.Endpoint, err)
	}
	return c, nil
}

// ProfileToken returns the media profile token commands are sent to.
func (c *Camera) ProfileToken() string { return c.profile }

// Move starts a continuous move. A positive d asks the camera to stop on its
// own after d, which guards against a lost Stop; zero means until Stop.
func (c *Camera) Move(ctx context.Context, v Velocity, d time.Duration) error {
	vel := fmt.Sprintf(`<tt:PanTilt x="%s" y="%s"/>`, fmtFloat(clamp(v.Pan)), fmtFloat(clamp(v.Tilt)))
	var timeout string
	if d > 0 {
		timeout = el("tptz:Timeout", xsDuration(d))
	}

	return c.call(ctx, c.ptzURL,
		`<tptz:ContinuousMove xmlns:tptz="`+nsPTZ+`" xmlns:tt="`+nsSchema+`">`+
			el("tptz:ProfileToken", c.profile)+
			`<tptz:Velocity>`+vel+`</tptz:Velocity>`+timeout+
			`</tptz:ContinuousMove>`, nil)
}

func (c *Camera) Stop(ctx context.Context) error {
	return c.call(ctx, c.ptzURL,
		`<tptz:Stop xmlns:tptz="`+nsPTZ+`">`+
			el("tptz:ProfileToken", c.profile)+
			`<tptz:PanTilt>true</tptz:PanTilt><tptz:Zoom>true</tptz:Zoom>`+
			`</tptz:Stop>`, nil)
}

func (c *Camera) Presets(ctx context.Context) ([]Preset, error) {
	var resp struct {
		Presets []struct {
			Token string `xml:"token,attr"`
			Name  string `xml:"Name"`
		} `xml:"Preset"`
	}
	err := c.call(ctx, c.ptzURL,
		`<tptz:GetPresets xmlns:tptz="`+nsPTZ+`">`+
			el("tptz:ProfileToken", c.profile)+
			`</tptz:GetPresets>`, &resp)
	if err != nil {
		return nil, err
	}
	out := make([]Preset, len(resp.Presets))
	for i, p := range resp.Presets {
		out[i] = Preset{Token: p.Token, Name: p.Name}
	}
	return out, nil
}

func (c *Camera) GotoPreset(ctx context.Context, token string) error {
	return c.call(ctx, c.ptzURL,
		`<tptz:GotoPreset xmlns:tptz="`+nsPTZ+`">`+
			el("tptz:ProfileToken", c.profile)+
			el("tptz:PresetToken", token)+
			`</tptz:GotoPreset>`, nil)
}

// SavePreset stores the current position. An empty token creates a new
// preset; a non-empty one overwrites it. Returns the preset token.
func (c *Camera) SavePreset(ctx context.Context, name, token string) (string, error) {
	body := `<tptz:SetPreset xmlns:tptz="` + nsPTZ + `">` + el("tptz:ProfileToken", c.profile)
	if name != "" {
		body += el("tptz:PresetName", name)
	}
	if token != "" {
		body += el("tptz:PresetToken", token)
	}
	body += `</tptz:SetPreset>`

	var resp struct {
		Token string `xml:"PresetToken"`
	}
	if err := c.call(ctx, c.ptzURL, body, &resp); err != nil {
		return "", err
	}
	return resp.Token, nil
}

func (c *Camera) RemovePreset(ctx context.Context, token string) error {
	return c.call(ctx, c.ptzURL,
		`<tptz:RemovePreset xmlns:tptz="`+nsPTZ+`">`+
			el("tptz:ProfileToken", c.profile)+
			el("tptz:PresetToken", token)+
			`</tptz:RemovePreset>`, nil)
}

// clockSkew returns device time minus local time. The request is sent
// unauthenticated, as the spec requires devices to allow.
func (c *Camera) clockSkew(ctx context.Context) (time.Duration, error) {
	var resp struct {
		UTC struct {
			Hour   int `xml:"Time>Hour"`
			Minute int `xml:"Time>Minute"`
			Second int `xml:"Time>Second"`
			Year   int `xml:"Date>Year"`
			Month  int `xml:"Date>Month"`
			Day    int `xml:"Date>Day"`
		} `xml:"SystemDateAndTime>UTCDateTime"`
	}
	sent := time.Now()
	if err := c.do(ctx, c.device, "", `<tds:GetSystemDateAndTime xmlns:tds="`+nsDevice+`"/>`, &resp); err != nil {
		return 0, err
	}
	u := resp.UTC
	if u.Year == 0 {
		return 0, errors.New("no UTCDateTime in response")
	}
	dev := time.Date(u.Year, time.Month(u.Month), u.Day, u.Hour, u.Minute, u.Second, 0, time.UTC)
	return dev.Sub(sent), nil
}

// capabilities returns the Media and PTZ service URLs, rebased on the dialed
// host: cameras behind NAT advertise unreachable addresses.
func (c *Camera) capabilities(ctx context.Context, device *url.URL) (media, ptz string, err error) {
	var resp struct {
		Media string `xml:"Capabilities>Media>XAddr"`
		PTZ   string `xml:"Capabilities>PTZ>XAddr"`
	}
	err = c.call(ctx, device.String(),
		`<tds:GetCapabilities xmlns:tds="`+nsDevice+`"><tds:Category>All</tds:Category></tds:GetCapabilities>`, &resp)
	if err != nil {
		return "", "", err
	}
	media = rebase(device, resp.Media)
	if media == "" {
		// Many cameras serve Media on the device endpoint.
		media = device.String()
	}
	return media, rebase(device, resp.PTZ), nil
}

type profile struct {
	Token string `xml:"token,attr"`
	Name  string `xml:"Name"`
	PTZ   *struct {
		Token string `xml:"token,attr"`
	} `xml:"PTZConfiguration"`
}

func (c *Camera) profiles(ctx context.Context, mediaURL string) ([]profile, error) {
	var resp struct {
		Profiles []profile `xml:"Profiles"`
	}
	err := c.call(ctx, mediaURL, `<trt:GetProfiles xmlns:trt="`+nsMedia+`"/>`, &resp)
	return resp.Profiles, err
}

func pickProfile(profiles []profile, want string) (string, error) {
	for _, p := range profiles {
		if p.PTZ == nil {
			continue
		}
		if want == "" || p.Token == want || p.Name == want {
			return p.Token, nil
		}
	}
	if want != "" {
		return "", fmt.Errorf("%w: %q", ErrNoPTZProfile, want)
	}
	return "", ErrNoPTZProfile
}

// CheckEndpoint reports whether Dial accepts endpoint.
func CheckEndpoint(endpoint string) error {
	_, err := deviceURL(endpoint)
	return err
}

// deviceURL normalizes "host", "host:port" or an http URL to the device
// service URL. https is rejected: the image ships no CA bundle.
func deviceURL(endpoint string) (*url.URL, error) {
	if endpoint == "" {
		return nil, errors.New("empty")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("unsupported scheme %q, only http", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/onvif/device_service"
	}
	return u, nil
}

// rebase keeps the path and query of xaddr but uses device's scheme and host.
func rebase(device *url.URL, xaddr string) string {
	if xaddr == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(xaddr))
	if err != nil {
		return ""
	}
	u.Scheme, u.Host = device.Scheme, device.Host
	return u.String()
}

func clamp(f float64) float64 { return max(-1, min(1, f)) }

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// xsDuration formats d as an xs:duration in seconds, e.g. "PT1.5S".
func xsDuration(d time.Duration) string {
	return "PT" + fmtFloat(d.Seconds()) + "S"
}
