package signaling

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// The host's admission banner shows the first waiting person in the
// dashboard. With map order it changed every second, so the name could flip
// between two waiting people (and Admit could hit the other one) [SL-QA-1].
func TestDashboardListsParticipantsInArrivalOrder(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", Title: "Room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	host := &Client{ParticipantID: "p_m-host", SessionID: "room", Name: "Host", Role: "host", Status: "admitted", Send: make(chan []byte, 512)}
	h.Register(host)
	// IDs deliberately not in arrival order.
	want := []string{"p_m-host", "p_z", "p_a", "p_q", "p_c", "p_y", "p_b"}
	for _, id := range want[1:] {
		role := "guest"
		if id == "p_c" {
			role = "producer"
		}
		h.Register(&Client{ParticipantID: id, SessionID: "room", Name: id, Role: role, Status: "pending", Send: make(chan []byte, 64)})
	}
	for round := 0; round < 30; round++ {
		h.emitDashboardTelemetry()
		var dash *DashboardPayload
		for dash == nil {
			var env Envelope
			_ = json.Unmarshal(<-host.Send, &env)
			if env.Type == TypeDashboard {
				dash = &DashboardPayload{}
				_ = json.Unmarshal(env.Payload, dash)
			}
		}
		got := make([]string, len(dash.Participants))
		for i, p := range dash.Participants {
			got[i] = p.ParticipantID
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("round %d: dashboard order %v, want arrival order %v", round, got, want)
			}
		}
	}
}
