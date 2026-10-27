package turn

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestL1RevokeCutsLiveAllocation: after Revoke, a removed participant's
// existing allocation stops relaying at once (it does not wait for the next
// refresh), and a new allocation is refused once Authorize says no [SL-SEC-1].
func TestL1RevokeCutsLiveAllocation(t *testing.T) {
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var denied atomic.Bool
	s := startTestServer(t, func(pid string) bool { return !(denied.Load() && pid == "p_guest") }, nil)

	c := Mint(testSecret, "p_guest", time.Hour, time.Now())
	relay, _, err := allocate(t, s, c.Username, c.Credential)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	defer relay.Close()
	other := Mint(testSecret, "p_other", time.Hour, time.Now())
	otherRelay, _, err := allocate(t, s, other.Username, other.Credential)
	if err != nil {
		t.Fatalf("allocate other: %v", err)
	}
	defer otherRelay.Close()
	if s.RelayCount("p_guest") != 1 || s.RelayCount("p_other") != 1 {
		t.Fatalf("relay tracking: guest=%d other=%d", s.RelayCount("p_guest"), s.RelayCount("p_other"))
	}

	buf := make([]byte, 64)
	recv := func() (string, bool) {
		_ = peer.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := peer.ReadFrom(buf)
		return string(buf[:n]), err == nil
	}
	if _, err := relay.WriteTo([]byte("before"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if got, ok := recv(); !ok || got != "before" {
		t.Fatalf("relay not working before revoke: %q %v", got, ok)
	}

	denied.Store(true)
	if n := s.Revoke("p_guest"); n != 1 {
		t.Fatalf("revoked %d relays, want 1", n)
	}
	if s.RelayCount("p_guest") != 0 {
		t.Fatal("revoked relay still tracked")
	}
	_, _ = relay.WriteTo([]byte("after"), peer.LocalAddr())
	if got, ok := recv(); ok {
		t.Fatalf("revoked allocation still relayed %q", got)
	}
	// Other participants are untouched.
	if _, err := otherRelay.WriteTo([]byte("other"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if got, ok := recv(); !ok || got != "other" {
		t.Fatalf("other participant's relay broke: %q %v", got, ok)
	}
	// And the same credentials cannot allocate again.
	if r2, _, err := allocate(t, s, c.Username, c.Credential); err == nil {
		_ = r2.Close()
		t.Fatal("revoked participant allocated again")
	}
}
