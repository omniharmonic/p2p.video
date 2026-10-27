package signaling

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

type fakeRefs struct{ started, stopped []string }

func (f *fakeRefs) StartTake(sessionID, dir string) error {
	f.started = append(f.started, sessionID+"|"+dir)
	return nil
}
func (f *fakeRefs) StopTake(sessionID string) { f.stopped = append(f.stopped, sessionID) }

// The alignment reference runs exactly for the take, into the take's refs
// directory beside raw/ (where postproc looks for it) [SL-P0-10].
func TestTakeStartsAndStopsReferenceRecording(t *testing.T) {
	data := t.TempDir()
	st, err := store.Open(filepath.Join(data, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	h := NewHub(st, &config.Config{SecretKey: []byte("secret"), DataDir: data}, nil)
	defer h.Close()
	refs := &fakeRefs{}
	h.References = refs
	h.startRecording("room")
	take := h.currentTakeID("room")
	if take == "" || len(refs.started) != 1 {
		t.Fatalf("take %q, reference starts %v", take, refs.started)
	}
	if want := "room|" + filepath.Join(data, "sessions", "room", "takes", take, "refs"); refs.started[0] != want {
		t.Fatalf("reference dir %q, want %q", refs.started[0], want)
	}
	h.stopRecording("room")
	if len(refs.stopped) != 1 || refs.stopped[0] != "room" {
		t.Fatalf("reference stops %v", refs.stopped)
	}
}

// A producer is never recorded, so the reference recorder may not keep
// their microphone either; waiting people and other sessions are refused.
func TestReferenceExcludesProducersAndOutsiders(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := NewHub(st, &config.Config{SecretKey: []byte("secret")}, nil)
	defer h.Close()
	for _, c := range []*Client{
		{ParticipantID: "guest", SessionID: "room", Role: "guest", Status: "admitted", Send: make(chan []byte, 10)},
		{ParticipantID: "pat", SessionID: "room", Role: RoleProducer, Status: "admitted", Send: make(chan []byte, 10)},
		{ParticipantID: "waiting", SessionID: "room", Role: "guest", Status: "pending", Send: make(chan []byte, 10)},
	} {
		h.Register(c)
	}
	for _, tc := range []struct {
		session, participant string
		want                 bool
	}{{"room", "guest", true}, {"room", "pat", false}, {"room", "waiting", false}, {"other", "guest", false}, {"room", "nobody", false}} {
		if got := h.RecordsReference(tc.session, tc.participant); got != tc.want {
			t.Errorf("RecordsReference(%s, %s) = %v", tc.session, tc.participant, got)
		}
	}
}
