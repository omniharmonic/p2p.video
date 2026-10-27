package signaling

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// A room remembers each person's lower third by display name and gives it
// back when they join the next episode [SL-P5-4]; clearing it forgets it,
// and an instant meeting remembers nothing.
func TestLowerThirdsRememberedAcrossRoomEpisodes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.CreateRoom(&store.Room{ID: "r1", Name: "Show", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*store.Session{{ID: "ep1", RoomID: "r1", CreatedAt: now}, {ID: "ep2", RoomID: "r1", CreatedAt: now}, {ID: "ep3", RoomID: "r1", CreatedAt: now}, {ID: "adhoc", CreatedAt: now}} {
		if err := st.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHub(st, &config.Config{SecretKey: []byte("k")}, nil)
	defer h.Close()
	join := func(sess, id, name, role string) *Client {
		c := &Client{ParticipantID: id, SessionID: sess, Role: role, Status: "pending", Send: make(chan []byte, 256)}
		if role == RoleHost {
			c.Status = "admitted"
		}
		h.Register(c)
		h.handleHello(c, &HelloPayload{Name: name, Role: role})
		if role != RoleHost {
			h.AdmitParticipant(sess, id)
		}
		return c
	}
	send := func(c *Client, p LowerThirdPayload) {
		raw, _ := json.Marshal(map[string]any{"t": TypeLowerThird, "payload": p})
		h.HandleMessage(c, raw)
	}
	lastBranding := func(c *Client) BrandingPayload {
		t.Helper()
		envs := drainEnv(c, TypeBranding)
		if len(envs) == 0 {
			t.Fatalf("no branding for %s", c.ParticipantID)
		}
		var b BrandingPayload
		_ = json.Unmarshal(envs[len(envs)-1].Payload, &b)
		return b
	}

	host1 := join("ep1", "h1", "Host", RoleHost)
	join("ep1", "g1", "Ana Ruiz", RoleGuest)
	send(host1, LowerThirdPayload{ParticipantID: "g1", Name: "Dr. Ana Ruiz", Title: "Founder, Acme"})
	if b := lastBranding(host1); !b.RememberTitles {
		t.Fatal("a room meeting should say titles are remembered")
	}
	if rem, err := st.RoomLowerThirdFor("r1", "ana ruiz"); err != nil || rem.Name != "Dr. Ana Ruiz" || rem.Title != "Founder, Acme" {
		t.Fatalf("room memory %v %+v", err, rem)
	}

	// Next episode: the same person (differently spaced/cased name) gets it
	// back, everyone sees it, and it is the meeting's own lower third (so
	// the take manifest has it). Someone else gets nothing.
	host2 := join("ep2", "h2", "Host", RoleHost)
	drainEnv(host2)
	guest2 := join("ep2", "g2", "  ana  RUIZ ", RoleGuest)
	join("ep2", "b2", "Bo", RoleGuest)
	for _, c := range []*Client{host2, guest2} {
		b := lastBranding(c)
		if len(b.LowerThirds) != 1 || b.LowerThirds[0].ParticipantID != "g2" || b.LowerThirds[0].Name != "Dr. Ana Ruiz" || b.LowerThirds[0].Title != "Founder, Acme" {
			t.Fatalf("episode 2 branding for %s: %+v", c.ParticipantID, b.LowerThirds)
		}
	}
	if lts, _ := st.ListLowerThirds("ep2"); lts["g2"].Title != "Founder, Acme" || len(lts) != 1 {
		t.Fatalf("episode 2 lower thirds %+v", lts)
	}
	// An edit in this meeting is kept on reconnect and remembered.
	send(host2, LowerThirdPayload{ParticipantID: "g2", Title: "CEO, Acme"})
	h.sendProduction(guest2)
	if lts, _ := st.ListLowerThirds("ep2"); lts["g2"].Title != "CEO, Acme" || lts["g2"].Name != "" {
		t.Fatalf("edit kept %+v", lts["g2"])
	}
	if rem, _ := st.RoomLowerThirdFor("r1", "Ana Ruiz"); rem == nil || rem.Title != "CEO, Acme" || rem.Name != "" {
		t.Fatalf("edit remembered %+v", rem)
	}
	// Clearing both fields forgets it for the room.
	send(host2, LowerThirdPayload{ParticipantID: "g2"})
	if _, err := st.RoomLowerThirdFor("r1", "Ana Ruiz"); err == nil {
		t.Fatal("cleared title still remembered")
	}
	host3 := join("ep3", "h3", "Host", RoleHost)
	drainEnv(host3)
	join("ep3", "g3", "Ana Ruiz", RoleGuest)
	if lts, _ := st.ListLowerThirds("ep3"); len(lts) != 0 {
		t.Fatalf("forgotten title came back: %+v", lts)
	}

	// An instant meeting never writes room memory.
	hostA := join("adhoc", "ha", "Host", RoleHost)
	join("adhoc", "ga", "Cy", RoleGuest)
	send(hostA, LowerThirdPayload{ParticipantID: "ga", Title: "Guest"})
	if b := lastBranding(hostA); b.RememberTitles {
		t.Fatal("instant meeting claims titles are remembered")
	}
	if list, _ := st.ListRoomLowerThirds("r1"); len(list) != 0 {
		t.Fatalf("room memory %+v", list)
	}
}
