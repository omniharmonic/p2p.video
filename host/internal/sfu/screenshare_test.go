package sfu

import (
	"errors"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// publishScreen adds a VP8 "screen" track to an already-negotiated client
// PeerConnection, renegotiates it through HandleRenegotiation and starts
// sending RTP. It returns the sender so the share can be removed again.
func (h *loopbackHarness) publishScreen(c *harnessClient, session, id, trackID string) *webrtc.RTPSender {
	t := h.t
	t.Helper()
	local, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, trackID, "client-chosen-stream")
	if err != nil {
		t.Fatal(err)
	}
	c.lock.Lock()
	sender, err := c.pc.AddTrack(local)
	c.lock.Unlock()
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
	h.renegotiate(c, session, id)
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
				_ = local.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(seq) * 3000, Marker: true}, Payload: []byte{0x10, 0x00, 0x9d, 0x01, 0x2a}})
			}
		}
	}()
	return sender
}

// renegotiate sends a client offer for the existing PeerConnection, as the
// browser does when it adds or removes a screen share.
func (h *loopbackHarness) renegotiate(c *harnessClient, session, id string) {
	t := h.t
	t.Helper()
	c.lock.Lock()
	defer c.lock.Unlock()
	offer, err := c.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := h.s.HandleRenegotiation(session, id, c.pc.LocalDescription().SDP)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatal(err)
	}
}

func (h *loopbackHarness) waitStable(session, id string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.s.mu.Lock()
		p := h.s.peers[peerKey{session, id}]
		stable := p != nil && !p.awaiting && !p.dirty
		h.s.mu.Unlock()
		if stable {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("%s never settled its subscriptions", id)
}

func (h *loopbackHarness) screenPublications(session, id string) int {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	n := 0
	for pub := range h.s.tracks {
		if pub.owner.key == (peerKey{session, id}) && pub.source == SourceScreen {
			n++
		}
	}
	return n
}

// A participant publishes camera (here: microphone) and then a screen on the
// same PeerConnection. Receivers get both, the screen under its own stream
// ID, and the camera keeps flowing. Ending the share server-side unpublishes
// it for everyone; the sharer's own removal renegotiates cleanly.
func TestScreenSharePublishesSecondSourceOnSamePeer(t *testing.T) {
	h := newLoopbackHarness(t)
	s := h.s
	alice := h.join("room", "alice", true)
	bob := h.join("room", "bob", true)
	carol := h.join("room", "carol", false)
	h.waitFor(bob, "alice")
	h.waitFor(carol, "alice")
	h.waitStable("room", "alice")

	s.mu.Lock()
	peerBefore := s.peers[peerKey{"room", "alice"}]
	s.mu.Unlock()
	s.SetScreenTracks("room", "alice", []string{"screen-video"})
	sender := h.publishScreen(alice, "room", "alice", "screen-video")
	h.waitFor(bob, "alice"+ScreenStreamSuffix)
	h.waitFor(carol, "alice"+ScreenStreamSuffix)
	drain(bob.received)
	h.waitFor(bob, "alice") // the camera/mic publication is unaffected
	s.mu.Lock()
	if s.peers[peerKey{"room", "alice"}] != peerBefore {
		t.Fatal("renegotiation replaced the publisher's peer")
	}
	s.mu.Unlock()
	if got := h.screenPublications("room", "alice"); got != 1 {
		t.Fatalf("screen publications = %d, want 1", got)
	}

	// Host stop / source_remove: the hub clears the declaration.
	s.SetScreenTracks("room", "alice", nil)
	if got := h.screenPublications("room", "alice"); got != 0 {
		t.Fatalf("screen still published after stop: %d", got)
	}
	h.waitStable("room", "bob")
	h.waitStable("room", "carol")
	s.mu.Lock()
	for _, k := range []peerKey{{"room", "bob"}, {"room", "carol"}} {
		for pub := range s.peers[k].senders {
			if pub.source == SourceScreen {
				t.Errorf("%s still subscribed to the stopped screen", k.participant)
			}
		}
	}
	s.mu.Unlock()
	// The sharer then removes its track and renegotiates; camera keeps flowing.
	alice.lock.Lock()
	err := alice.pc.RemoveTrack(sender)
	alice.lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.renegotiate(alice, "room", "alice")
	drain(bob.received)
	h.waitFor(bob, "alice")
	if got := h.screenPublications("room", "alice"); got != 0 {
		t.Fatalf("removed screen track republished: %d", got)
	}
}

// An undeclared extra track is forwarded as camera media; a declaration is
// what makes a track a screen. Declarations are per participant and session.
func TestScreenDeclarationIsScopedToParticipant(t *testing.T) {
	s, err := NewSFU(nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetScreenTracks("room", "alice", []string{"t1"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sourceOfLocked(peerKey{"room", "alice"}, "t1") != SourceScreen {
		t.Fatal("declared track not classified as screen")
	}
	for _, k := range []peerKey{{"room", "bob"}, {"other", "alice"}} {
		if s.sourceOfLocked(k, "t1") != SourceCamera {
			t.Fatalf("declaration leaked to %v", k)
		}
	}
	if s.sourceOfLocked(peerKey{"room", "alice"}, "t2") != SourceCamera {
		t.Fatal("undeclared track classified as screen")
	}
}

func TestScreenDeclarationClearedOnLeave(t *testing.T) {
	s, err := NewSFU(nil)
	if err != nil {
		t.Fatal(err)
	}
	s.SetScreenTracks("room", "alice", []string{"t1"})
	s.OnParticipantLeave("room", "alice")
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.screens) != 0 {
		t.Fatal("screen declaration retained after leave")
	}
}

// Glare: the server's subscription offer is outstanding when the client
// renegotiates. The server keeps its offer (impolite) and rejects the client's.
func TestRenegotiationGlareRejectedWhileServerOfferOutstanding(t *testing.T) {
	h := newLoopbackHarness(t)
	s := h.s
	alice := h.join("room", "alice", true)
	alice.ignoreOffers.Store(true)
	h.join("room", "bob", true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		awaiting := s.peers[peerKey{"room", "alice"}].awaiting
		s.mu.Unlock()
		if awaiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never offered bob's track to alice")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := s.HandleRenegotiation("room", "alice", alice.pc.LocalDescription().SDP); !errors.Is(err, ErrGlare) {
		t.Fatalf("renegotiation during server offer: err=%v, want ErrGlare", err)
	}
	if _, err := s.HandleRenegotiation("room", "nobody", "v=0"); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("renegotiation without peer: err=%v, want ErrNoPeer", err)
	}
}
