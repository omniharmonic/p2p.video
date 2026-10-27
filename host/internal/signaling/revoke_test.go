package signaling

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// TestL1DenyRemoveAndEndRevokeRelayAccess: denying or removing a participant,
// or ending the meeting, calls OnParticipantRevoked (which closes live TURN
// allocations) and ParticipantMayRelay turns false [SL-SEC-1].
func TestL1DenyRemoveAndEndRevokeRelayAccess(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	var mu sync.Mutex
	var revoked []string
	h.OnParticipantRevoked = func(pid string) { mu.Lock(); revoked = append(revoked, pid); mu.Unlock() }
	for _, id := range []string{"a", "b", "c"} {
		_ = st.CreateParticipant(&store.Participant{ID: id, SessionID: "room", Role: "guest", Status: "admitted", JoinedAt: time.Now()})
		h.Register(&Client{ParticipantID: id, SessionID: "room", Role: "guest", Status: "admitted", Send: make(chan []byte, 64)})
	}
	if !h.ParticipantMayRelay("a") {
		t.Fatal("admitted participant should relay")
	}
	h.DenyParticipant("room", "a", "bye")
	if h.ParticipantMayRelay("a") {
		t.Fatal("denied participant may still relay")
	}
	_ = st.EndSession("room", time.Now())
	h.EndSession("room", SessionEndByHost)
	if h.ParticipantMayRelay("b") {
		t.Fatal("participant of an ended meeting may still relay")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"a", "b", "c"} {
		if !slices.Contains(revoked, id) {
			t.Errorf("%s not revoked: %v", id, revoked)
		}
	}
}
