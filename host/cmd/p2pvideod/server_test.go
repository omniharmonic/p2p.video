package main

import (
	"net/http"
	"testing"
	"time"
)

// TestH1HTTPServerBoundsSlowClients: header and idle timeouts are set, and no
// whole-request timeout would cut a slow-but-legitimate chunk upload or a
// WebSocket [SL-SEC-1].
func TestH1HTTPServerBoundsSlowClients(t *testing.T) {
	s := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout != 10*time.Second || s.IdleTimeout <= 0 || s.MaxHeaderBytes <= 0 {
		t.Fatalf("server limits: %+v", s)
	}
	if s.ReadTimeout != 0 || s.WriteTimeout != 0 {
		t.Fatal("a whole-request timeout would break 256 kbit/s uploads, WebSockets and downloads")
	}
}
