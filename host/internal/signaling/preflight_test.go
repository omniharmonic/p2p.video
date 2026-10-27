package signaling

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func TestPreflightFixtureMatchesProtocol(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "protocol", "preflight.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Type != TypePreflight {
		t.Fatalf("fixture type %q", env.Type)
	}
	dec := json.NewDecoder(bytes.NewReader(env.Payload))
	dec.DisallowUnknownFields()
	var p PreflightPayload
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 9 || p.Overall != PreflightFail {
		t.Fatalf("unexpected fixture payload %+v", p)
	}
	// The fixture is already in sanitized form.
	clean := SanitizePreflight(p)
	if clean.Overall != p.Overall || len(clean.Items) != len(p.Items) {
		t.Fatalf("fixture not stable under sanitize: %+v", clean)
	}
}

func TestSanitizePreflight(t *testing.T) {
	cases := []struct {
		name    string
		items   []PreflightItem
		overall string
		n       int
	}{
		{"empty is ok", nil, PreflightOK, 0},
		{"info does not count", []PreflightItem{{ID: "power", Status: PreflightInfo}}, PreflightOK, 1},
		{"warn", []PreflightItem{{ID: "a", Status: PreflightOK}, {ID: "b", Status: PreflightWarn}}, PreflightWarn, 2},
		{"fail wins", []PreflightItem{{ID: "a", Status: PreflightFail}, {ID: "b", Status: PreflightWarn}}, PreflightFail, 2},
		{"unknown status and empty id dropped", []PreflightItem{{ID: "a", Status: "red"}, {ID: "", Status: PreflightFail}}, PreflightOK, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizePreflight(PreflightPayload{ParticipantID: "spoof", Name: "spoof", Overall: PreflightOK, Items: tc.items})
			if got.Overall != tc.overall || len(got.Items) != tc.n || got.ParticipantID != "" || got.Name != "" {
				t.Fatalf("got %+v", got)
			}
		})
	}
	many := make([]PreflightItem, 40)
	for i := range many {
		many[i] = PreflightItem{ID: strings.Repeat("x", 100), Label: strings.Repeat("é", 100), Status: PreflightOK, Detail: strings.Repeat("d", 1000)}
	}
	got := SanitizePreflight(PreflightPayload{Items: many})
	if len(got.Items) != PreflightMaxItems {
		t.Fatalf("items not capped: %d", len(got.Items))
	}
	it := got.Items[0]
	if len([]rune(it.ID)) != PreflightMaxIDLen || len([]rune(it.Label)) != PreflightMaxLabelLen || len([]rune(it.Detail)) != PreflightMaxDetailLen {
		t.Fatalf("strings not truncated: %d %d %d", len(it.ID), len([]rune(it.Label)), len(it.Detail))
	}
}

func TestPreflightReachesHostDashboardWithServerIdentity(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", Title: "Room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	newClient := func(id, name, role, status string) *Client {
		c := &Client{ParticipantID: id, SessionID: "room", Name: name, Role: role, Status: status, Send: make(chan []byte, 64)}
		h.Register(c)
		return c
	}
	host := newClient("host-id", "Host", "host", "admitted")
	pending := newClient("pending-id", "Waiting guest", "guest", "pending")
	denied := newClient("denied-id", "Denied", "guest", "denied")

	msg, _ := json.Marshal(map[string]any{"t": TypePreflight, "payload": map[string]any{
		"participantId": "host-id", "name": "Impostor", "overall": "ok",
		"items": []map[string]string{{"id": "mic", "label": "Microphone", "status": "fail", "detail": "No signal"}},
	}})
	h.HandleMessage(pending, msg)
	h.HandleMessage(denied, msg)
	h.emitDashboardTelemetry()

	var dash *DashboardPayload
	for {
		select {
		case b := <-host.Send:
			var env Envelope
			_ = json.Unmarshal(b, &env)
			if env.Type == TypeDashboard {
				dash = &DashboardPayload{}
				_ = json.Unmarshal(env.Payload, dash)
			}
			continue
		default:
		}
		break
	}
	if dash == nil {
		t.Fatal("host received no dashboard")
	}
	byID := map[string]DashboardParticipantState{}
	for _, p := range dash.Participants {
		byID[p.ParticipantID] = p
	}
	got := byID["pending-id"].Preflight
	if got == nil || got.ParticipantID != "pending-id" || got.Name != "Waiting guest" || got.Overall != PreflightFail || len(got.Items) != 1 {
		t.Fatalf("pending guest preflight %+v", got)
	}
	if byID["host-id"].Preflight != nil {
		t.Fatal("spoofed participantId attached preflight to the host")
	}
	if byID["denied-id"].Preflight != nil {
		t.Fatal("denied participant preflight stored")
	}
	if got.ReceivedAtMs == 0 {
		t.Fatal("hub did not stamp ReceivedAtMs")
	}
}

func countRefreshes(c *Client) int {
	n := 0
	for {
		select {
		case b := <-c.Send:
			var env Envelope
			_ = json.Unmarshal(b, &env)
			var p PreflightPayload
			_ = json.Unmarshal(env.Payload, &p)
			if env.Type == TypePreflight && p.Refresh {
				n++
			}
		default:
			return n
		}
	}
}

// Before a take the host asks admitted participants to re-check, so the
// record confirmation sees a fresh snapshot. Only an admitted host can ask.
func TestPreflightRefreshRelaysFromHostOnly(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	newClient := func(id, session, role, status string) *Client {
		c := &Client{ParticipantID: id, SessionID: session, Name: id, Role: role, Status: status, Send: make(chan []byte, 64)}
		h.Register(c)
		return c
	}
	host := newClient("host-id", "room", "host", "admitted")
	guest := newClient("guest-id", "room", "guest", "admitted")
	pending := newClient("pending-id", "room", "guest", "pending")
	outsider := newClient("outsider-id", "other", "guest", "admitted")

	msg, _ := json.Marshal(map[string]any{"t": TypePreflight, "payload": map[string]any{"refresh": true, "items": []map[string]string{{"id": "mic", "label": "Microphone", "status": "ok"}}}})
	h.HandleMessage(guest, msg)
	if countRefreshes(host)+countRefreshes(pending) != 0 {
		t.Fatal("a guest triggered a refresh")
	}
	h.HandleMessage(host, msg)
	if n := countRefreshes(guest); n != 1 {
		t.Fatalf("admitted guest got %d refreshes", n)
	}
	if countRefreshes(pending) != 0 || countRefreshes(outsider) != 0 || countRefreshes(host) != 0 {
		t.Fatal("refresh reached pending, another session, or the host itself")
	}
	host.mu.RLock()
	stored := host.preflight
	host.mu.RUnlock()
	if stored == nil || stored.Refresh || len(stored.Items) != 1 {
		t.Fatalf("host's own checklist not stored cleanly: %+v", stored)
	}
	// The dashboard carries the refresh time so the host can tell which
	// reports were made after it asked.
	h.emitDashboardTelemetry()
	var refreshAt int64
	for {
		select {
		case b := <-host.Send:
			var env Envelope
			_ = json.Unmarshal(b, &env)
			if env.Type == TypeDashboard {
				var d DashboardPayload
				_ = json.Unmarshal(env.Payload, &d)
				refreshAt = d.PreflightRefreshAtMs
			}
			continue
		default:
		}
		break
	}
	if refreshAt == 0 || refreshAt != stored.ReceivedAtMs {
		t.Fatalf("dashboard refresh time %d, host report %d", refreshAt, stored.ReceivedAtMs)
	}
}

// The recording mic report reaches the host only with known enums [SL-P1-10].
func TestSanitizePreflightRecordingMic(t *testing.T) {
	good := &RecordingMicState{Mode: RecordingMicAuto, Resolved: RecordingMicClean, Reason: RecordingMicReasonNoHeadphones, EchoCancellation: true, NoiseSuppression: true}
	out := SanitizePreflight(PreflightPayload{Items: []PreflightItem{}, RecordingMic: good})
	if out.RecordingMic == nil || *out.RecordingMic != *good {
		t.Fatalf("valid report dropped or changed: %+v", out.RecordingMic)
	}
	for _, bad := range []*RecordingMicState{
		{Mode: "loud", Resolved: RecordingMicRaw},
		{Mode: RecordingMicRaw, Resolved: RecordingMicAuto},
		{Mode: RecordingMicRaw, Resolved: "<img>"},
	} {
		if SanitizePreflight(PreflightPayload{RecordingMic: bad}).RecordingMic != nil {
			t.Fatalf("invalid report kept: %+v", bad)
		}
	}
	odd := SanitizeRecordingMic(&RecordingMicState{Mode: RecordingMicRaw, Resolved: RecordingMicRaw, Reason: "because <b>"})
	if odd == nil || odd.Reason != "" {
		t.Fatalf("unknown reason kept: %+v", odd)
	}
}
