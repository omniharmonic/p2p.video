package main

import (
	"net"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/config"
)

// TestRelayPeerFilterFailsClosed: with the SFU mux on ephemeral ports the
// relay is not started, rather than allowed to reach any loopback port (L2).
func TestRelayPeerFilterFailsClosed(t *testing.T) {
	if f, err := relayPeerFilter(0); err == nil || f != nil {
		t.Fatal("ephemeral SFU ports must disable TURN")
	}
	f, err := relayPeerFilter(50000)
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{"127.0.0.1:50000": true, "127.0.0.1:50001": false, "127.0.0.1:53": false, "10.0.0.1:50000": false} {
		u, _ := net.ResolveUDPAddr("udp", addr)
		if f(u) != want {
			t.Errorf("%s allowed=%v, want %v", addr, f(u), want)
		}
	}
}

func TestApplyNetworkFlags(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PublicURL = "https://studio.tail1234.ts.net"
	if err := applyNetworkFlags(cfg, true, "turn:127.0.0.1:3478?transport=tcp", true); err != nil {
		t.Fatal(err)
	}
	if cfg.PublicSTUN {
		t.Fatal("--no-public-stun ignored")
	}
	want := []string{"turn:127.0.0.1:3478?transport=tcp", "turns:studio.tail1234.ts.net:8443?transport=tcp"}
	if len(cfg.TURNURLs) != 2 || cfg.TURNURLs[0] != want[0] || cfg.TURNURLs[1] != want[1] {
		t.Fatalf("TURN urls = %v", cfg.TURNURLs)
	}
	if o := sfuNetworkOptions(cfg); len(o.STUNURLs) != 0 || !o.IncludeLoopback || o.UDPPort != 50000 {
		t.Fatalf("sfu options = %+v", o)
	}

	for name, mutate := range map[string]func(*config.Config) (bool, string, bool){
		"funnel with no-funnel": func(c *config.Config) (bool, string, bool) { c.NoFunnel = true; return false, "", true },
		"funnel without https host": func(c *config.Config) (bool, string, bool) {
			c.PublicURL = "http://localhost:7443"
			return false, "", true
		},
		"public TURN listener": func(c *config.Config) (bool, string, bool) { c.TURNListen = "0.0.0.0:3478"; return false, "", false },
		"advertised but disabled": func(c *config.Config) (bool, string, bool) {
			c.TURNListen = ""
			return false, "turn:127.0.0.1:3478", false
		},
		"bad url": func(c *config.Config) (bool, string, bool) { return false, "stun:x:1", false },
	} {
		t.Run(name, func(t *testing.T) {
			c := config.DefaultConfig()
			c.PublicURL = "https://studio.tail1234.ts.net"
			stun, urls, funnel := mutate(c)
			if err := applyNetworkFlags(c, stun, urls, funnel); err == nil {
				t.Fatal("accepted")
			}
		})
	}

	def := config.DefaultConfig()
	if err := applyNetworkFlags(def, false, "", false); err != nil {
		t.Fatal(err)
	}
	if o := sfuNetworkOptions(def); len(o.STUNURLs) == 0 || len(def.TURNURLs) != 0 {
		t.Fatalf("defaults: STUN allowed, TURN running but unadvertised; got %+v %v", o, def.TURNURLs)
	}
}
