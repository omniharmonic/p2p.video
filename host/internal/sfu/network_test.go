package sfu

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/omniharmonic/studiolink/host/internal/turn"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	_ = c.Close()
	return port
}

func TestUDPMuxSinglePortAndBusyFallback(t *testing.T) {
	port := freeUDPPort(t)
	a, err := NewSFU(nil, WithNetwork(NetworkOptions{UDPPort: port, IncludeLoopback: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.UDPPort() != port {
		t.Fatalf("mux port=%d want %d", a.UDPPort(), port)
	}
	// The port is now busy: a second SFU must still start, on ephemeral ports.
	b, err := NewSFU(nil, WithNetwork(NetworkOptions{UDPPort: port, IncludeLoopback: true}))
	if err != nil {
		t.Fatalf("busy port must not be fatal: %v", err)
	}
	defer b.Close()
	if b.UDPPort() != 0 {
		t.Fatalf("fallback should report ephemeral (0), got %d", b.UDPPort())
	}
	pc, err := b.api.NewPeerConnection(b.config)
	if err != nil {
		t.Fatal(err)
	}
	_ = pc.Close()
}

// A restarted daemon must get the same mux port back once the previous
// process released it (UDP has no TIME_WAIT) [SL-P0-7].
func TestUDPMuxRebindsAfterClose(t *testing.T) {
	port := freeUDPPort(t)
	for i := 0; i < 3; i++ {
		s, err := NewSFU(nil, WithNetwork(NetworkOptions{UDPPort: port, IncludeLoopback: true}))
		if err != nil {
			t.Fatal(err)
		}
		if s.UDPPort() != port {
			t.Fatalf("restart %d: mux port=%d want %d", i, s.UDPPort(), port)
		}
		if c := s.Counts(); c["sfuPeers"] != 0 || c["sfuPublications"] != 0 || c["sfuSubscriptions"] != 0 || c["sfuPendingCandidateKeys"] != 0 {
			t.Fatalf("fresh SFU counts %v", c)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSFUSTUNOnlyFromPolicy(t *testing.T) {
	s, err := NewSFU(nil, WithNetwork(NetworkOptions{STUNURLs: []string{"stun:stun.example.test:3478"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.config.ICEServers) != 1 || s.config.ICEServers[0].URLs[0] != "stun:stun.example.test:3478" {
		t.Fatalf("SFU ICE servers = %+v", s.config.ICEServers)
	}
	plain, _ := NewSFU(nil)
	if len(plain.config.ICEServers) != 0 {
		t.Fatal("default SFU must not contact public STUN")
	}
}

// A relay-only client (as a browser with iceTransportPolicy "relay" behind a
// UDP-blocking network would be) reaches the SFU solely through the embedded
// TURN server over TCP on loopback, exactly the Funnel :8443 path minus TLS.
func TestRelayOnlyClientConnectsThroughEmbeddedTURN(t *testing.T) {
	port := freeUDPPort(t)
	s, err := NewSFU(nil, WithNetwork(NetworkOptions{UDPPort: port, IncludeLoopback: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.UDPPort() != port {
		t.Fatalf("mux not bound")
	}
	secret, _ := turn.NewSecret()
	relay, err := turn.Start(turn.Config{
		ListenAddr: "127.0.0.1:0", Secret: secret,
		AllowPeer: func(a *net.UDPAddr) bool { return a.IP.IsLoopback() && a.Port == s.UDPPort() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	creds := turn.Mint(secret, "p_guest", time.Hour, time.Now())
	settings := webrtc.SettingEngine{}
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	client, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{
			URLs:     []string{fmt.Sprintf("turn:%s?transport=tcp", relay.Addr())},
			Username: creds.Username, Credential: creds.Credential,
		}},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	sfuCandidates := make(chan webrtc.ICECandidateInit, 64)
	s.SetOnCandidate(func(_, _ string, c string, mid *string, idx *uint16) {
		sfuCandidates <- webrtc.ICECandidateInit{Candidate: c, SDPMid: mid, SDPMLineIndex: idx}
	})
	connected := make(chan struct{})
	client.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(client)
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	answer, err := s.HandleOffer("room", "p_guest", client.LocalDescription().SDP)
	if err != nil {
		t.Fatal(err)
	}
	defer s.OnParticipantLeave("room", "p_guest")
	if err := client.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case c := <-sfuCandidates:
			if err := client.AddICECandidate(c); err != nil {
				t.Fatal(err)
			}
			continue
		case <-connected:
		case <-deadline:
			t.Fatalf("relay-only client never connected (state %s)", client.ConnectionState())
		}
		break
	}
	pair, err := client.GetTransceivers()[0].Receiver().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		t.Fatalf("no selected pair: %v", err)
	}
	if pair.Local.Typ != webrtc.ICECandidateTypeRelay {
		t.Fatalf("selected local candidate is %s, want relay", pair.Local.Typ)
	}
	if pair.Remote.Port != uint16(port) || !net.ParseIP(pair.Remote.Address).IsLoopback() {
		t.Fatalf("relay reached %s:%d, want SFU loopback mux port %d", pair.Remote.Address, pair.Remote.Port, port)
	}
}
