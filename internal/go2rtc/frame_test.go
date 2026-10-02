package go2rtc

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFrameSize(t *testing.T) {
	for _, tc := range []struct {
		size    int
		wantErr bool
	}{
		{maxFrame, false},
		{maxFrame + 1, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(bytes.Repeat([]byte{0xff}, tc.size))
		}))
		b, err := Frame(context.Background(), srv.Client(), srv.URL, "cam")
		srv.Close()
		if gotErr := err != nil; gotErr != tc.wantErr {
			t.Errorf("size %d: err = %v, want error %v", tc.size, err, tc.wantErr)
		}
		if !tc.wantErr && len(b) != tc.size {
			t.Errorf("size %d: got %d bytes", tc.size, len(b))
		}
	}
}
