package signaling

import (
	"encoding/json"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"path/filepath"
	"testing"
	"time"
)

func TestResumeRestoresOnlyAuthenticatedAdmittedIdentity(t *testing.T) {
	for _, status := range []string{"disconnected", "denied"} {
		t.Run(status, func(t *testing.T) {
			st, e := store.Open(filepath.Join(t.TempDir(), "db"))
			if e != nil {
				t.Fatal(e)
			}
			defer st.Close()
			_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
			_ = st.CreateParticipant(&store.Participant{ID: "guest", SessionID: "room", Role: "guest", Name: "Guest", Status: status, JoinedAt: time.Now()})
			h := NewHub(st, &config.Config{SecretKey: []byte("resume-secret")}, nil)
			defer h.Close()
			c := &Client{ParticipantID: "temporary", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 10)}
			h.Register(c)
			b, _ := json.Marshal(map[string]any{"t": "hello", "payload": map[string]any{"name": "Guest", "resumeToken": MintUploadToken("guest", "room", time.Hour, []byte("resume-secret"))}})
			h.HandleMessage(c, b)
			if status == "denied" {
				if c.getStatus() != "denied" {
					t.Fatal("revoked identity resumed")
				}
				return
			}
			if c.ParticipantID != "guest" || c.getStatus() != "admitted" {
				t.Fatalf("resume lost identity/admission: %s %s", c.ParticipantID, c.getStatus())
			}
			if _, ok := h.clients["temporary"]; ok {
				t.Fatal("temporary connection identity leaked")
			}
		})
	}
}
func TestStaleDisconnectDoesNotRemoveResumedConnection(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	h := NewHub(st, &config.Config{}, nil)
	defer h.Close()
	old := &Client{ParticipantID: "guest", SessionID: "room", Status: "admitted", Send: make(chan []byte, 10)}
	next := &Client{ParticipantID: "guest", SessionID: "room", Status: "admitted", Send: make(chan []byte, 10)}
	h.Register(old)
	h.Register(next)
	h.Unregister(old)
	if h.sessionGroups["room"]["guest"] != next {
		t.Fatal("stale websocket disconnect removed replacement")
	}
}

func TestResumeGrantCannotCrossSessionOrRole(t *testing.T) {
	st, e := store.Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	_ = st.CreateParticipant(&store.Participant{ID: "host", SessionID: "room", Role: "host", Status: "disconnected", JoinedAt: time.Now()})
	_ = st.CreateParticipant(&store.Participant{ID: "other", SessionID: "elsewhere", Role: "guest", Status: "disconnected", JoinedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	for _, id := range []string{"host", "other"} {
		c := &Client{ParticipantID: "temporary-" + id, SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 10)}
		h.Register(c)
		token := MintUploadToken(id, "room", time.Hour, []byte("secret"))
		if e := h.resumeClient(c, token); e == nil {
			t.Fatal("resume crossed role/session boundary")
		}
	}
}
