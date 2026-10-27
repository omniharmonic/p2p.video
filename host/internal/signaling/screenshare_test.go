package signaling

import (
	"reflect"
	"sync"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/sfu"
)

// fakeRTC records the hub's media-plane calls.
type fakeRTC struct {
	mu       sync.Mutex
	screens  map[string][]string // participant -> declared screen track IDs
	offers   []string            // "<participant>:<sdp>" full (re)placement offers
	renegs   []string            // "<participant>:<sdp>" renegotiations
	left     []string
	renegErr error
}

func newFakeRTC() *fakeRTC { return &fakeRTC{screens: map[string][]string{}} }

func (f *fakeRTC) HandleOffer(_, p, sdp string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offers = append(f.offers, p+":"+sdp)
	return "answer-" + sdp, nil
}
func (f *fakeRTC) HandleRenegotiation(_, p, sdp string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renegErr != nil {
		return "", f.renegErr
	}
	f.renegs = append(f.renegs, p+":"+sdp)
	return "reneg-answer-" + sdp, nil
}
func (f *fakeRTC) HandleCandidate(_, _, _ string, _ *string, _ *uint16) error { return nil }
func (f *fakeRTC) HandleAnswer(_, _, _ string) error                          { return nil }
func (f *fakeRTC) SetOnOffer(func(string, string, string))                    {}
func (f *fakeRTC) SetOnCandidate(func(string, string, string, *string, *uint16)) {
}
func (f *fakeRTC) OnParticipantLeave(_, p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.left = append(f.left, p)
	delete(f.screens, p)
}
func (f *fakeRTC) SetScreenTracks(_, p string, ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(ids) == 0 {
		delete(f.screens, p)
		return
	}
	f.screens[p] = append([]string(nil), ids...)
}
func (f *fakeRTC) declared(p string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screens[p]
}

func newScreenRig(t *testing.T) (*collabRig, *fakeRTC) {
	r := newCollabRig(t)
	f := newFakeRTC()
	r.h.WebRTC = f
	return r, f
}

func presenter(t *testing.T, roster RosterPayload) string {
	t.Helper()
	who := ""
	for _, p := range roster.Participants {
		if p.ScreenSharing {
			if who != "" {
				t.Fatalf("two presenters in roster: %s and %s", who, p.ParticipantID)
			}
			who = p.ParticipantID
		}
	}
	return who
}

func screenAdd(ids ...string) map[string]any {
	return map[string]any{"sourceId": "screen", "kind": "screen", "label": "Screen", "trackIds": ids}
}

func TestScreenShareDeclaresTracksAndShowsPresenterInRoster(t *testing.T) {
	r, f := newScreenRig(t)
	host := r.join("p_host", "Host", "host", true)
	guest := r.join("p_guest", "Guest", "guest", true)
	drainEnv(host)
	drainEnv(guest)
	r.send(guest, TypeSourceAdd, screenAdd("v1", "a1"))
	if got := f.declared("p_guest"); !reflect.DeepEqual(got, []string{"v1", "a1"}) {
		t.Fatalf("declared screen tracks = %v", got)
	}
	if who := presenter(t, lastRoster(t, host)); who != "p_guest" {
		t.Fatalf("host roster presenter = %q", who)
	}
	if who := presenter(t, lastRoster(t, guest)); who != "p_guest" {
		t.Fatalf("sharer roster presenter = %q", who)
	}
	// The sharer stops: roster clears and the SFU declaration is withdrawn.
	r.send(guest, TypeSourceRemove, map[string]any{"sourceId": "screen", "kind": "screen"})
	if who := presenter(t, lastRoster(t, host)); who != "" {
		t.Fatalf("presenter after source_remove = %q", who)
	}
	if f.declared("p_guest") != nil {
		t.Fatal("SFU declaration retained after source_remove")
	}
}

func TestOnlyOneScreenShareAtATime(t *testing.T) {
	r, f := newScreenRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Guest A", "guest", true)
	b := r.join("p_b", "Guest B", "guest", true)
	r.send(a, TypeSourceAdd, screenAdd("va"))
	drainEnv(host)
	drainEnv(b)
	r.send(b, TypeSourceAdd, screenAdd("vb"))
	expectActionError(t, b, TypeSourceAdd, ErrCodeConflict)
	if f.declared("p_b") != nil {
		t.Fatal("second presenter's tracks were declared")
	}
	if len(drainEnv(host, TypeRoster)) != 0 {
		t.Fatal("rejected share changed the roster")
	}
	// Re-declaring by the current presenter (e.g. a new window) is allowed.
	r.send(a, TypeSourceAdd, screenAdd("va2"))
	if got := f.declared("p_a"); !reflect.DeepEqual(got, []string{"va2"}) {
		t.Fatalf("presenter re-declare = %v", got)
	}
	// Someone else's source_remove cannot end it.
	r.send(b, TypeSourceRemove, map[string]any{"sourceId": "screen", "kind": "screen"})
	if r.h.ScreenOwner("room") != "p_a" {
		t.Fatal("another participant ended the share")
	}
}

func TestScreenShareRejectsInvalidDeclarations(t *testing.T) {
	r, f := newScreenRig(t)
	guest := r.join("p_guest", "Guest", "guest", true)
	waiting := r.join("p_wait", "Waiting", "guest", false)
	long := make([]byte, MaxTrackIDRunes+1)
	for i := range long {
		long[i] = 'x'
	}
	for _, payload := range []map[string]any{
		{"sourceId": "cam", "kind": "camera", "trackIds": []string{"v"}},
		screenAdd(),
		screenAdd("a", "b", "c"),
		screenAdd("dup", "dup"),
		screenAdd(""),
		screenAdd(string(long)),
	} {
		drainEnv(guest)
		r.send(guest, TypeSourceAdd, payload)
		expectActionError(t, guest, TypeSourceAdd, ErrCodeInvalid)
	}
	r.send(waiting, TypeSourceAdd, screenAdd("v"))
	if f.declared("p_guest") != nil || f.declared("p_wait") != nil || r.h.ScreenOwner("room") != "" {
		t.Fatal("invalid or unadmitted declaration accepted")
	}
}

func TestHostCanStopSomeonesShareAndGuestsCannot(t *testing.T) {
	r, f := newScreenRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Guest A", "guest", true)
	b := r.join("p_b", "Guest B", "guest", true)
	r.send(a, TypeSourceAdd, screenAdd("va"))
	drainEnv(a)

	// Negative: a guest cannot stop someone else's share.
	drainEnv(b)
	r.send(b, TypeScreenStop, map[string]any{"participantId": "p_a"})
	expectActionError(t, b, TypeScreenStop, ErrCodeForbidden)
	if r.h.ScreenOwner("room") != "p_a" || f.declared("p_a") == nil {
		t.Fatal("guest stopped another participant's share")
	}
	if len(drainEnv(a, TypeScreenStop)) != 0 {
		t.Fatal("guest's stop request reached the sharer")
	}
	// Negative: the sharer can't forge a host stop either (it is not a host).
	r.send(a, TypeScreenStop, map[string]any{"participantId": "p_a"})
	expectActionError(t, a, TypeScreenStop, ErrCodeForbidden)

	// Host stops it: SFU unpublishes first, the sharer is told, roster clears.
	drainEnv(host)
	r.send(host, TypeScreenStop, map[string]any{"participantId": "p_a"})
	if f.declared("p_a") != nil || r.h.ScreenOwner("room") != "" {
		t.Fatal("host stop did not unpublish the share")
	}
	stops := drainEnv(a, TypeScreenStop)
	if len(stops) != 1 {
		t.Fatalf("sharer got %d screen_stop messages", len(stops))
	}
	if got := decode[ScreenStopPayload](t, stops[0]); got.ParticipantID != "p_a" || got.By != "Host" {
		t.Fatalf("screen_stop payload = %+v", got)
	}
	if presenter(t, lastRoster(t, b)) != "" {
		t.Fatal("roster still shows a presenter")
	}
	// Stopping when nobody presents is reported, not silently accepted.
	r.send(host, TypeScreenStop, map[string]any{"participantId": "p_a"})
	expectActionError(t, host, TypeScreenStop, ErrCodeNotFound)
	// Once stopped, someone else may present.
	r.send(b, TypeSourceAdd, screenAdd("vb"))
	if r.h.ScreenOwner("room") != "p_b" {
		t.Fatal("next presenter rejected after host stop")
	}
}

func TestScreenShareEndsWhenPresenterLeavesOrIsRemoved(t *testing.T) {
	r, f := newScreenRig(t)
	host := r.join("p_host", "Host", "host", true)
	a := r.join("p_a", "Guest A", "guest", true)
	b := r.join("p_b", "Guest B", "guest", true)
	r.send(a, TypeSourceAdd, screenAdd("va"))
	r.h.Unregister(a)
	if r.h.ScreenOwner("room") != "" || f.declared("p_a") != nil {
		t.Fatal("share outlived its presenter's connection")
	}
	r.send(b, TypeSourceAdd, screenAdd("vb"))
	drainEnv(host)
	r.send(host, TypeRemove, map[string]any{"participantId": "p_b"})
	if r.h.ScreenOwner("room") != "" || f.declared("p_b") != nil {
		t.Fatal("share outlived its removed presenter")
	}
	if presenter(t, lastRoster(t, host)) != "" {
		t.Fatal("roster still shows the removed presenter")
	}
}

func TestRenegotiationOfferRoutedWithoutReplacingPeer(t *testing.T) {
	r, f := newScreenRig(t)
	guest := r.join("p_guest", "Guest", "guest", true)
	drainEnv(guest)
	r.send(guest, TypeSdpOffer, map[string]any{"sdp": "with-screen", "type": "offer", "renegotiate": true})
	f.mu.Lock()
	renegs, offers := append([]string(nil), f.renegs...), append([]string(nil), f.offers...)
	f.mu.Unlock()
	if !reflect.DeepEqual(renegs, []string{"p_guest:with-screen"}) || len(offers) != 0 {
		t.Fatalf("renegotiation routed as replacement: renegs=%v offers=%v", renegs, offers)
	}
	answers := drainEnv(guest, TypeSdpAnswer)
	if len(answers) != 1 {
		t.Fatalf("got %d answers", len(answers))
	}
	if a := decode[SdpPayload](t, answers[0]); a.SDP != "reneg-answer-with-screen" || !a.Renegotiate {
		t.Fatalf("answer = %+v", a)
	}
	// Glare: no answer is sent; the client rolls back on the server's offer.
	f.renegErr = sfu.ErrGlare
	r.send(guest, TypeSdpOffer, map[string]any{"sdp": "x", "type": "offer", "renegotiate": true})
	if len(drainEnv(guest, TypeSdpAnswer)) != 0 {
		t.Fatal("answer sent for a glared renegotiation")
	}
	// A plain offer still replaces the peer (media recovery).
	r.send(guest, TypeSdpOffer, map[string]any{"sdp": "fresh", "type": "offer"})
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.offers, []string{"p_guest:fresh"}) {
		t.Fatalf("recovery offer not routed to HandleOffer: %v", f.offers)
	}
}

func TestScreenStreamSuffixMatchesSFU(t *testing.T) {
	if ScreenStreamSuffix != sfu.ScreenStreamSuffix {
		t.Fatalf("signaling %q != sfu %q", ScreenStreamSuffix, sfu.ScreenStreamSuffix)
	}
	var _ WebRTCHandler = (*sfu.SFU)(nil)
}
