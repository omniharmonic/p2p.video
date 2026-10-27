package signaling

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func TestSanitizeNetStats(t *testing.T) {
	got := SanitizeNetStats(NetStatsPayload{
		ParticipantID: "spoof", Name: "spoof", ReceivedAtMs: 1,
		Quality: "excellent", Path: "wormhole", Protocol: strings.Repeat("u", 40),
		RTTMs: -5, AvailableOutgoingKbps: math.MaxInt32, OutboundLossPct: 250, InboundLossPct: math.NaN(),
		JitterMs: 999_999, FramesDroppedPct: -1, CapTier: 99, QualityLimitation: strings.Repeat("b", 100),
		Reasons: []string{"", "a", "b", "c", "d", strings.Repeat("é", 100)},
		Upload:  &NetUploadStats{PendingBytes: -1, RateBps: -1, Mode: "turbo", Concurrency: 64},
	})
	if got.ParticipantID != "" || got.Name != "" || got.ReceivedAtMs != 0 {
		t.Fatalf("identity not cleared: %+v", got)
	}
	if got.Quality != NetQualityUnknown || got.Path != "" {
		t.Fatalf("enums not validated: %q %q", got.Quality, got.Path)
	}
	if got.RTTMs != 0 || got.OutboundLossPct != 100 || got.InboundLossPct != 0 || got.JitterMs != 60_000 || got.FramesDroppedPct != 0 || got.CapTier != 16 {
		t.Fatalf("numbers not clamped: %+v", got)
	}
	if len(got.Protocol) != NetStatsMaxProtoLen || len(got.QualityLimitation) != NetStatsMaxLimitLen {
		t.Fatalf("strings not truncated: %q %q", got.Protocol, got.QualityLimitation)
	}
	if len(got.Reasons) != NetStatsMaxReasons || got.Reasons[0] != "a" {
		t.Fatalf("reasons %v", got.Reasons)
	}
	if u := got.Upload; u == nil || u.PendingBytes != 0 || u.RateBps != 0 || u.Mode != UploadModeIdle || u.Concurrency != 16 {
		t.Fatalf("upload %+v", got.Upload)
	}
	ok := SanitizeNetStats(NetStatsPayload{Quality: NetQualityPoor, Path: NetPathTailnet})
	if ok.Quality != NetQualityPoor || ok.Path != NetPathTailnet || ok.Upload != nil {
		t.Fatalf("valid report changed: %+v", ok)
	}
}

func drainAllEnvelopes(c *Client) []Envelope {
	var out []Envelope
	for {
		select {
		case b := <-c.Send:
			var env Envelope
			_ = json.Unmarshal(b, &env)
			out = append(out, env)
		default:
			return out
		}
	}
}

func TestNetStatsRelayedWithServerIdentityAndOnDashboard(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", Title: "Room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	now := time.UnixMilli(1_761_400_000_000)
	h.now = func() time.Time { return now }
	newClient := func(id, session, role, status string) *Client {
		c := &Client{ParticipantID: id, SessionID: session, Name: "Name " + id, Role: role, Status: status, Send: make(chan []byte, 64)}
		h.Register(c)
		return c
	}
	host := newClient("host-id", "room", "host", "admitted")
	guest := newClient("guest-id", "room", "guest", "admitted")
	pending := newClient("pending-id", "room", "guest", "pending")
	outsider := newClient("outsider-id", "other", "guest", "admitted")

	report := func(c *Client, quality string) {
		msg, _ := json.Marshal(map[string]any{"t": TypeNetStats, "payload": map[string]any{
			"participantId": "host-id", "name": "Impostor", "quality": quality, "path": "relay", "capTier": 2,
			"upload": map[string]any{"pendingBytes": 1 << 20, "rateBps": 800_000, "mode": "throttled", "concurrency": 1},
		}})
		h.HandleMessage(c, msg)
	}
	report(pending, NetQualityPoor) // not admitted: ignored
	report(guest, NetQualityFair)

	relayed := 0
	for _, env := range drainAllEnvelopes(host) {
		if env.Type != TypeNetStats {
			continue
		}
		relayed++
		var p NetStatsPayload
		_ = json.Unmarshal(env.Payload, &p)
		if env.ParticipantID != "guest-id" || p.ParticipantID != "guest-id" || p.Name != "Name guest-id" || p.Quality != NetQualityFair || p.ReceivedAtMs != now.UnixMilli() {
			t.Fatalf("relayed report %+v (envelope %q)", p, env.ParticipantID)
		}
	}
	if relayed != 1 {
		t.Fatalf("host got %d net_stats, want 1", relayed)
	}
	for _, c := range []*Client{guest, pending, outsider} {
		for _, env := range drainAllEnvelopes(c) {
			if env.Type == TypeNetStats {
				t.Fatalf("%s received a net_stats relay", c.ParticipantID)
			}
		}
	}

	// Faster than the minimum interval: dropped. After it: accepted.
	now = now.Add(100 * time.Millisecond)
	report(guest, NetQualityPoor)
	if n := len(drainAllEnvelopes(host)); n != 0 {
		t.Fatalf("flooding report relayed (%d)", n)
	}
	now = now.Add(NetStatsMinIntervalMs * time.Millisecond)
	report(guest, NetQualityPoor)

	h.emitDashboardTelemetry()
	var dash *DashboardPayload
	for _, env := range drainAllEnvelopes(host) {
		if env.Type == TypeDashboard {
			dash = &DashboardPayload{}
			_ = json.Unmarshal(env.Payload, dash)
		}
	}
	if dash == nil {
		t.Fatal("no dashboard")
	}
	for _, p := range dash.Participants {
		switch p.ParticipantID {
		case "guest-id":
			ns := p.NetStats
			if ns == nil || ns.Quality != NetQualityPoor || ns.Upload == nil || ns.Upload.PendingBytes != 1<<20 || ns.Upload.Mode != UploadModeThrottled {
				t.Fatalf("guest dashboard net stats %+v", ns)
			}
		case "host-id", "pending-id":
			if p.NetStats != nil {
				t.Fatalf("%s has net stats it never sent (spoofed id?)", p.ParticipantID)
			}
		}
	}
}
