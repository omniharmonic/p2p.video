package upload

// Regression tests for H1 and L6 [SL-SEC-1].

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

type limitRig struct {
	h     *Handler
	st    *store.Store
	cfg   *config.Config
	token map[string]string
}

// newLimitRig: session s with participants p_a and p_b, one recording take
// and one video track each.
func newLimitRig(t *testing.T) *limitRig {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{DataDir: filepath.Join(dir, "data"), SecretKey: []byte("limits-test-secret-key-0123456789"), MaxChunkSizeMB: 1}
	now := time.Now().UTC()
	_ = st.CreateSession(&store.Session{ID: "s", Title: "T", CreatedAt: now, PresetJSON: `{"video_bitrate_bps":1000000}`})
	_ = st.CreateTake(&store.Take{ID: "take", SessionID: "s", Number: 1, StartAtHostUs: 1, WallClockStart: now, Status: "recording"})
	rig := &limitRig{h: &Handler{Store: st, Config: cfg}, st: st, cfg: cfg, token: map[string]string{}}
	for _, p := range []string{"p_a", "p_b"} {
		_ = st.CreateParticipant(&store.Participant{ID: p, SessionID: "s", Name: p, Role: "guest", Status: "admitted", JoinedAt: now})
		_ = st.CreateSource(&store.Source{ID: "src_" + p, ParticipantID: p, Kind: "camera"})
		_ = st.CreateTrack(&store.Track{ID: "trk_" + p, TakeID: "take", SourceID: "src_" + p, Kind: "video", Mime: "video/webm", Status: "recording", LastSeq: -1})
		rig.token[p] = signaling.MintUploadToken(p, "s", time.Hour, cfg.SecretKey)
	}
	return rig
}

func (r *limitRig) put(p string, seq int, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/takes/take/tracks/trk_%s/chunks/%d", p, seq), body)
	req.Header.Set("Authorization", "Bearer "+r.token[p])
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

// TestH1SlowUploadDoesNotBlockOthers: a chunk whose body is still arriving
// holds no lock that another participant's upload (or a retry on the same
// track) needs.
func TestH1SlowUploadDoesNotBlockOthers(t *testing.T) {
	rig := newLimitRig(t)
	pr, pw := io.Pipe()
	slowDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { slowDone <- rig.put("p_a", 0, pr) }()
	_, _ = pw.Write([]byte("first half "))
	time.Sleep(50 * time.Millisecond) // the slow request is now inside the handler, mid-body

	fast := make(chan int, 2)
	go func() { fast <- rig.put("p_b", 0, strings.NewReader("other participant")).Code }()
	go func() { fast <- rig.put("p_a", 1, strings.NewReader("same track, next chunk")).Code }()
	for i := 0; i < 2; i++ {
		select {
		case code := <-fast:
			if code != http.StatusCreated {
				t.Fatalf("concurrent upload: %d", code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("an upload was blocked behind a slow body (handler-wide lock)")
		}
	}
	_, _ = pw.Write([]byte("second half"))
	_ = pw.Close()
	if rec := <-slowDone; rec.Code != http.StatusCreated {
		t.Fatalf("slow upload: %d %s", rec.Code, rec.Body)
	}
	if b, _ := rig.st.StoredChunkBytes("trk_p_a"); b != int64(len("first half second half")+len("same track, next chunk")) {
		t.Fatalf("stored bytes %d", b)
	}
}

func TestH1ChunkAndSequenceLimits(t *testing.T) {
	rig := newLimitRig(t)
	big := bytes.Repeat([]byte{1}, 1<<20+1)
	if rec := rig.put("p_a", 0, bytes.NewReader(big)); rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "fragment limit") {
		t.Fatalf("oversized chunk: %d %s", rec.Code, rec.Body)
	}
	// Unknown length (chunked body): still cut off at the limit.
	if rec := rig.put("p_a", 0, io.MultiReader(bytes.NewReader(big))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized streamed chunk: %d", rec.Code)
	}
	if rec := rig.put("p_a", MaxSeq+1, strings.NewReader("x")); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("seq over cap: %d", rec.Code)
	}
	if rec := rig.put("p_a", MaxSeq, strings.NewReader("x")); rec.Code != http.StatusCreated {
		t.Fatalf("seq at cap: %d %s", rec.Code, rec.Body)
	}
	entries, _ := os.ReadDir(filepath.Join(rig.cfg.DataDir, "sessions", "s", "takes", "take", "raw", "trk_p_a"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("rejected upload left a temp file: %s", e.Name())
		}
	}
}

// TestH1TrackQuota: a track cannot hold more than the documented quota.
func TestH1TrackQuota(t *testing.T) {
	rig := newLimitRig(t)
	tk, _ := rig.st.GetTake("take")
	rig.h.now = func() time.Time { return tk.WallClockStart }
	// 1 Mbit/s preset, floor lowered for the test: (0 s + 10 min) × 1.5.
	rig.h.quotaFloorBps = 1
	want := int64(1_000_000 / 8 * QuotaGrace.Seconds() * QuotaFactor)
	if got := rig.h.TrackQuota(tk); got != want {
		t.Fatalf("quota %d, want %d", got, want)
	}
	// The quota grows with a recording take's wall-clock duration...
	rig.h.now = func() time.Time { return tk.WallClockStart.Add(time.Hour) }
	if got := rig.h.TrackQuota(tk); got <= want {
		t.Fatalf("quota did not grow with the take: %d", got)
	}
	// ...and the default floor keeps low presets generous.
	rig.h.quotaFloorBps = 0
	if got := rig.h.TrackQuota(tk); got < int64(QuotaFloorBps/8*(time.Hour+QuotaGrace).Seconds()) {
		t.Fatalf("floor not applied: %d", got)
	}

	// Force a tiny quota by pretending the preset is tiny and fill it.
	_ = rig.st.UpdateSessionSettings("s", `{"video_bitrate_bps":8}`) // 1 byte/s
	rig.h.quotaFloorBps = 1
	rig.h.now = func() time.Time { return tk.WallClockStart }
	quota := rig.h.TrackQuota(tk) // 1 × 600 × 1.5 = 900 bytes
	if quota != 900 {
		t.Fatalf("tiny quota %d", quota)
	}
	if rec := rig.put("p_a", 0, bytes.NewReader(bytes.Repeat([]byte{2}, 800))); rec.Code != http.StatusCreated {
		t.Fatalf("within quota: %d %s", rec.Code, rec.Body)
	}
	rec := rig.put("p_a", 1, bytes.NewReader(bytes.Repeat([]byte{3}, 200)))
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "storage limit") {
		t.Fatalf("over quota: %d %s", rec.Code, rec.Body)
	}
	// Another track has its own quota.
	if rec := rig.put("p_b", 0, bytes.NewReader(bytes.Repeat([]byte{4}, 800))); rec.Code != http.StatusCreated {
		t.Fatalf("other track: %d", rec.Code)
	}
}

// TestH1TracksPerParticipantCap: registration is refused past the cap.
func TestH1TracksPerParticipantCap(t *testing.T) {
	rig := newLimitRig(t)
	register := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/takes/take/tracks", strings.NewReader(`{"kind":"audio","mime":"audio/webm","label":"mic\u202e"}`))
		req.Header.Set("Authorization", "Bearer "+rig.token["p_b"])
		rec := httptest.NewRecorder()
		rig.h.ServeHTTP(rec, req)
		return rec
	}
	for n := 1; n < MaxTracksPerParticipantTake; n++ { // p_b already has one track
		if rec := register(); rec.Code != http.StatusCreated {
			t.Fatalf("track %d: %d %s", n, rec.Code, rec.Body)
		}
	}
	if rec := register(); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "limit") {
		t.Fatalf("over cap: %d %s", rec.Code, rec.Body)
	}
	srcs, _ := rig.st.ListSourcesByParticipant("p_b")
	for _, s := range srcs {
		if strings.ContainsRune(s.Label, '\u202e') {
			t.Fatalf("bidi override kept in track label %q", s.Label)
		}
	}
}

// TestL6UploadFilesAreOwnerOnly: chunk directories, chunks and the
// assembled recording are not readable by other local users.
func TestL6UploadFilesAreOwnerOnly(t *testing.T) {
	rig := newLimitRig(t)
	if rec := rig.put("p_a", 0, strings.NewReader("media")); rec.Code != http.StatusCreated {
		t.Fatal(rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/takes/take/tracks/trk_p_a/complete", strings.NewReader(`{"lastSeq":0,"totalBytes":5}`))
	req.Header.Set("Authorization", "Bearer "+rig.token["p_a"])
	rec := httptest.NewRecorder()
	rig.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body)
	}
	raw := filepath.Join(rig.cfg.DataDir, "sessions", "s", "takes", "take", "raw")
	for path, want := range map[string]os.FileMode{
		filepath.Join(raw, "trk_p_a"):               0o700,
		filepath.Join(raw, "trk_p_a", "000000.bin"): 0o600,
		filepath.Join(raw, "trk_p_a.webm"):          0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode %v, want %v", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
}
