package sfu

import (
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// loopbackHarness drives real client PeerConnections against the SFU. Peers
// exchange real ICE/DTLS/SRTP over loopback; no physical devices or
// third-party STUN servers are used.
type loopbackHarness struct {
	t       *testing.T
	s       *SFU
	mu      sync.Mutex
	clients map[peerKey]*harnessClient
	errors  chan error
}

type harnessClient struct {
	pc       *webrtc.PeerConnection
	lock     sync.Mutex
	received chan string
	// ignoreOffers leaves server subscription offers unanswered, holding the
	// server peer in have-local-offer to reproduce glare on re-offer.
	ignoreOffers atomic.Bool
	offersSeen   atomic.Int32
}

func newLoopbackHarness(t *testing.T) *loopbackHarness {
	t.Helper()
	s, err := NewSFU(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Constrain the test to loopback: it must never involve public networks.
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	media := &webrtc.MediaEngine{}
	if err := media.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	interceptors := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(media, interceptors); err != nil {
		t.Fatal(err)
	}
	s.api = webrtc.NewAPI(webrtc.WithSettingEngine(settings), webrtc.WithMediaEngine(media), webrtc.WithInterceptorRegistry(interceptors))
	h := &loopbackHarness{t: t, s: s, clients: map[peerKey]*harnessClient{}, errors: make(chan error, 32)}
	s.SetOnOffer(func(session, id, sdp string) {
		h.mu.Lock()
		c := h.clients[peerKey{session, id}]
		h.mu.Unlock()
		if c == nil {
			return
		}
		c.offersSeen.Add(1)
		if c.ignoreOffers.Load() {
			return
		}
		c.lock.Lock()
		defer c.lock.Unlock()
		pc := c.pc
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
			h.errors <- err
			return
		}
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			h.errors <- err
			return
		}
		if err = pc.SetLocalDescription(answer); err != nil {
			h.errors <- err
			return
		}
		if err = s.HandleAnswer(session, id, pc.LocalDescription().SDP); err != nil {
			h.errors <- err
		}
	})
	return h
}

// join negotiates a fresh client PeerConnection for (session, id). Calling it
// again for the same participant models client media recovery: a new
// RTCPeerConnection re-offers while the previous one is abandoned.
func (h *loopbackHarness) join(session, id string, publish bool) *harnessClient {
	t := h.t
	t.Helper()
	s := h.s
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	key := peerKey{session, id}
	c := &harnessClient{pc: pc, received: make(chan string, 128)}
	t.Cleanup(func() { s.OnParticipantLeave(session, id); _ = pc.Close() })
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			_, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			select {
			case c.received <- track.StreamID():
			default:
			}
		}
	})
	var local *webrtc.TrackLocalStaticRTP
	if publish {
		local, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", id)
		if err != nil {
			t.Fatal(err)
		}
		sender, err := pc.AddTrack(local)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				if _, _, err := sender.ReadRTCP(); err != nil {
					return
				}
			}
		}()
	} else {
		if _, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		if err := s.HandleCandidate(session, id, init.Candidate, init.SDPMid, init.SDPMLineIndex); err != nil {
			h.errors <- err
		}
	})
	gathered := webrtc.GatheringCompletePromise(pc)
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("client ICE gathering timeout")
	}
	// Server offers for this participant are routed to the newest client, as
	// the signaling hub routes by participant ID.
	h.mu.Lock()
	h.clients[key] = c
	h.mu.Unlock()
	c.lock.Lock()
	_, err = s.HandleOffer(session, id, pc.LocalDescription().SDP)
	if err != nil {
		c.lock.Unlock()
		t.Fatal(err)
	}
	s.mu.Lock()
	host := s.peers[key].pc
	s.mu.Unlock()
	hostGathered := webrtc.GatheringCompletePromise(host)
	select {
	case <-hostGathered:
	case <-time.After(5 * time.Second):
		c.lock.Unlock()
		t.Fatal("server ICE gathering timeout")
	}
	err = pc.SetRemoteDescription(*host.LocalDescription())
	c.lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if local != nil {
		stop := make(chan struct{})
		t.Cleanup(func() { close(stop) })
		go func() {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			var seq uint16
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					seq++
					_ = local.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 960}, Payload: []byte{0xf8, 0xff, 0xfe}})
				}
			}
		}()
	}
	return c
}

func (h *loopbackHarness) waitFor(c *harnessClient, publisher string) {
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-h.errors:
			h.t.Fatalf("negotiation error: %v", err)
		case got := <-c.received:
			if got == publisher {
				return
			}
		case <-deadline:
			h.t.Fatalf("no RTP from %s", publisher)
		}
	}
}

func drain(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func (h *loopbackHarness) publicationsOf(session, id string) []*publication {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	var pubs []*publication
	for pub := range h.s.tracks {
		if pub.owner.key == (peerKey{session, id}) {
			pubs = append(pubs, pub)
		}
	}
	return pubs
}

func TestSessionRoutingRenegotiationAndLeave(t *testing.T) {
	h := newLoopbackHarness(t)
	s := h.s
	alice := h.join("room-a", "alice", true)
	outsider := h.join("room-b", "alice", true) // Same participant ID is isolated by session.
	bob := h.join("room-a", "bob", true)
	h.waitFor(bob, "alice")
	h.waitFor(alice, "bob")
	carol := h.join("room-a", "carol", false)
	h.waitFor(carol, "alice")
	h.waitFor(carol, "bob")
	select {
	case got := <-outsider.received:
		t.Fatalf("cross-session media from %s", got)
	case <-time.After(200 * time.Millisecond):
	}
	s.OnParticipantLeave("room-a", "bob")
	if len(h.publicationsOf("room-a", "bob")) != 0 {
		t.Error("departed publisher retained")
	}
	s.mu.Lock()
	if s.peers[peerKey{"room-b", "alice"}] == nil {
		t.Error("other session removed")
	}
	s.mu.Unlock()
	// Existing publishers must continue flowing after another participant leaves.
	h.waitFor(carol, "alice")
}

// A participant whose media path died re-offers from a new PeerConnection. The
// SFU must replace its peer, and every other participant must re-subscribe to
// the replacement's tracks while the re-offering participant regains theirs.
func TestReofferReplacesPeerAndOthersResubscribe(t *testing.T) {
	h := newLoopbackHarness(t)
	s := h.s
	alice := h.join("room", "alice", true)
	bob := h.join("room", "bob", true)
	carol := h.join("room", "carol", true)
	h.waitFor(bob, "alice")
	h.waitFor(carol, "alice")
	h.waitFor(alice, "bob")
	s.mu.Lock()
	oldPeer := s.peers[peerKey{"room", "alice"}]
	s.mu.Unlock()
	oldPubs := h.publicationsOf("room", "alice")
	if len(oldPubs) != 1 {
		t.Fatalf("alice publications before recovery: %d", len(oldPubs))
	}

	// Simulate the client abandoning a failed RTCPeerConnection.
	_ = alice.pc.Close()
	drain(bob.received)
	drain(carol.received)
	alice2 := h.join("room", "alice", true)

	s.mu.Lock()
	newPeer := s.peers[peerKey{"room", "alice"}]
	peerCount := len(s.peers)
	s.mu.Unlock()
	if newPeer == nil || newPeer == oldPeer {
		t.Fatal("re-offer did not replace the participant's peer")
	}
	if peerCount != 3 {
		t.Fatalf("expected 3 peers after replacement, got %d", peerCount)
	}
	if oldPeer.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
		// Close is asynchronous; give it a moment.
		deadline := time.Now().Add(3 * time.Second)
		for oldPeer.pc.ConnectionState() != webrtc.PeerConnectionStateClosed && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if oldPeer.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			t.Fatalf("replaced server peer not closed: %s", oldPeer.pc.ConnectionState())
		}
	}
	// Others receive RTP from the replacement publication only (the old client
	// PeerConnection is closed and can no longer send).
	h.waitFor(bob, "alice")
	h.waitFor(carol, "alice")
	// The recovered participant re-subscribes to everyone else.
	h.waitFor(alice2, "bob")
	h.waitFor(alice2, "carol")
	for _, pub := range h.publicationsOf("room", "alice") {
		if pub.owner != newPeer {
			t.Fatal("stale publication from replaced peer retained")
		}
		for _, old := range oldPubs {
			if pub == old {
				t.Fatal("old publication still routed")
			}
		}
	}
	s.mu.Lock()
	for _, other := range []peerKey{{"room", "bob"}, {"room", "carol"}} {
		for pub := range s.peers[other].senders {
			if pub.owner == oldPeer {
				t.Errorf("%s still subscribed to replaced peer", other.participant)
			}
		}
	}
	s.mu.Unlock()
}

// Glare: the server has a subscription offer outstanding for the participant
// when the client re-offers from a new PeerConnection. The fresh offer wins.
func TestReofferWhileServerOfferInFlight(t *testing.T) {
	h := newLoopbackHarness(t)
	s := h.s
	alice := h.join("room", "alice", true)
	alice.ignoreOffers.Store(true)
	bob := h.join("room", "bob", true)
	h.waitFor(bob, "alice")
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		awaiting := s.peers[peerKey{"room", "alice"}].awaiting
		s.mu.Unlock()
		if awaiting && alice.offersSeen.Load() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never offered bob's track to alice")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = alice.pc.Close()
	alice2 := h.join("room", "alice", true)
	h.waitFor(alice2, "bob")
	drain(bob.received)
	h.waitFor(bob, "alice")
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.peers[peerKey{"room", "alice"}]; p == nil || p.pc == alice.pc {
		t.Fatal("re-offer did not replace awaiting peer")
	}
}

func TestInvalidOfferDoesNotRetainPeer(t *testing.T) {
	s, err := NewSFU(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandleOffer("room", "bad", "not SDP"); err == nil {
		t.Fatal("invalid SDP accepted")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.peers) != 0 {
		t.Fatal("failed negotiation retained peer")
	}
}

func TestEarlyCandidatesBoundedAndClearedOnLeave(t *testing.T) {
	s, err := NewSFU(nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		if err := s.HandleCandidate("room", "alice", "", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.HandleCandidate("room", "alice", "", nil, nil); err == nil {
		t.Fatal("unbounded early candidates")
	}
	s.OnParticipantLeave("room", "alice")
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingCandidates) != 0 {
		t.Fatal("early candidates retained after disconnect")
	}
}
