package signaling

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/turn"
)

var iceTestTURNSecret = []byte("turn-secret-for-tests-0123456789")

func TestBuildICEServersPolicy(t *testing.T) {
	now := time.Now()
	stun := []string{"stun:stun.cloudflare.com:3478"}
	turnURLs := []string{"turns:studio.tail1.ts.net:8443?transport=tcp"}
	cases := []struct {
		name               string
		cfg                *config.Config
		wantSTUN, wantTURN bool
	}{
		{"nil config", nil, false, false},
		{"no policy", &config.Config{}, false, false},
		{"public STUN allowed", &config.Config{PublicSTUN: true, STUNURLs: stun}, true, false},
		{"public STUN disabled", &config.Config{PublicSTUN: false, STUNURLs: stun}, false, false},
		{"TURN urls but relay not running", &config.Config{TURNURLs: turnURLs}, false, false},
		{"TURN running but not advertised", &config.Config{TURNSecret: iceTestTURNSecret}, false, false},
		{"TURN only (no public STUN)", &config.Config{TURNSecret: iceTestTURNSecret, TURNURLs: turnURLs}, false, true},
		{"STUN and TURN", &config.Config{PublicSTUN: true, STUNURLs: stun, TURNSecret: iceTestTURNSecret, TURNURLs: turnURLs}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			servers := BuildICEServers(tc.cfg, "p_guest", now)
			var gotSTUN, gotTURN bool
			for _, s := range servers {
				for _, u := range s.URLs {
					switch {
					case strings.HasPrefix(u, "stun:"):
						gotSTUN = true
						if s.Username != "" || s.Credential != "" {
							t.Fatal("credentials attached to STUN")
						}
					case strings.HasPrefix(u, "turn"):
						gotTURN = true
						pid, err := turn.Verify(iceTestTURNSecret, s.Username, s.Credential, now)
						if err != nil || pid != "p_guest" {
							t.Fatalf("TURN credential not bound to participant: %q %v", pid, err)
						}
						if _, err := turn.Verify(iceTestTURNSecret, s.Username, s.Credential, now.Add(turn.DefaultTTL+time.Second)); err == nil {
							t.Fatal("TURN credential outlives TTL")
						}
					}
				}
			}
			if gotSTUN != tc.wantSTUN || gotTURN != tc.wantTURN {
				t.Fatalf("stun=%v turn=%v, want %v %v", gotSTUN, gotTURN, tc.wantSTUN, tc.wantTURN)
			}
		})
	}
	if s := BuildICEServers(&config.Config{TURNSecret: iceTestTURNSecret, TURNURLs: turnURLs}, "", now); len(s) != 0 {
		t.Fatal("TURN minted without a participant")
	}
}

func readAdmission(t *testing.T, c *Client) (AdmissionPayload, string) {
	t.Helper()
	for {
		select {
		case raw := <-c.Send:
			var env Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			if env.Type != TypeAdmission {
				continue
			}
			var adm AdmissionPayload
			if err := json.Unmarshal(env.Payload, &adm); err != nil {
				t.Fatal(err)
			}
			return adm, string(raw)
		default:
			t.Fatal("no admission message")
		}
	}
}

func drain(c *Client) string {
	var all strings.Builder
	for {
		select {
		case raw := <-c.Send:
			all.Write(raw)
		default:
			return all.String()
		}
	}
}

func TestTURNCredentialsOnlyForAdmittedParticipants(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateSession(&store.Session{ID: "room", AutoAdmit: false, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SecretKey:  []byte("upload-secret-upload-secret-0123"),
		PublicSTUN: true, STUNURLs: []string{"stun:stun.l.google.com:19302"},
		TURNSecret: iceTestTURNSecret, TURNURLs: []string{"turn:127.0.0.1:3478?transport=tcp"},
	}
	h := NewHub(st, cfg, nil)
	defer h.Close()

	host := &Client{ParticipantID: "p_host", SessionID: "room", Role: "host", Status: "admitted", Send: make(chan []byte, 32)}
	h.Register(host)
	h.handleHello(host, &HelloPayload{Name: "Host", Role: "host"})
	hostAdm, _ := readAdmission(t, host)
	if hostAdm.Status != "admitted" || len(hostAdm.IceServers) != 2 {
		t.Fatalf("host admission missing ICE servers: %+v", hostAdm)
	}

	pending := &Client{ParticipantID: "p_pending", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 32)}
	h.Register(pending)
	h.handleHello(pending, &HelloPayload{Name: "Pending", Role: "guest"})
	adm, raw := readAdmission(t, pending)
	if adm.Status != "pending" || len(adm.IceServers) != 0 || strings.Contains(raw, "credential") || strings.Contains(raw, "turn:") {
		t.Fatalf("pending participant received ICE/TURN data: %s", raw)
	}
	if h.ParticipantMayRelay("p_pending") {
		t.Fatal("pending participant may relay")
	}
	// The host's join-request notification must not carry credentials either.
	if leaked := drain(host); strings.Contains(leaked, "credential") {
		t.Fatalf("credential leaked to host: %s", leaked)
	}

	h.DenyParticipant("room", "p_pending", "test")
	adm, raw = readAdmission(t, pending)
	if adm.Status != "denied" || len(adm.IceServers) != 0 || strings.Contains(raw, "credential") {
		t.Fatalf("denied participant received ICE/TURN data: %s", raw)
	}
	if h.ParticipantMayRelay("p_pending") {
		t.Fatal("denied participant may relay")
	}

	guest := &Client{ParticipantID: "p_guest", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 32)}
	h.Register(guest)
	h.handleHello(guest, &HelloPayload{Name: "Guest", Role: "guest"})
	_, _ = readAdmission(t, guest)
	drain(host)
	h.AdmitParticipant("room", "p_guest")
	adm, _ = readAdmission(t, guest)
	if adm.Status != "admitted" {
		t.Fatalf("guest not admitted: %+v", adm)
	}
	var turnServer *ICEServer
	for i := range adm.IceServers {
		if adm.IceServers[i].Username != "" {
			turnServer = &adm.IceServers[i]
		}
	}
	if turnServer == nil {
		t.Fatalf("admitted guest got no TURN entry: %+v", adm.IceServers)
	}
	if pid, err := turn.Verify(iceTestTURNSecret, turnServer.Username, turnServer.Credential, time.Now()); err != nil || pid != "p_guest" {
		t.Fatalf("bad TURN credential: %v", err)
	}
	if leaked := drain(host); strings.Contains(leaked, turnServer.Credential) {
		t.Fatal("guest TURN credential broadcast to host")
	}
	if !h.ParticipantMayRelay("p_guest") {
		t.Fatal("admitted participant refused relay")
	}
	h.DenyParticipant("room", "p_guest", "kicked")
	if h.ParticipantMayRelay("p_guest") {
		t.Fatal("removed participant keeps relay access")
	}
	if h.ParticipantMayRelay("p_unknown") {
		t.Fatal("unknown participant may relay")
	}
}
