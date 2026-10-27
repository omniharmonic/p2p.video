package signaling

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func TestParticipantStateFixtureMatchesProtocol(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "protocol", "participant-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeParticipantState {
		t.Fatalf("fixture type %q", env.Type)
	}
	dec := json.NewDecoder(bytes.NewReader(env.Payload))
	dec.DisallowUnknownFields()
	var p ParticipantStatePayload
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.ParticipantID != "p_fixture" || !p.MicMuted || p.CamOff {
		t.Fatalf("unexpected fixture payload %+v", p)
	}
}

func drainEnvelopes(c *Client, msgType string) []ParticipantStatePayload {
	var out []ParticipantStatePayload
	for {
		select {
		case b := <-c.Send:
			var env Envelope
			_ = json.Unmarshal(b, &env)
			if env.Type != msgType {
				continue
			}
			var p ParticipantStatePayload
			_ = json.Unmarshal(env.Payload, &p)
			out = append(out, p)
		default:
			return out
		}
	}
}

func TestParticipantStateRelayStampsIdentityAndSnapshotsLateJoiners(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	newClient := func(id, name, status string) *Client {
		c := &Client{ParticipantID: id, SessionID: "room", Name: name, Role: "guest", Status: status, Send: make(chan []byte, 32)}
		h.Register(c)
		return c
	}
	host := newClient("host-id", "Host", "admitted")
	guest := newClient("guest-id", "Guest", "admitted")
	pending := newClient("pending-id", "Waiting", "pending")
	outsider := &Client{ParticipantID: "elsewhere-id", SessionID: "other", Status: "admitted", Send: make(chan []byte, 32)}
	h.Register(outsider)

	// A spoofed identity is replaced by the authenticated connection's.
	msg, _ := json.Marshal(map[string]any{"t": TypeParticipantState, "payload": map[string]any{"participantId": "host-id", "name": "Impostor", "micMuted": true, "camOff": false}})
	h.HandleMessage(guest, msg)
	got := drainEnvelopes(host, TypeParticipantState)
	if len(got) != 1 || got[0].ParticipantID != "guest-id" || got[0].Name != "Guest" || !got[0].MicMuted || got[0].CamOff {
		t.Fatalf("host received %+v", got)
	}
	if len(drainEnvelopes(pending, TypeParticipantState)) != 0 {
		t.Fatal("state leaked to a participant awaiting admission")
	}
	if len(drainEnvelopes(outsider, TypeParticipantState)) != 0 {
		t.Fatal("state crossed sessions")
	}
	drainEnvelopes(guest, TypeParticipantState)

	// Unadmitted participants cannot publish state.
	h.HandleMessage(pending, msg)
	if len(drainEnvelopes(host, TypeParticipantState)) != 0 {
		t.Fatal("pending participant state relayed")
	}

	// The host's first state message returns the guest's latest state.
	msg, _ = json.Marshal(map[string]any{"t": TypeParticipantState, "payload": map[string]any{"micMuted": false, "camOff": true}})
	h.HandleMessage(host, msg)
	hostView := drainEnvelopes(host, TypeParticipantState)
	var sawGuest bool
	for _, p := range hostView {
		if p.ParticipantID == "guest-id" && p.MicMuted {
			sawGuest = true
		}
	}
	if !sawGuest {
		t.Fatalf("late state publisher missed snapshot: %+v", hostView)
	}
	guestView := drainEnvelopes(guest, TypeParticipantState)
	if len(guestView) != 1 || guestView[0].ParticipantID != "host-id" || !guestView[0].CamOff {
		t.Fatalf("guest received %+v", guestView)
	}
	// Later updates are deltas only.
	h.HandleMessage(host, msg)
	if n := len(drainEnvelopes(host, TypeParticipantState)); n != 1 {
		t.Fatalf("repeat state resent snapshot: %d envelopes", n)
	}
}
