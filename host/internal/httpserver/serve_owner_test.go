package httpserver

import (
	"github.com/omniharmonic/studiolink/host/internal/config"
	"net/http/httptest"
	"testing"
)

func TestPrivateServeOwnerManagement(t *testing.T) {
	cfg := &config.Config{PublicURL: "https://host.tail123.ts.net", OwnerTailscaleLogin: "owner@example.test"}
	s := &Server{Config: cfg}
	cases := []struct {
		name, remote, host, client, login, funnel string
		want                                      bool
	}{
		{"owner through Serve", "127.0.0.1:5000", "host.tail123.ts.net", "100.64.0.10", "owner@example.test", "", true},
		{"shared guest", "127.0.0.1:5000", "host.tail123.ts.net", "100.64.0.10", "guest@example.test", "", false},
		{"public Funnel", "127.0.0.1:5000", "host.tail123.ts.net", "100.64.0.10", "owner@example.test", "?1", false},
		{"public forwarded address", "127.0.0.1:5000", "host.tail123.ts.net", "203.0.113.2", "owner@example.test", "", false},
		{"direct untrusted peer", "100.64.0.10:5000", "host.tail123.ts.net", "100.64.0.10", "owner@example.test", "", false},
		{"wrong proxy hostname", "127.0.0.1:5000", "other.tail123.ts.net", "100.64.0.10", "owner@example.test", "", false},
		{"spoofed header chain", "127.0.0.1:5000", "host.tail123.ts.net", "100.64.0.10, 203.0.113.2", "owner@example.test", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://"+tc.host+"/api/v1/sessions", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-For", tc.client)
			r.Header.Set("Tailscale-User-Login", tc.login)
			r.Header.Set("Tailscale-Funnel-Request", tc.funnel)
			if s.managementAllowed(r) != tc.want {
				t.Fatal("management boundary mismatch")
			}
		})
	}
	cfg.OwnerTailscaleLogin = ""
	r := httptest.NewRequest("POST", "http://host.tail123.ts.net/api/v1/sessions", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "100.64.0.10")
	r.Header.Set("Tailscale-User-Login", "owner@example.test")
	if s.managementAllowed(r) {
		t.Fatal("default must remain local-only")
	}
}
