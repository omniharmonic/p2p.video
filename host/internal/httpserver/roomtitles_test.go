package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/store"
)

// The lower thirds a room remembers [SL-P5-4] are host management: listed,
// edited and forgotten by the admin only (never a meeting's host token or a
// guest), and the next episode applies them.
func TestRoomTitlesManagedByHostOnly(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	path := "/api/v1/rooms/" + room.ID + "/titles"
	list := func(raw []byte) []store.RoomLowerThird {
		t.Helper()
		var out struct {
			Titles []store.RoomLowerThird `json:"titles"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("titles body %s: %v", raw, err)
		}
		return out.Titles
	}
	if code, raw := h.do("GET", path, "", nil); code != 200 || len(list(raw)) != 0 {
		t.Fatalf("empty list %d %s", code, raw)
	}
	// A right-to-left override and a newline are stripped like names.
	code, raw := h.do("PUT", path, "{\"displayName\":\" Ana  Ruiz \",\"name\":\"Dr. Ana Ruiz\",\"title\":\"Founder,\u202e Acme\\n\"}", nil)
	got := list(raw)
	if code != 200 || len(got) != 1 || got[0].Key != "ana ruiz" || got[0].DisplayName != "Ana Ruiz" || got[0].Name != "Dr. Ana Ruiz" || got[0].Title != "Founder, Acme" {
		t.Fatalf("put %d %s", code, raw)
	}
	if code, raw := h.do("PUT", path, `{"displayName":"  ","title":"x"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("blank display name %d %s", code, raw)
	}

	start := h.start(room.ID)
	for _, hdr := range []http.Header{{"Authorization": {"Bearer nope"}}, bearer(start.HostToken)} {
		for _, m := range []string{"GET", "PUT", "DELETE"} {
			if code, _ := h.do(m, path+"?key=ana+ruiz", `{"displayName":"Ana Ruiz","title":"Hacked"}`, hdr); code != http.StatusForbidden {
				t.Errorf("%s titles with %v = %d, want 403", m, hdr, code)
			}
		}
	}
	if rem, err := h.st.RoomLowerThirdFor(room.ID, "Ana Ruiz"); err != nil || rem.Title != "Founder, Acme" {
		t.Fatalf("title changed by a non-admin: %v %+v", err, rem)
	}

	// Forget by key; an unknown key is fine.
	if code, raw := h.do("DELETE", path+"?key="+url.QueryEscape("ana ruiz"), "", nil); code != 200 || len(list(raw)) != 0 {
		t.Fatalf("delete %d %s", code, raw)
	}
	if code, _ := h.do("DELETE", path+"?key=nobody", "", nil); code != 200 {
		t.Fatalf("delete unknown %d", code)
	}
	if code, _ := h.do("DELETE", path, "", nil); code != http.StatusBadRequest {
		t.Fatalf("delete without key %d", code)
	}
	// Both fields empty forgets too.
	h.do("PUT", path, `{"displayName":"Bo","title":"Producer"}`, nil)
	if code, raw := h.do("PUT", path, `{"displayName":"bo","name":"","title":""}`, nil); code != 200 || len(list(raw)) != 0 {
		t.Fatalf("clear %d %s", code, raw)
	}
}
