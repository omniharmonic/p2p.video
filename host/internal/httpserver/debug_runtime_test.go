package httpserver

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
)

// Descriptors opening and closing on other goroutines (a busy daemon) must
// not make the count fail: it used to report -1 on macOS [SL-P5-4].
func TestCountOpenFDsUnderChurn(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no /dev/fd")
	}
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			if f, err := os.Open(os.DevNull); err == nil {
				f.Close()
			}
		}
	}()
	defer func() { stop.Store(true); <-done }()
	for i := 0; i < 2000; i++ {
		if n := countOpenFDs(); n <= 0 {
			t.Fatalf("iteration %d: countOpenFDs = %d", i, n)
		}
	}
}

// The runtime snapshot is loopback-only diagnostics: never through Tailscale
// Serve or Funnel, never to a cross-origin page [SL-P0-6].
func TestDebugRuntimeIsLoopbackOnly(t *testing.T) {
	srv := &Server{RuntimeCounts: func() map[string]int { return map[string]int{"sfuPeers": 2} }}
	h := srv.Handler()

	req := httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/debug/runtime", nil)
	req.RemoteAddr = "127.0.0.1:50123"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("loopback: %d %s", rec.Code, rec.Body)
	}
	var stats RuntimeStats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Goroutines <= 0 || stats.HeapAllocBytes == 0 || stats.Counts["sfuPeers"] != 2 || stats.OpenFDs == 0 {
		t.Fatalf("unexpected stats %+v", stats)
	}

	cases := []struct {
		name   string
		host   string
		remote string
		header [2]string
	}{
		{"funnel", "studio.tail1234.ts.net", "127.0.0.1:1", [2]string{"Tailscale-Funnel-Request", "?1"}},
		{"serve forwarded", "studio.tail1234.ts.net", "127.0.0.1:1", [2]string{"X-Forwarded-For", "100.100.1.1"}},
		{"serve identity on loopback host", "127.0.0.1:7443", "127.0.0.1:1", [2]string{"Tailscale-User-Login", "owner@example.com"}},
		{"funnel header on loopback host", "127.0.0.1:7443", "127.0.0.1:1", [2]string{"Tailscale-Funnel-Request", "?1"}},
		{"lan client", "192.168.1.5:7443", "192.168.1.9:4000", [2]string{}},
		{"any browser page", "127.0.0.1:7443", "127.0.0.1:1", [2]string{"Origin", "http://127.0.0.1:7443"}},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", "http://"+tc.host+"/api/v1/debug/runtime", nil)
		req.RemoteAddr = tc.remote
		if tc.header[0] != "" {
			req.Header.Set(tc.header[0], tc.header[1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%s: got %d, want 403", tc.name, rec.Code)
		}
	}
	post := httptest.NewRequest("POST", "http://127.0.0.1:7443/api/v1/debug/runtime", nil)
	post.RemoteAddr = "127.0.0.1:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, post)
	if rec.Code != 405 {
		t.Fatalf("POST: %d", rec.Code)
	}
}
