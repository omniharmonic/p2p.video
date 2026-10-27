package signaling

import (
	"encoding/json"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdmissionDoesNotLeakUploadGrantToOtherParticipants(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	host := &Client{ParticipantID: "host-id", SessionID: "room", Role: "host", Status: "admitted", Send: make(chan []byte, 10)}
	guest := &Client{ParticipantID: "guest-id", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 10)}
	h.Register(host)
	h.Register(guest)
	h.AdmitParticipant("room", "guest-id")
	// Others may get the roster update, but never an admission or a grant.
	for len(host.Send) > 0 {
		var other Envelope
		_ = json.Unmarshal(<-host.Send, &other)
		if other.Type == TypeAdmission || strings.Contains(string(other.Payload), "uploadToken") {
			t.Fatal("guest upload credential broadcast to the host/other participants")
		}
	}
	admissions := 0
	var env Envelope
	for len(guest.Send) > 0 {
		var e Envelope
		_ = json.Unmarshal(<-guest.Send, &e)
		if e.Type == TypeAdmission {
			admissions++
			env = e
		}
	}
	if admissions != 1 {
		t.Fatalf("guest should get one admission, got %d", admissions)
	}
	var adm AdmissionPayload
	_ = json.Unmarshal(env.Payload, &adm)
	claims, err := VerifyUploadToken(adm.UploadToken, []byte("secret"))
	if err != nil || claims.ParticipantID != "guest-id" {
		t.Fatalf("bad grant: %v", err)
	}
}

func TestDeniedParticipantDisconnectRemainsRevoked(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	_ = st.CreateParticipant(&store.Participant{ID: "guest", SessionID: "room", Status: "denied", JoinedAt: time.Now()})
	h := NewHub(st, &config.Config{}, nil)
	defer h.Close()
	c := &Client{ParticipantID: "guest", SessionID: "room", Role: "guest", Status: "denied", Send: make(chan []byte, 10)}
	h.Register(c)
	h.Unregister(c)
	p, err := st.GetParticipant("guest")
	if err != nil || p.Status != "denied" {
		t.Fatalf("revocation undone on disconnect: %v %v", p, err)
	}
}

func TestUnregisterStopsClientWriter(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := NewHub(st, &config.Config{}, nil)
	defer h.Close()
	c := &Client{ParticipantID: "p", SessionID: "s", Send: make(chan []byte, 10)}
	h.Register(c)
	h.Unregister(c)
	// Once disconnected, future callbacks must not retain queued signaling messages.
	c.SendEnvelope(&Envelope{Type: TypeDashboard})
	if len(c.Send) != 0 {
		t.Fatal("disconnected client continues accepting messages with no writer")
	}
}
