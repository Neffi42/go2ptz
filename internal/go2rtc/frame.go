// Package go2rtc talks to the go2rtc HTTP API.
package go2rtc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxFrame = 32 << 20

func Frame(ctx context.Context, hc *http.Client, base, src string) ([]byte, error) {
	u, err := url.JoinPath(base, "api/frame.jpeg")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?src="+url.QueryEscape(src), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if loc := resp.Header.Get("Location"); loc != "" {
			return nil, fmt.Errorf("go2rtc: frame %q: redirected to %s (is GO2RTC_URL behind auth?)", src, loc)
		}
		return nil, fmt.Errorf("go2rtc: frame %q: http %d", src, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		return nil, fmt.Errorf("go2rtc: frame %q: unexpected content type %q", src, ct)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFrame+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFrame {
		return nil, fmt.Errorf("go2rtc: frame %q: larger than %d bytes", src, maxFrame)
	}
	return b, nil
}
