package signaling

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// publishOnlyRTC is a fakeRTC that also records SetPublishOnly.
type publishOnlyRTC struct {
	*fakeRTC
	mu   sync.Mutex
	only map[string]bool
}

func (f *publishOnlyRTC) SetPublishOnly(_, p string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.only[p] = on
}

// joinPhone runs the WebSocket handler's job (capability → parent) and the
// real hello path for a phone.
func (r *collabRig) joinPhone(id, parent, claimedName string) *Client {
	r.t.Helper()
	c := &Client{ParticipantID: id, SessionID: "room", Role: RoleSubsource, ParentParticipantID: parent, Status: "pending", Send: make(chan []byte, 512)}
	r.h.Register(c)
	r.h.handleHello(c, &HelloPayload{Name: claimedName, Role: RoleSubsource})
	return c
}

func grantOf(t *testing.T, c *Client) string {
	t.Helper()
	for _, env := range drainEnv(c, TypeAdmission) {
		if a := decode[AdmissionPayload](t, env); a.Status == "admitted" {
			return a.UploadToken
		}
	}
	t.Fatalf("%s was not admitted", c.ParticipantID)
	return ""
}

func TestSubsourceCapabilityMintVerifyExpiry(t *testing.T) {
	secret := []byte("s")
	now := time.Unix(1_800_000_000, 0)
	tok, exp, err := MintSubsourceCap("room", "p_guest", SubsourceCapTTL, secret, now)
	if err != nil || !exp.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("mint: %v %v", exp, err)
	}
	c, err := VerifySubsourceCap(tok, secret, now.Add(9*time.Minute))
	if err != nil || c.SessionID != "room" || c.ParentID != "p_guest" || len(c.Nonce) != 32 {
		t.Fatalf("verify: %+v %v", c, err)
	}
	if _, err := VerifySubsourceCap(tok, secret, now.Add(10*time.Minute+time.Second)); err == nil {
		t.Fatal("expired capability verified")
	}
	if _, err := VerifySubsourceCap(tok, []byte("other"), now); err == nil {
		t.Fatal("capability verified with another secret")
	}
	body, sig, _ := strings.Cut(tok, ".")
	if _, err := VerifySubsourceCap(body+"x."+sig, secret, now); err == nil {
		t.Fatal("tampered capability verified")
	}
	// Domain separation: a capability is neither an upload nor a host token,
	// and an upload token is not a capability.
	if _, err := VerifyUploadToken(tok, secret); err == nil {
		t.Fatal("capability verified as an upload grant")
	}
	if VerifyHostToken(tok, "room", secret) {
		t.Fatal("capability verified as a host token")
	}
	if _, err := VerifySubsourceCap(MintUploadToken("p_guest", "room", time.Hour, secret), secret, time.Now()); err == nil {
		t.Fatal("upload grant verified as a capability")
	}
	if _, _, err := MintSubsourceCap("ro:om", "p", time.Minute, secret, now); err == nil {
		t.Fatal("separator in a field accepted")
	}
}

func TestSubsourceCapabilityOnlyForAdmittedParticipantAndSingleUse(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Ada", "guest", true)
	waiting := r.join("p_wait", "Wait", "guest", false)
	guestGrant := grantOf(t, guest)
	_ = grantOf(t, host)

	// Not admitted, wrong session, garbage: refused.
	if _, _, err := r.h.MintSubsourceCapability("room", MintUploadToken(waiting.ParticipantID, "room", time.Hour, r.h.Config.SecretKey)); err == nil {
		t.Fatal("a waiting guest minted a phone link")
	}
	if _, _, err := r.h.MintSubsourceCapability("other", guestGrant); err == nil {
		t.Fatal("grant for another session minted a phone link")
	}
	if _, _, err := r.h.MintSubsourceCapability("room", "nope"); err == nil {
		t.Fatal("garbage minted a phone link")
	}
	tok, _, err := r.h.MintSubsourceCapability("room", guestGrant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.ConsumeSubsourceCap("other", tok); err == nil {
		t.Fatal("capability consumed for another session")
	}
	parent, err := r.h.ConsumeSubsourceCap("room", tok)
	if err != nil || parent != "p_guest" {
		t.Fatalf("consume: %q %v", parent, err)
	}
	if _, err := r.h.ConsumeSubsourceCap("room", tok); !errors.Is(err, ErrSubsourceCapUsed) {
		t.Fatalf("second use: %v", err)
	}
	// Expired: the hub clock moves past the TTL.
	tok2, _, _ := r.h.MintSubsourceCapability("room", guestGrant)
	r.now = r.now.Add(SubsourceCapTTL + time.Second)
	if _, err := r.h.ConsumeSubsourceCap("room", tok2); err == nil {
		t.Fatal("expired capability consumed")
	}
}

func TestSubsourceCapabilityRefusedOnceParentLeft(t *testing.T) {
	r := newCollabRig(t)
	r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Ada", "guest", true)
	tok, _, err := r.h.MintSubsourceCapability("room", grantOf(t, guest))
	if err != nil {
		t.Fatal(err)
	}
	r.h.Unregister(guest)
	if _, err := r.h.ConsumeSubsourceCap("room", tok); !errors.Is(err, ErrSubsourceParent) {
		t.Fatalf("parent gone: %v", err)
	}
}

func TestSubsourceAdmittedWithoutPromptNamedAfterParentAndPublishOnly(t *testing.T) {
	r := newCollabRig(t)
	f := &publishOnlyRTC{fakeRTC: newFakeRTC(), only: map[string]bool{}}
	r.h.WebRTC = f
	host := r.join("p_host", "Host", "host", true)
	r.join("p_guest", "Ada", "guest", true)
	// A locked room does not stop a phone minted for an admitted participant.
	r.send(host, TypeRoomLock, map[string]any{"locked": true})
	drainEnv(host)
	phone := r.joinPhone("p_phone", "p_guest", "‮evil")
	var adm AdmissionPayload
	for _, env := range drainEnv(phone, TypeAdmission) {
		adm = decode[AdmissionPayload](t, env)
	}
	if adm.Status != "admitted" || adm.Role != RoleSubsource || adm.ParentParticipantID != "p_guest" || adm.Name != "Ada · Phone" || adm.UploadToken == "" {
		t.Fatalf("phone admission: %+v", adm)
	}
	var roster RosterPayload
	for _, env := range drainEnv(host) {
		switch env.Type {
		case TypeJoinRequest:
			t.Fatal("the host was asked to admit the phone")
		case TypeRoster:
			roster = decode[RosterPayload](t, env)
		}
	}
	if !f.only["p_phone"] {
		t.Fatal("phone not marked publish-only in the SFU")
	}
	var entry *RosterEntry
	for i := range roster.Participants {
		if roster.Participants[i].ParticipantID == "p_phone" {
			entry = &roster.Participants[i]
		}
	}
	if entry == nil || entry.ParentParticipantID != "p_guest" || entry.Role != RoleSubsource || entry.Name != "Ada · Phone" {
		t.Fatalf("roster entry: %+v", entry)
	}
	part, err := r.st.GetParticipant("p_phone")
	if err != nil || part.Role != RoleSubsource || part.ParentParticipantID != "p_guest" {
		t.Fatalf("stored phone: %+v %v", part, err)
	}
}

func TestSubsourceMessagesAreLimitedServerSide(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	r.join("p_guest", "Ada", "guest", true)
	phone := r.joinPhone("p_phone", "p_guest", "")
	drainEnv(phone)
	drainEnv(host)
	for typ, payload := range map[string]any{
		TypeChat:        map[string]any{"text": "hi"},
		TypeHand:        map[string]any{"raised": true},
		TypeReaction:    map[string]any{"emoji": "👍"},
		TypeSourceAdd:   screenAdd("v"),
		TypeRecordStart: map[string]any{},
		TypeRoomLock:    map[string]any{"locked": true},
		TypeRemove:      map[string]any{"participantId": "p_guest"},
	} {
		r.send(phone, typ, payload)
		expectActionError(t, phone, typ, ErrCodeForbidden)
	}
	if len(drainEnv(host, TypeChat, TypeReaction)) != 0 || r.h.IsRecording("room") || r.h.IsRoomLocked("room") {
		t.Fatal("a phone's refused message had an effect")
	}
}

func TestSubsourceNeverExpectedInTakeAndRemovedWithParent(t *testing.T) {
	r := newCollabRig(t)
	host := r.join("p_host", "Host", "host", true)
	r.join("p_guest", "Ada", "guest", true)
	phone := r.joinPhone("p_phone", "p_guest", "")
	r.send(host, TypeRecordStart, map[string]any{})
	tk := r.h.activeTake["room"]
	if tk == nil {
		t.Fatal("no take")
	}
	missing, err := r.st.ListMissingTakeParticipants(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range missing {
		if p.ID == "p_phone" {
			t.Fatal("the phone is an expected recorder of the take")
		}
	}
	if len(missing) != 2 {
		t.Fatalf("expected host and guest, got %+v", missing)
	}
	drainEnv(phone)
	// Removing the participant removes its phone too.
	r.send(host, TypeRemove, map[string]any{"participantId": "p_guest"})
	if phone.getStatus() != "denied" {
		t.Fatal("phone stayed in the call after its participant was removed")
	}
	if part, _ := r.st.GetParticipant("p_phone"); part == nil || part.Status != "denied" {
		t.Fatalf("phone not revoked: %+v", part)
	}
	// A removed phone can't come back with its grant.
	grant := MintUploadToken("p_phone", "room", time.Hour, r.h.Config.SecretKey)
	if _, ok := r.h.SubsourceResume("room", grant); ok {
		t.Fatal("removed phone resumed")
	}
}

func TestSubsourceResumeOnlyForPhones(t *testing.T) {
	r := newCollabRig(t)
	r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Ada", "guest", true)
	phone := r.joinPhone("p_phone", "p_guest", "")
	if parent, ok := r.h.SubsourceResume("room", grantOf(t, phone)); !ok || parent != "p_guest" {
		t.Fatalf("phone grant resume: %q %v", parent, ok)
	}
	if _, ok := r.h.SubsourceResume("room", grantOf(t, guest)); ok {
		t.Fatal("a guest grant authorized a phone socket")
	}
	if _, ok := r.h.SubsourceResume("other", MintUploadToken("p_phone", "other", time.Hour, r.h.Config.SecretKey)); ok {
		t.Fatal("phone grant crossed sessions")
	}
}

func TestSubsourceLimitPerParticipant(t *testing.T) {
	r := newCollabRig(t)
	r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Ada", "guest", true)
	grant := grantOf(t, guest)
	for i := 0; i < MaxSubsourcesPerParticipant; i++ {
		r.joinPhone("p_phone"+string(rune('a'+i)), "p_guest", "")
	}
	if _, _, err := r.h.MintSubsourceCapability("room", grant); !errors.Is(err, ErrSubsourceLimit) {
		t.Fatalf("limit: %v", err)
	}
}
