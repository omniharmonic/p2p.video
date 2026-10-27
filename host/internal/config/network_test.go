package config

import "testing"

func TestFunnelTURNURL(t *testing.T) {
	got, err := FunnelTURNURL("https://studio.tail1234.ts.net")
	if err != nil || got != "turns:studio.tail1234.ts.net:8443?transport=tcp" {
		t.Fatalf("got %q %v", got, err)
	}
	for _, bad := range []string{"http://localhost:7443", "", "https://"} {
		if _, err := FunnelTURNURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseTURNURLs(t *testing.T) {
	got, err := ParseTURNURLs(" turn:127.0.0.1:3478?transport=tcp, turns:h.ts.net:8443?transport=tcp ,")
	if err != nil || len(got) != 2 || got[0] != "turn:127.0.0.1:3478?transport=tcp" {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{"stun:x:1", "http://x", "turn:", "turn:h", "turn:a@b:1", "turn:h:1?x=y"} {
		if _, err := ParseTURNURLs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got, err := ParseTURNURLs(""); err != nil || len(got) != 0 {
		t.Fatal("empty list should be allowed")
	}
}

func TestDefaultNetworkPolicy(t *testing.T) {
	c := DefaultConfig()
	if !c.PublicSTUN || len(c.STUNURLs) == 0 || c.UDPPort != 50000 || c.TURNListen != "127.0.0.1:3478" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if len(c.TURNURLs) != 0 || c.TURNSecret != nil {
		t.Fatal("TURN must not be advertised by default")
	}
}
