package signaling

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

type collabRig struct {
	t   *testing.T
	h   *Hub
	st  *store.Store
	now time.Time
}

func newCollabRig(t *testing.T) *collabRig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := &collabRig{t: t, st: st, now: time.Unix(1_793_000_000, 0)}
	r.h = NewHub(st, &config.Config{SecretKey: []byte("collab-secret")}, nil)
	r.h.now = func() time.Time { return r.now }
	t.Cleanup(r.h.Close)
	return r
}

// join registers a client and runs the real hello path; guests are admitted
// by the host unless pending is requested.
func (r *collabRig) join(id, name, role string, admit bool) *Client {
	r.t.Helper()
	c := &Client{ParticipantID: id, SessionID: "room", Role: role, Status: "pending", Send: make(chan []byte, 512)}
	if role == "host" {
		c.Status = "admitted"
	}
	r.h.Register(c)
	r.h.handleHello(c, &HelloPayload{Name: name, Role: role})
	if role != "host" && admit {
		r.h.AdmitParticipant("room", id)
	}
	return c
}

func (r *collabRig) send(c *Client, typ string, payload any) {
	raw, _ := json.Marshal(map[string]any{"t": typ, "payload": payload})
	r.h.HandleMessage(c, raw)
}

// drain returns all queued envelopes of the given types (all if none given).
func drainEnv(c *Client, types ...string) []Envelope {
	var out []Envelope
	for {
		select {
		case b := <-c.Send:
			var env Envelope
			_ = json.Unmarshal(b, &env)
			if len(types) == 0 {
				out = append(out, env)
				continue
			}
			for _, t := range types {
				if env.Type == t {
					out = append(out, env)
				}
			}
		default:
			return out
		}
	}
}

func decode[T any](t *testing.T, env Envelope) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(env.Payload, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func lastRoster(t *testing.T, c *Client) RosterPayload {
	t.Helper()
	envs := drainEnv(c, TypeRoster)
	if len(envs) == 0 {
		t.Fatalf("%s received no roster", c.ParticipantID)
	}
	return decode[RosterPayload](t, envs[len(envs)-1])
}

func expectActionError(t *testing.T, c *Client, action, code string) {
	t.Helper()
	for _, env := range drainEnv(c, TypeActionError) {
		e := decode[ActionErrorPayload](t, env)
		if e.Action == action && e.Code == code {
			return
		}
	}
	t.Fatalf("%s: expected %s action_error %s", c.ParticipantID, action, code)
}

func TestChatStampsIdentityAndReachesOnlyAdmittedSessionMembers(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Guest", "guest", true)
	waiting := r.join("p_wait", "Waiting", "guest", false)
	outsider := &Client{ParticipantID: "p_out", SessionID: "other", Status: "admitted", Send: make(chan []byte, 32)}
	r.h.Register(outsider)
	for _, c := range []*Client{host, guest, waiting, outsider} {
		drainEnv(c)
	}

	xss := `  <img src=x onerror=alert(1)> https://example.com  `
	raw, _ := json.Marshal(map[string]any{"t": TypeChat, "payload": map[string]any{"text": xss, "participantId": "p_host", "name": "Impostor", "id": "forged", "ts": 1}})
	r.h.HandleMessage(guest, raw)
	got := drainEnv(host, TypeChat)
	if len(got) != 1 {
		t.Fatalf("host got %d chat messages", len(got))
	}
	msg := decode[ChatMessage](t, got[0])
	if msg.ParticipantID != "p_guest" || msg.Name != "Guest" || msg.Role != "guest" || msg.ID == "forged" || msg.TS != r.now.UnixMilli() {
		t.Fatalf("identity not server-stamped: %+v", msg)
	}
	if msg.Text != strings.TrimSpace(xss) {
		t.Fatalf("text altered beyond trimming: %q", msg.Text)
	}
	if len(drainEnv(guest, TypeChat)) != 1 {
		t.Fatal("sender did not receive its server echo")
	}
	if len(drainEnv(waiting, TypeChat)) != 0 || len(drainEnv(outsider, TypeChat)) != 0 {
		t.Fatal("chat leaked to waiting room or another session")
	}
	// Waiting participants cannot post.
	r.send(waiting, TypeChat, map[string]any{"text": "let me in"})
	if len(drainEnv(host, TypeChat)) != 0 {
		t.Fatal("pending participant posted chat")
	}
}

func TestChatValidationAndRateLimit(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Guest", "guest", true)
	drainEnv(host)
	drainEnv(guest)

	for _, bad := range []string{"", "   \n\t ", strings.Repeat("a", ChatMaxRunes+1)} {
		r.send(guest, TypeChat, map[string]any{"text": bad})
		expectActionError(t, guest, TypeChat, ErrCodeInvalid)
	}
	if n := len(drainEnv(host, TypeChat)); n != 0 {
		t.Fatalf("invalid chat relayed %d times", n)
	}
	// Exactly the limit is fine (counted in characters, not bytes).
	r.send(guest, TypeChat, map[string]any{"text": strings.Repeat("é", ChatMaxRunes)})
	if n := len(drainEnv(host, TypeChat)); n != 1 {
		t.Fatalf("max-length chat relayed %d times", n)
	}

	r.now = r.now.Add(time.Minute)
	for i := 0; i < ChatRateLimit; i++ {
		r.send(guest, TypeChat, map[string]any{"text": "hi"})
	}
	r.send(guest, TypeChat, map[string]any{"text": "one too many"})
	expectActionError(t, guest, TypeChat, ErrCodeRateLimited)
	if n := len(drainEnv(host, TypeChat)); n != ChatRateLimit {
		t.Fatalf("relayed %d messages, want %d", n, ChatRateLimit)
	}
	// The limit is per participant: the host is unaffected.
	r.send(host, TypeChat, map[string]any{"text": "host still talks"})
	if n := len(drainEnv(guest, TypeChat)); n != 1 {
		t.Fatalf("guest saw %d messages", n)
	}
	drainEnv(host)
	r.now = r.now.Add(ChatRateWindow + time.Millisecond)
	r.send(guest, TypeChat, map[string]any{"text": "after the window"})
	if n := len(drainEnv(host, TypeChat)); n != 1 {
		t.Fatal("rate limit did not reset after the window")
	}
}

func TestChatHistoryIsBoundedAndReplayedOnAdmissionAndResume(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	for i := 0; i < ChatHistoryLimit+15; i++ {
		r.now = r.now.Add(2 * time.Second)
		r.send(host, TypeChat, map[string]any{"text": "line " + time.Duration(i).String()})
	}
	drainEnv(host)

	late := r.join("p_late", "Late", "guest", false)
	if len(drainEnv(late, TypeChatHistory)) != 0 {
		t.Fatal("waiting participant received chat history before admission")
	}
	r.h.AdmitParticipant("room", "p_late")
	envs := drainEnv(late)
	var adm AdmissionPayload
	var hist []ChatHistoryPayload
	for _, e := range envs {
		switch e.Type {
		case TypeAdmission:
			adm = decode[AdmissionPayload](t, e)
		case TypeChatHistory:
			hist = append(hist, decode[ChatHistoryPayload](t, e))
		}
	}
	if len(hist) != 1 || len(hist[0].Messages) != ChatHistoryLimit {
		t.Fatalf("history replay: %d payloads", len(hist))
	}
	if first := hist[0].Messages[0]; first.ID != "m16" {
		t.Fatalf("oldest retained message %s, want m16", first.ID)
	}

	// A reconnect with the signed grant replays history again.
	resumed := &Client{ParticipantID: "p_temp", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 512)}
	r.h.Register(resumed)
	r.h.handleHello(resumed, &HelloPayload{Name: "Late", Role: "guest", ResumeToken: adm.UploadToken})
	if resumed.ParticipantID != "p_late" || resumed.getStatus() != "admitted" {
		t.Fatalf("resume failed: %s %s", resumed.ParticipantID, resumed.getStatus())
	}
	if len(drainEnv(resumed, TypeChatHistory)) != 1 {
		t.Fatal("resumed participant did not get chat history")
	}
}

func TestHandQueueOrderAndHostOnlyLowering(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Alice", "guest", true)
	b := r.join("p_b", "Bob", "guest", true)
	drainEnv(host)
	drainEnv(a)
	drainEnv(b)

	r.send(b, TypeHand, map[string]any{"raised": true})
	r.send(a, TypeHand, map[string]any{"raised": true}) // same millisecond: still ordered
	ro := lastRoster(t, host)
	at := map[string]int64{}
	for _, p := range ro.Participants {
		if p.HandRaised {
			at[p.ParticipantID] = p.HandRaisedAt
		}
	}
	if len(at) != 2 || at["p_b"] >= at["p_a"] {
		t.Fatalf("hand queue order wrong: %+v", at)
	}
	drainEnv(a)

	// A guest cannot lower (or raise) someone else's hand.
	r.send(a, TypeHand, map[string]any{"participantId": "p_b", "raised": false})
	expectActionError(t, a, TypeHand, ErrCodeForbidden)
	if len(drainEnv(host, TypeRoster)) != 0 {
		t.Fatal("guest changed someone else's hand")
	}
	// The host can lower it but not raise it.
	r.send(host, TypeHand, map[string]any{"participantId": "p_b", "raised": true})
	expectActionError(t, host, TypeHand, ErrCodeInvalid)
	r.send(host, TypeHand, map[string]any{"participantId": "p_b", "raised": false})
	for _, p := range lastRoster(t, b).Participants {
		if p.ParticipantID == "p_b" && p.HandRaised {
			t.Fatal("host could not lower a hand")
		}
		if p.ParticipantID == "p_a" && !p.HandRaised {
			t.Fatal("lowering one hand lowered another")
		}
	}
}

func TestReactionsAllowListAndRateLimit(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Guest", "guest", true)
	drainEnv(host)
	drainEnv(guest)

	r.send(guest, TypeReaction, map[string]any{"emoji": "🎉", "participantId": "p_host", "name": "Impostor"})
	got := drainEnv(host, TypeReaction)
	if len(got) != 1 {
		t.Fatal("reaction not relayed")
	}
	if p := decode[ReactionPayload](t, got[0]); p.ParticipantID != "p_guest" || p.Name != "Guest" || p.Emoji != "🎉" {
		t.Fatalf("reaction not stamped: %+v", p)
	}
	r.send(guest, TypeReaction, map[string]any{"emoji": "<script>"})
	expectActionError(t, guest, TypeReaction, ErrCodeInvalid)
	r.now = r.now.Add(time.Minute)
	for i := 0; i < ReactionRateLimit+3; i++ {
		r.send(guest, TypeReaction, map[string]any{"emoji": "👍"})
	}
	if n := len(drainEnv(host, TypeReaction)); n != ReactionRateLimit {
		t.Fatalf("relayed %d reactions, want %d", n, ReactionRateLimit)
	}
	expectActionError(t, guest, TypeReaction, ErrCodeRateLimited)
}

func TestGuestCannotMuteRemoveOrLock(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Alice", "guest", true)
	b := r.join("p_b", "Bob", "guest", true)
	drainEnv(host)
	drainEnv(a)
	drainEnv(b)

	r.send(a, TypeMuteRequest, map[string]any{"participantId": "p_b"})
	expectActionError(t, a, TypeMuteRequest, ErrCodeForbidden)
	r.send(a, TypeRemove, map[string]any{"participantId": "p_b"})
	expectActionError(t, a, TypeRemove, ErrCodeForbidden)
	r.send(a, TypeRemove, map[string]any{"participantId": "p_host"})
	expectActionError(t, a, TypeRemove, ErrCodeForbidden)
	r.send(a, TypeRoomLock, map[string]any{"locked": true})
	expectActionError(t, a, TypeRoomLock, ErrCodeForbidden)
	// The legacy deny path is host-only too.
	r.send(a, TypeDeny, map[string]any{"participantId": "p_b"})

	if len(drainEnv(b, TypeMuteRequest, TypeAdmission)) != 0 {
		t.Fatal("guest action reached its target")
	}
	if b.getStatus() != "admitted" || !r.h.ParticipantMayRelay("p_b") {
		t.Fatal("guest removed another guest")
	}
	if r.h.IsRoomLocked("room") {
		t.Fatal("guest locked the room")
	}
	// A waiting participant is no more privileged.
	w := r.join("p_wait", "Waiting", "guest", false)
	drainEnv(w)
	r.send(w, TypeRoomLock, map[string]any{"locked": true})
	expectActionError(t, w, TypeRoomLock, ErrCodeForbidden)
}

func TestHostMuteRequestReachesOnlyTarget(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Alice", "guest", true)
	b := r.join("p_b", "Bob", "guest", true)
	drainEnv(host)
	drainEnv(a)
	drainEnv(b)
	r.send(host, TypeMuteRequest, map[string]any{"participantId": "p_a"})
	got := drainEnv(a, TypeMuteRequest)
	if len(got) != 1 {
		t.Fatal("target did not receive mute request")
	}
	if p := decode[MuteRequestPayload](t, got[0]); p.ParticipantID != "p_a" || p.By != "Host" {
		t.Fatalf("mute request %+v", p)
	}
	if len(drainEnv(b, TypeMuteRequest)) != 0 {
		t.Fatal("mute request reached a bystander")
	}
	r.send(host, TypeMuteRequest, map[string]any{"participantId": "p_gone"})
	expectActionError(t, host, TypeMuteRequest, ErrCodeNotFound)
}

func TestHostRemoveRevokesGrantsAndUpdatesRoster(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Alice", "guest", true)
	b := r.join("p_b", "Bob", "guest", true)
	var grant string
	for _, e := range drainEnv(a, TypeAdmission) {
		grant = decode[AdmissionPayload](t, e).UploadToken
	}
	r.send(a, TypeHand, map[string]any{"raised": true})
	drainEnv(host)
	drainEnv(b)

	r.send(host, TypeRemove, map[string]any{"participantId": "p_a"})
	adms := drainEnv(a, TypeAdmission)
	if len(adms) != 1 {
		t.Fatal("removed participant was not told")
	}
	if adm := decode[AdmissionPayload](t, adms[0]); adm.Status != "denied" || adm.Reason != ReasonRemoved || adm.UploadToken != "" {
		t.Fatalf("removal admission %+v", adm)
	}
	if p, _ := r.st.GetParticipant("p_a"); p == nil || p.Status != "denied" {
		t.Fatalf("store not revoked: %+v", p)
	}
	if r.h.ParticipantMayRelay("p_a") {
		t.Fatal("removed participant keeps TURN relay access")
	}
	for _, p := range lastRoster(t, b).Participants {
		if p.ParticipantID == "p_a" {
			t.Fatal("removed participant still in roster")
		}
	}
	// Removed participants can't chat or resume.
	r.send(a, TypeChat, map[string]any{"text": "still here?"})
	if len(drainEnv(b, TypeChat)) != 0 {
		t.Fatal("removed participant still chats")
	}
	again := &Client{ParticipantID: "p_tmp", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 64)}
	r.h.Register(again)
	r.h.handleHello(again, &HelloPayload{Name: "Alice", Role: "guest", ResumeToken: grant})
	if again.getStatus() != "denied" {
		t.Fatal("removed participant resumed with its old grant")
	}
	// The host cannot be removed.
	r.send(host, TypeRemove, map[string]any{"participantId": "p_host"})
	expectActionError(t, host, TypeRemove, ErrCodeNotFound)
}

func TestRoomLockRefusesNewJoinersButAllowsResume(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Alice", "guest", true)
	var grant string
	for _, e := range drainEnv(a, TypeAdmission) {
		grant = decode[AdmissionPayload](t, e).UploadToken
	}
	waiting := r.join("p_wait", "Waiting", "guest", false)
	drainEnv(host)
	drainEnv(waiting)

	r.send(host, TypeRoomLock, map[string]any{"locked": true})
	if !lastRoster(t, host).Locked {
		t.Fatal("roster does not show the lock")
	}
	states := drainEnv(waiting, TypeRoomState)
	if len(states) != 1 || !decode[RoomStatePayload](t, states[0]).Locked {
		t.Fatal("waiting room not told about the lock")
	}

	late := r.join("p_late", "Late", "guest", false)
	adms := drainEnv(late, TypeAdmission)
	if len(adms) != 1 {
		t.Fatalf("late joiner admissions: %d", len(adms))
	}
	if adm := decode[AdmissionPayload](t, adms[0]); adm.Status != "denied" || adm.Reason != ReasonRoomLocked || len(adm.IceServers) != 0 {
		t.Fatalf("locked room admission %+v", adm)
	}
	if len(drainEnv(host, TypeJoinRequest)) != 0 {
		t.Fatal("locked room still forwards join requests")
	}

	// An admitted participant reconnecting with its grant gets back in.
	back := &Client{ParticipantID: "p_tmp", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 64)}
	r.h.Register(back)
	r.h.handleHello(back, &HelloPayload{Name: "Alice", Role: "guest", ResumeToken: grant})
	if back.getStatus() != "admitted" || back.ParticipantID != "p_a" {
		t.Fatalf("resume refused while locked: %s", back.getStatus())
	}
	// The host is never locked out.
	host2 := r.join("p_host2", "Host", "host", true)
	if host2.getStatus() != "admitted" {
		t.Fatal("host locked out")
	}

	r.send(host, TypeRoomLock, map[string]any{"locked": false})
	later := r.join("p_later", "Later", "guest", false)
	if later.getStatus() != "pending" {
		t.Fatalf("unlocked room status %s", later.getStatus())
	}
}

func TestRoomStateTellsWaitingRoomAboutRecording(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	waiting := r.join("p_wait", "Waiting", "guest", false)
	initial := drainEnv(waiting, TypeRoomState)
	if len(initial) != 1 || decode[RoomStatePayload](t, initial[0]).Recording {
		t.Fatal("waiting room missing initial room state")
	}
	drainEnv(host)
	r.send(host, TypeRecordStart, nil)
	st := drainEnv(waiting, TypeRoomState)
	if len(st) != 1 {
		t.Fatal("waiting room not told recording started")
	}
	if p := decode[RoomStatePayload](t, st[0]); !p.Recording || p.TakeNumber != 1 {
		t.Fatalf("room state %+v", p)
	}
	// A guest who arrives mid-take learns before admission.
	late := r.join("p_late", "Late", "guest", false)
	if p := decode[RoomStatePayload](t, drainEnv(late, TypeRoomState)[0]); !p.Recording {
		t.Fatal("mid-take joiner not told about recording")
	}
	r.send(host, TypeRecordStop, nil)
	if p := decode[RoomStatePayload](t, drainEnv(waiting, TypeRoomState)[0]); p.Recording {
		t.Fatal("recording stop not announced")
	}
}

func TestClampName(t *testing.T) {
	if got := clampName("  " + strings.Repeat("x", MaxNameRunes+10) + " "); len([]rune(got)) != MaxNameRunes {
		t.Fatalf("name not clamped: %d", len(got))
	}
	if got := clampName("Ana\xff"); got != "Ana" {
		t.Fatalf("invalid UTF-8 kept: %q", got)
	}
}
