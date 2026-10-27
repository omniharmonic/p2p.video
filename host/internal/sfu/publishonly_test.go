package sfu

import (
	"testing"
	"time"
)

// A phone sub-source publishes its camera to the call but receives nobody
// [SL-P1-9]: others decode it; it gets no subscriptions.
func TestPublishOnlyPeerSendsButReceivesNothing(t *testing.T) {
	h := newLoopbackHarness(t)
	h.s.SetPublishOnly("room", "phone", true)
	alice := h.join("room", "alice", true)
	phone := h.join("room", "phone", true)
	h.waitFor(alice, "phone")
	select {
	case got := <-phone.received:
		t.Fatalf("publish-only peer received media from %s", got)
	case <-time.After(500 * time.Millisecond):
	}
	h.s.mu.Lock()
	subs := len(h.s.peers[peerKey{"room", "phone"}].senders)
	h.s.mu.Unlock()
	if subs != 0 {
		t.Fatalf("publish-only peer has %d subscriptions", subs)
	}
	// Leaving clears the mark, so the participant ID starts clean next time.
	h.s.OnParticipantLeave("room", "phone")
	h.s.mu.Lock()
	marked := h.s.publishOnly[peerKey{"room", "phone"}]
	h.s.mu.Unlock()
	if marked {
		t.Fatal("publish-only mark survived leave")
	}
}
