package httpserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Replacing or turning off a room's guest link bumps the SEQUENCE of its
// upcoming invites (the event's location changed), so a re-sent invite or
// the feed replaces the old event; other rooms and cancelled meetings are
// left alone [SL-P5-4].
func TestRoomLinkChangeBumpsInviteSequence(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	other := h.createRoom("Other Show")
	_, m, raw := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, startIn(48*time.Hour)))
	_, gone, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, startIn(72*time.Hour)))
	_, o, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, other.ID, startIn(48*time.Hour)))
	if m.ID == "" || gone.ID == "" || o.ID == "" {
		t.Fatalf("schedule: %s", raw)
	}
	if code, _, raw := h.meetingCall("POST", "/api/v1/schedule/"+gone.ID+"/cancel", ""); code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, raw)
	}
	seq := func(id string) int {
		t.Helper()
		got, err := h.st.GetScheduledMeeting(id)
		if err != nil {
			t.Fatal(err)
		}
		return got.Sequence
	}
	cancelledSeq := seq(gone.ID)

	if code, _ := h.do("POST", "/api/v1/rooms/"+room.ID+"/rotate", "", nil); code != http.StatusOK {
		t.Fatalf("rotate: %d", code)
	}
	if seq(m.ID) != 1 || seq(o.ID) != 0 || seq(gone.ID) != cancelledSeq {
		t.Fatalf("after rotate: %d %d %d", seq(m.ID), seq(o.ID), seq(gone.ID))
	}
	// The invite now carries the new link with the higher SEQUENCE.
	code, ics := h.do("GET", "/api/v1/schedule/"+m.ID+"/invite.ics", "", nil)
	lines := unfoldICS(string(ics))
	if code != http.StatusOK || icsProp(lines, "SEQUENCE") != "1" || strings.Contains(string(ics), room.GuestURL) {
		t.Fatalf("invite after rotate: %d SEQUENCE=%s", code, icsProp(lines, "SEQUENCE"))
	}
	if code, _ := h.do("POST", "/api/v1/rooms/"+room.ID+"/revoke", "", nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if seq(m.ID) != 2 || seq(o.ID) != 0 {
		t.Fatalf("after revoke: %d %d", seq(m.ID), seq(o.ID))
	}
}
