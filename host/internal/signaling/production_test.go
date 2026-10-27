package signaling

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func lastPrompter(t *testing.T, c *Client) PrompterPayload {
	t.Helper()
	envs := drainEnv(c, TypePrompter)
	if len(envs) == 0 {
		t.Fatalf("%s got no prompter state", c.ParticipantID)
	}
	var p PrompterPayload
	_ = json.Unmarshal(envs[len(envs)-1].Payload, &p)
	return p
}

func actionErrors(c *Client, action string) []ActionErrorPayload {
	var out []ActionErrorPayload
	for _, env := range drainEnv(c, TypeActionError) {
		var p ActionErrorPayload
		_ = json.Unmarshal(env.Payload, &p)
		if p.Action == action {
			out = append(out, p)
		}
	}
	return out
}

func strPtr(s string) *string { return &s }

// The visibility matrix and edit rights [SL-P5-1]: only the host sets
// visibility; the producer edits unless the script is private to the host;
// guests never edit and receive the text only when it is for everyone.
func TestPrompterVisibilityAndRoleEnforcement(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("h", "Host", "host", true)
	prod := r.join("p", "Pat", RoleProducer, true)
	guest := r.join("g", "Gia", "guest", true)
	for _, c := range []*Client{host, prod, guest} {
		if p := lastPrompter(t, c); p.Visibility != PrompterHost || p.Text != "" {
			t.Fatalf("%s initial %+v", c.ParticipantID, p)
		}
	}

	r.send(host, TypePrompterEdit, PrompterEditPayload{Text: strPtr("# Intro\nSecret notes")})
	if p := lastPrompter(t, host); p.Text != "# Intro\nSecret notes" || p.Revision != 1 || p.UpdatedBy != "Host" {
		t.Fatalf("host %+v", p)
	}
	for _, c := range []*Client{prod, guest} {
		if p := lastPrompter(t, c); !p.Hidden || p.Text != "" {
			t.Fatalf("%s saw a host-only script: %+v", c.ParticipantID, p)
		}
	}

	// Negative: a guest can't edit or change visibility; neither changes anything.
	r.send(guest, TypePrompterEdit, PrompterEditPayload{Text: strPtr("hijack")})
	r.send(guest, TypePrompterEdit, PrompterEditPayload{Visibility: PrompterEveryone})
	if errs := actionErrors(guest, TypePrompterEdit); len(errs) != 2 || errs[0].Code != ErrCodeForbidden {
		t.Fatalf("guest edit errors %+v", errs)
	}
	// Negative: the producer can't edit a host-only script.
	r.send(prod, TypePrompterEdit, PrompterEditPayload{Text: strPtr("hijack")})
	if errs := actionErrors(prod, TypePrompterEdit); len(errs) != 1 || errs[0].Code != ErrCodeForbidden {
		t.Fatalf("producer private edit errors %+v", errs)
	}
	if st := r.h.prompterSnapshot("room"); st.text != "# Intro\nSecret notes" || st.revision != 1 {
		t.Fatalf("rejected edits changed the script: %+v", st)
	}

	r.send(host, TypePrompterEdit, PrompterEditPayload{Visibility: PrompterCrew})
	if p := lastPrompter(t, prod); p.Hidden || p.Text != "# Intro\nSecret notes" {
		t.Fatalf("producer under crew %+v", p)
	}
	if p := lastPrompter(t, guest); !p.Hidden || p.Text != "" {
		t.Fatalf("guest under crew %+v", p)
	}
	// The producer edits under crew, but can't change visibility.
	r.send(prod, TypePrompterEdit, PrompterEditPayload{Text: strPtr("## Segment 2")})
	if p := lastPrompter(t, host); p.Text != "## Segment 2" || p.UpdatedBy != "Pat" {
		t.Fatalf("producer edit %+v", p)
	}
	r.send(prod, TypePrompterEdit, PrompterEditPayload{Visibility: PrompterEveryone})
	if errs := actionErrors(prod, TypePrompterEdit); len(errs) != 1 || errs[0].Code != ErrCodeForbidden {
		t.Fatalf("producer visibility errors %+v", errs)
	}
	r.send(host, TypePrompterEdit, PrompterEditPayload{Visibility: "public"})
	if errs := actionErrors(host, TypePrompterEdit); len(errs) != 1 || errs[0].Code != ErrCodeInvalid {
		t.Fatalf("unknown visibility %+v", errs)
	}

	r.send(host, TypePrompterEdit, PrompterEditPayload{Visibility: PrompterEveryone})
	if p := lastPrompter(t, guest); p.Hidden || p.Text != "## Segment 2" {
		t.Fatalf("guest under everyone %+v", p)
	}
	// Still no guest edits under everyone.
	r.send(guest, TypePrompterEdit, PrompterEditPayload{Text: strPtr("hijack")})
	if errs := actionErrors(guest, TypePrompterEdit); len(errs) != 1 {
		t.Fatalf("guest edit under everyone %+v", errs)
	}
	// A late joiner gets the current state on admission.
	late := r.join("g2", "Lee", "guest", true)
	if p := lastPrompter(t, late); p.Text != "## Segment 2" {
		t.Fatalf("late joiner %+v", p)
	}
	// Phones get nothing.
	phone := &Client{ParticipantID: "ph", SessionID: "room", Role: RoleSubsource, Status: "admitted", Send: make(chan []byte, 64)}
	r.h.Register(phone)
	r.h.sendProduction(phone)
	r.send(phone, TypePrompterEdit, PrompterEditPayload{Text: strPtr("x")})
	if envs := drainEnv(phone, TypePrompter, TypeBranding); len(envs) != 0 {
		t.Fatalf("phone got production state: %d", len(envs))
	}
}

func TestPrompterSizeCapAndNoLogging(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	r := newCollabRig(t)
	host := r.join("h", "Host", "host", true)
	drainEnv(host)
	big := strings.Repeat("a", PrompterMaxBytes+1)
	r.send(host, TypePrompterEdit, PrompterEditPayload{Text: &big})
	if errs := actionErrors(host, TypePrompterEdit); len(errs) != 1 || errs[0].Code != ErrCodeInvalid {
		t.Fatalf("oversized script accepted: %+v", errs)
	}
	exact := strings.Repeat("é", PrompterMaxBytes/2) // 2 bytes each: exactly the cap
	r.send(host, TypePrompterEdit, PrompterEditPayload{Text: &exact})
	if p := lastPrompter(t, host); len(p.Text) != PrompterMaxBytes {
		t.Fatalf("script at the cap rejected: %d", len(p.Text))
	}
	bad := "ok\xff"
	r.send(host, TypePrompterEdit, PrompterEditPayload{Text: &bad})
	marker := "TOPSECRET-SCRIPT-LINE"
	r.send(host, TypePrompterEdit, PrompterEditPayload{Text: strPtr("intro\r\n" + marker)})
	if p := lastPrompter(t, host); p.Text != "intro\n"+marker {
		t.Fatalf("CRLF not normalized: %q", p.Text)
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatal("script text reached the logs")
	}
}

func TestPrompterScrollHostOnlyAndRelayedToViewers(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("h", "Host", "host", true)
	prod := r.join("p", "Pat", RoleProducer, true)
	guest := r.join("g", "Gia", "guest", true)
	drainEnv(host)
	drainEnv(prod)
	drainEnv(guest)

	r.send(guest, TypePrompterScroll, PrompterScrollPayload{Position: 0.5, Shared: true})
	if errs := actionErrors(guest, TypePrompterScroll); len(errs) != 1 || errs[0].Code != ErrCodeForbidden {
		t.Fatalf("guest scroll %+v", errs)
	}
	r.send(prod, TypePrompterScroll, PrompterScrollPayload{Position: 0.5, Shared: true})
	if errs := actionErrors(prod, TypePrompterScroll); len(errs) != 1 {
		t.Fatalf("producer scroll %+v", errs)
	}

	// Visibility crew: the producer follows, the guest receives nothing.
	r.send(host, TypePrompterEdit, PrompterEditPayload{Visibility: PrompterCrew})
	drainEnv(prod)
	drainEnv(guest)
	raw := []byte(`{"t":"prompter_scroll","payload":{"position":7,"playing":true,"speed":99,"shared":true}}`)
	r.h.HandleMessage(host, raw)
	envs := drainEnv(prod, TypePrompterScroll)
	if len(envs) != 1 {
		t.Fatalf("producer got %d scrolls", len(envs))
	}
	var sc PrompterScrollPayload
	_ = json.Unmarshal(envs[0].Payload, &sc)
	if sc.Position != 1 || sc.Speed != PrompterMaxSpeed || !sc.Playing || !sc.Shared {
		t.Fatalf("scroll not clamped: %+v", sc)
	}
	if n := len(drainEnv(guest, TypePrompterScroll)); n != 0 {
		t.Fatalf("guest got %d scrolls for a crew script", n)
	}
	if n := len(drainEnv(host, TypePrompterScroll)); n != 0 {
		t.Fatal("scroll echoed to the host")
	}
	if p := lastPrompterAfterJoin(t, r); p.Scroll != nil {
		t.Fatalf("a guest joining a crew script got the scroll: %+v", p)
	}
	// The shared position reaches late viewers in their state.
	r.send(host, TypePrompterEdit, PrompterEditPayload{Visibility: PrompterEveryone})
	if p := lastPrompter(t, guest); p.Scroll == nil || p.Scroll.Position != 1 {
		t.Fatalf("guest state lacks the shared scroll: %+v", p)
	}
	if got := finite(math.NaN(), 0, 1); got != 0 {
		t.Fatalf("NaN position %v", got)
	}
	// Rate limit: a burst is cut at the limit.
	drainEnv(guest)
	for i := 0; i < PrompterScrollRateLimit+10; i++ {
		r.send(host, TypePrompterScroll, PrompterScrollPayload{Position: 0.1, Shared: true})
	}
	if n := len(drainEnv(guest, TypePrompterScroll)); n > PrompterScrollRateLimit {
		t.Fatalf("scroll burst not limited: %d", n)
	}
}

func lastPrompterAfterJoin(t *testing.T, r *collabRig) PrompterPayload {
	t.Helper()
	c := r.join("late-"+time.Now().Format("150405.000000000"), "Late", "guest", true)
	return lastPrompter(t, c)
}

// A room's script persists with the room and is loaded by a later meeting;
// an instant meeting's script is memory only.
func TestPrompterPersistsPerRoomOnly(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	if err := st.CreateRoom(&store.Room{ID: "r1", Name: "Show", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*store.Session{{ID: "ep1", RoomID: "r1", CreatedAt: now}, {ID: "ep2", RoomID: "r1", CreatedAt: now}, {ID: "adhoc", CreatedAt: now}} {
		if err := st.CreateSession(s); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHub(st, &config.Config{SecretKey: []byte("k")}, nil)
	defer h.Close()
	hostIn := func(sess string) *Client {
		c := &Client{ParticipantID: "host-" + sess, SessionID: sess, Role: RoleHost, Status: "admitted", Send: make(chan []byte, 64)}
		h.Register(c)
		h.handleHello(c, &HelloPayload{Name: "Host", Role: RoleHost})
		return c
	}
	edit := func(c *Client, text, vis string) {
		raw, _ := json.Marshal(map[string]any{"t": TypePrompterEdit, "payload": PrompterEditPayload{Text: &text, Visibility: vis}})
		h.HandleMessage(c, raw)
	}
	edit(hostIn("ep1"), "Room script", PrompterEveryone)
	edit(hostIn("adhoc"), "Ad-hoc script", "")
	if p, _ := st.GetProduction("room:r1"); p.Script != "Room script" || p.ScriptVisibility != PrompterEveryone {
		t.Fatalf("room script not saved: %+v", p)
	}
	if p, _ := st.GetProduction("session:adhoc"); p.Script != "" {
		t.Fatal("instant meeting script written to disk")
	}
	if p := lastPrompter(t, hostIn("ep2")); p.Text != "Room script" || p.Visibility != PrompterEveryone {
		t.Fatalf("next room meeting %+v", p)
	}
}

func TestLowerThirdsHostAndProducerOnly(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("h", "Host", "host", true)
	prod := r.join("p", "Pat", RoleProducer, true)
	guest := r.join("g", "Ana", "guest", true)
	drainEnv(host)
	drainEnv(prod)
	drainEnv(guest)

	r.send(guest, TypeLowerThird, LowerThirdPayload{ParticipantID: "g", Title: "CEO of Everything"})
	if errs := actionErrors(guest, TypeLowerThird); len(errs) != 1 || errs[0].Code != ErrCodeForbidden {
		t.Fatalf("guest lower third %+v", errs)
	}
	if lts, _ := r.st.ListLowerThirds("room"); len(lts) != 0 {
		t.Fatalf("guest wrote a lower third: %+v", lts)
	}
	r.send(prod, TypeLowerThird, LowerThirdPayload{ParticipantID: "g", Name: "Ana Ruiz", Title: "Founder,‮ X\n"})
	envs := drainEnv(guest, TypeBranding)
	if len(envs) != 1 {
		t.Fatalf("branding broadcasts %d", len(envs))
	}
	var b BrandingPayload
	_ = json.Unmarshal(envs[0].Payload, &b)
	if len(b.LowerThirds) != 1 || b.LowerThirds[0].Name != "Ana Ruiz" || b.LowerThirds[0].Title != "Founder, X" || b.Accent != DefaultBrandAccent || b.LowerThirdsMode != LowerThirdsSpeaker {
		t.Fatalf("branding %+v", b)
	}
	r.send(host, TypeLowerThird, LowerThirdPayload{ParticipantID: "nobody", Title: "x"})
	if errs := actionErrors(host, TypeLowerThird); len(errs) != 1 || errs[0].Code != ErrCodeNotFound {
		t.Fatalf("unknown participant %+v", errs)
	}
	// The display name is the default: setting it stores no override.
	r.send(host, TypeLowerThird, LowerThirdPayload{ParticipantID: "h", Name: "Host", Title: "Producer"})
	lts, _ := r.st.ListLowerThirds("room")
	if lts["h"].Name != "" || lts["h"].Title != "Producer" {
		t.Fatalf("host lower third %+v", lts["h"])
	}
}
