package ptz

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	nsDevice = "http://www.onvif.org/ver10/device/wsdl"
	nsMedia  = "http://www.onvif.org/ver10/media/wsdl"
	nsPTZ    = "http://www.onvif.org/ver20/ptz/wsdl"
	nsSchema = "http://www.onvif.org/ver10/schema"

	nsWSSE = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
	nsWSU  = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"

	passwordDigest = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest"
	base64Binary   = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-soap-message-security-1.0#Base64Binary"
)

// ErrUnauthorized is returned for a plain HTTP 401 without a SOAP fault.
var ErrUnauthorized = errors.New("onvif: http 401 unauthorized")

// FaultError is a SOAP fault returned by the device.
type FaultError struct {
	Status  int
	Code    string
	Subcode string
	Reason  string
}

func (e *FaultError) Error() string {
	return fmt.Sprintf("onvif fault (http %d): %s/%s: %s", e.Status, e.Code, e.Subcode, e.Reason)
}

type soapFault struct {
	Code struct {
		Value   string `xml:"Value"`
		Subcode struct {
			Value string `xml:"Value"`
		} `xml:"Subcode"`
	} `xml:"Code"`
	Reason struct {
		Text string `xml:"Text"`
	} `xml:"Reason"`
}

type soapEnvelope struct {
	Body struct {
		Fault   *soapFault `xml:"Fault"`
		Content []byte     `xml:",innerxml"`
	} `xml:"Body"`
}

// skewTolerance is how far the device clock must have moved since the last
// measurement before an auth failure is retried.
const skewTolerance = time.Second

// call sends an authenticated request. On an auth failure it re-measures the
// device clock and retries once if it moved; a wrong password is not retried.
func (c *Camera) call(ctx context.Context, url, body string, out any) error {
	err := c.do(ctx, url, c.security(), body, out)
	if err == nil || c.user == "" || !isAuthFailure(err) {
		return err
	}
	prev := c.offset()
	now, skewErr := c.clockSkew(ctx)
	if skewErr != nil || (now-prev).Abs() < skewTolerance {
		return err
	}
	c.clockOffset.Store(int64(now))
	slog.Info("onvif: device clock moved, retrying", "from", prev, "to", now)
	return c.do(ctx, url, c.security(), body, out)
}

// isAuthFailure matches the faults devices use for a rejected UsernameToken.
// Spec says ter:NotAuthorized; many devices send WS-Security codes instead.
func isAuthFailure(err error) bool {
	if errors.Is(err, ErrUnauthorized) {
		return true
	}
	var f *FaultError
	if !errors.As(err, &f) {
		return false
	}
	if f.Status == http.StatusUnauthorized {
		return true
	}
	for _, code := range []string{f.Subcode, f.Code} {
		_, local, _ := strings.Cut(code, ":")
		if local == "" {
			local = code
		}
		switch local {
		case "NotAuthorized", "FailedAuthentication", "InvalidSecurity", "InvalidSecurityToken", "MessageExpired":
			return true
		}
	}
	return false
}

// do posts a SOAP 1.2 envelope and decodes the first Body child into out
// (which may be nil).
func (c *Camera) do(ctx context.Context, url, header, body string, out any) error {
	env := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">` +
		`<s:Header>` + header + `</s:Header><s:Body>` + body + `</s:Body></s:Envelope>`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(env))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	var e soapEnvelope
	if err := xml.Unmarshal(raw, &e); err != nil {
		if resp.StatusCode == http.StatusUnauthorized {
			return ErrUnauthorized
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("onvif: http %d: %s", resp.StatusCode, bytes.TrimSpace(raw[:min(len(raw), 200)]))
		}
		return fmt.Errorf("onvif: decode envelope: %w", err)
	}
	if f := e.Body.Fault; f != nil {
		return &FaultError{
			Status:  resp.StatusCode,
			Code:    f.Code.Value,
			Subcode: f.Code.Subcode.Value,
			Reason:  f.Reason.Text,
		}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("onvif: http %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(e.Body.Content, out); err != nil {
		return fmt.Errorf("onvif: decode body: %w", err)
	}
	return nil
}

// security builds a WS-Security UsernameToken PasswordDigest, with Created in
// device time since devices reject skewed tokens.
func (c *Camera) security() string {
	if c.user == "" {
		return ""
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	created := time.Now().Add(c.offset()).UTC().Format("2006-01-02T15:04:05.000Z")

	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(c.pass))
	digest := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return `<wsse:Security s:mustUnderstand="1" xmlns:wsse="` + nsWSSE + `" xmlns:wsu="` + nsWSU + `">` +
		`<wsse:UsernameToken>` +
		`<wsse:Username>` + html.EscapeString(c.user) + `</wsse:Username>` +
		`<wsse:Password Type="` + passwordDigest + `">` + digest + `</wsse:Password>` +
		`<wsse:Nonce EncodingType="` + base64Binary + `">` + base64.StdEncoding.EncodeToString(nonce) + `</wsse:Nonce>` +
		`<wsu:Created>` + created + `</wsu:Created>` +
		`</wsse:UsernameToken></wsse:Security>`
}

func el(name, text string) string {
	return "<" + name + ">" + html.EscapeString(text) + "</" + name + ">"
}
