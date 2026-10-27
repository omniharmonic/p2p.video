package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/minutes"
	"github.com/omniharmonic/studiolink/host/internal/parachute"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

type fakePackage struct {
	mu  sync.Mutex
	dir string
}

func (f *fakePackage) PublishedPackageDir(string) (string, error) { return f.dir, nil }
func (f *fakePackage) WithPublishedPackage(_ string, fn func(string) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f.dir)
}

type fakeMinutes struct {
	mu      sync.Mutex
	calls   []string
	byFile  map[string]*minutes.FileTranscript
	fail    map[string]error
	block   chan struct{}
	onCall  func()
	support error
}

func (f *fakeMinutes) Supported(context.Context) error { return f.support }
func (f *fakeMinutes) TranscribeFile(ctx context.Context, audio, speaker string, _ float64) (*minutes.FileTranscript, error) {
	f.mu.Lock()
	f.calls = append(f.calls, filepath.Base(audio)+"|"+speaker)
	cb := f.onCall
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
	if f.block != nil {
		<-f.block
	}
	if err := f.fail[filepath.Base(audio)]; err != nil {
		return nil, err
	}
	return f.byFile[filepath.Base(audio)], nil
}

type fakeMixer struct {
	inputs []ffmpeg.MixInput
}

func (m *fakeMixer) Mix(in []ffmpeg.MixInput, out string) error {
	m.inputs = in
	return os.WriteFile(out, []byte("RIFF"), 0o644)
}
func (m *fakeMixer) Duration(string) (float64, error) { return 9.5, nil }

func conf(v float64) *float64 { return &v }

func setup(t *testing.T) (*Service, *fakeMinutes, *fakeMixer, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	sources := []export.ExportSource{
		{ID: "a-ana", Participant: "Ana", ParticipantID: "p1", Kind: "audio", FileRelPath: "audio/Ana.wav", OffsetSec: 0.2, DurationSec: 9},
		{ID: "a-ben", Participant: "Ben", ParticipantID: "p2", Kind: "audio", FileRelPath: "audio/Ben.wav", OffsetSec: 1.0, DurationSec: 8.5},
	}
	if err := export.WriteManifest(dir, "sess", "Pilot", 1, time.Date(2026, 10, 25, 17, 0, 0, 0, time.UTC), 30, sources); err != nil {
		t.Fatal(err)
	}
	fm := &fakeMinutes{byFile: map[string]*minutes.FileTranscript{
		"Ana.wav": {Schema: minutes.FileTranscriptSchema, Engine: minutes.Engine{ASR: "apple-speech-on-device", Locale: "en-US"}, Segments: []minutes.Segment{
			{Start: 0.3, End: 1.5, Text: "Welcome back.", Confidence: conf(0.9)}, {Start: 4, End: 5, Text: "Go on."}}},
		"Ben.wav": {Schema: minutes.FileTranscriptSchema, Segments: []minutes.Segment{{Start: 1.0, End: 2.5, Text: "Thanks."}}},
	}}
	mx := &fakeMixer{}
	svc := &Service{Package: &fakePackage{dir: dir}, Minutes: fm, Mixer: mx, StateDir: filepath.Join(t.TempDir(), "integrations")}
	t.Cleanup(svc.Close)
	// These tests cover the Minutes engine; built-in tests are in engines_test.go.
	if err := svc.SetTranscriptionEngine(EngineMinutes); err != nil {
		t.Fatal(err)
	}
	return svc, fm, mx, dir
}

func TestTranscriptJobMergesIsolatedTracksIntoPackage(t *testing.T) {
	svc, fm, mx, dir := setup(t)
	if err := svc.StartTranscript("take1"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	v := svc.View("take1")
	if v.Transcript.State != StateDone || !v.HasTranscript || !v.HasHandoff || v.ClipEditor.State != StateDone {
		t.Fatalf("view %+v", v)
	}
	if strings.Join(fm.calls, ",") != "Ana.wav|Ana,Ben.wav|Ben" {
		t.Fatalf("calls %v", fm.calls)
	}
	doc, err := transcript.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, s := range doc.Segments {
		got = append(got, fmt.Sprintf("%s@%g:%s", s.Speaker, s.Start, s.Text))
	}
	// Ana 0.3+0.2=0.5, Ben 1.0+1.0=2.0, Ana 4+0.2=4.2
	if strings.Join(got, " | ") != "Ana@0.5:Welcome back. | Ben@2:Thanks. | Ana@4.2:Go on." {
		t.Fatalf("segments %v", got)
	}
	if doc.Segments[0].ParticipantID != "p1" || doc.Engine.Name != "minutes-cli" || len(doc.Speakers) != 2 || doc.Speakers[1].Status != "ok" {
		t.Fatalf("doc %+v", doc)
	}
	for _, f := range []string{"transcript/transcript.srt", "transcript/transcript.vtt", "transcript/transcript.md", "handoff/clip-editor.json", "handoff/program.wav", "handoff/ave/transcript.json", "handoff/ave/sync.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if len(mx.inputs) != 2 || mx.inputs[1].OffsetSec != 1.0 || !strings.HasSuffix(mx.inputs[1].Path, "audio/Ben.wav") {
		t.Fatalf("mix inputs %+v", mx.inputs)
	}
}

func TestTranscriptPartialAndTotalFailure(t *testing.T) {
	svc, fm, _, dir := setup(t)
	fm.fail = map[string]error{"Ben.wav": errors.New("Speech recognition permission denied")}
	_ = svc.StartTranscript("take1")
	svc.Wait()
	v := svc.View("take1")
	if v.Transcript.State != StateDone || !strings.Contains(v.Transcript.Message, "Some tracks failed: Ben") {
		t.Fatalf("partial: %+v", v.Transcript)
	}
	doc, _ := transcript.Read(dir)
	if doc.Speakers[1].Status != "error" || !strings.Contains(doc.Speakers[1].Error, "permission denied") || len(doc.Segments) != 2 {
		t.Fatalf("doc %+v", doc)
	}
	// Retry redoes only the failed track; Ana's finished track is kept.
	if v := svc.View("take1"); !v.CanResume {
		t.Fatal("a partial failure must be resumable")
	}
	fm.calls = nil
	delete(fm.fail, "Ben.wav")
	_ = svc.StartTranscript("take1")
	svc.Wait()
	if strings.Join(fm.calls, ",") != "Ben.wav|Ben" {
		t.Fatalf("retry calls %v", fm.calls)
	}
	if v := svc.View("take1"); v.Transcript.State != StateDone || v.CanResume || strings.Contains(v.Transcript.Message, "failed") {
		t.Fatalf("retry: %+v", v)
	}
	if doc, _ := transcript.Read(dir); len(doc.Segments) != 3 || doc.Speakers[1].Status != "ok" {
		t.Fatalf("retried doc %+v", doc)
	}

	total, fm2, _, _ := setup(t)
	fm2.fail = map[string]error{"Ana.wav": errors.New("boom"), "Ben.wav": errors.New("bang")}
	_ = total.StartTranscript("take1")
	total.Wait()
	if v := total.View("take1"); v.Transcript.State != StateError || !strings.Contains(v.Transcript.Message, "Ana: boom") {
		t.Fatalf("total: %+v", v.Transcript)
	}
}

func TestTranscriptRefusesWhenPackageChangesMidRun(t *testing.T) {
	svc, fm, _, dir := setup(t)
	fm.onCall = func() {
		_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema":"studiolink.take/1","take":2,"sources":[]}`), 0o644)
	}
	_ = svc.StartTranscript("take1")
	svc.Wait()
	v := svc.View("take1")
	if v.Transcript.State != StateError || !strings.Contains(v.Transcript.Message, "changed during transcription") || v.HasTranscript {
		t.Fatalf("view %+v", v)
	}
}

func TestOneJobPerKindAndUnavailableMinutes(t *testing.T) {
	svc, fm, _, _ := setup(t)
	fm.block = make(chan struct{})
	if err := svc.StartTranscript("take1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartTranscript("take1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start: %v", err)
	}
	if v := svc.View("take1"); v.Transcript.State != StateRunning {
		t.Fatalf("running state: %+v", v.Transcript)
	}
	close(fm.block)
	svc.Wait()

	none := &Service{Package: &fakePackage{dir: t.TempDir()}, MinutesReason: "minutes-cli not found"}
	defer none.Close()
	if err := none.SetTranscriptionEngine(EngineMinutes); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "optional") {
		t.Fatalf("select missing minutes: %v", err)
	}
	if err := none.StartTranscript("t"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no engine: %v", err)
	}
	old := &Service{Package: &fakePackage{dir: t.TempDir()}, Minutes: &fakeMinutes{support: minutes.ErrUnsupported}}
	defer old.Close()
	v := old.View("t")
	if v.Capabilities.Minutes.Available || !strings.Contains(v.Capabilities.Minutes.Reason, "transcribe-file") {
		t.Fatalf("old CLI view: %+v", v.Capabilities)
	}
	if v.Capabilities.Parachute.Available || v.Capabilities.ProgramMix.Available {
		t.Fatalf("unconfigured caps: %+v", v.Capabilities)
	}
}

func TestClipEditorWithoutTranscript(t *testing.T) {
	svc, _, _, dir := setup(t)
	if err := svc.WriteClipEditor("take1"); err != nil {
		t.Fatal(err)
	}
	v := svc.View("take1")
	if v.ClipEditor.State != StateDone || !strings.Contains(v.ClipEditor.Message, "Transcribe the take") || !v.HasHandoff || v.HasTranscript {
		t.Fatalf("view %+v", v)
	}
	if _, err := os.Stat(filepath.Join(dir, "handoff/ave/transcript.json")); !os.IsNotExist(err) {
		t.Fatal("ave transcript without a transcript")
	}
}

const token = "aaaa.bbbb.cccc-secret-signature"

type tok struct{}

func (tok) Token(context.Context) (string, error) { return token, nil }

func TestParachuteSendAndResendWithReceipts(t *testing.T) {
	svc, _, _, _ := setup(t)
	var mu sync.Mutex
	var bodies []map[string]any
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		data, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(data, &b)
		bodies, methods = append(bodies, b), append(methods, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"note": map[string]any{"id": "note-1", "updatedAt": "v" + string(rune('0'+len(bodies))), "path": b["path"]}})
	}))
	defer srv.Close()
	svc.Parachute = &parachute.Client{Origin: srv.URL, Vault: "test", Tokens: tok{}, HTTP: srv.Client()}
	if err := svc.StartParachute("take1"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	v := svc.View("take1")
	if v.Parachute.State != StateDone || !strings.HasPrefix(v.Parachute.Message, "Saved in the vault: Meetings/") || v.Capabilities.Parachute.Target != srv.URL+"/vault/test" {
		t.Fatalf("view %+v", v)
	}
	if strings.Contains(v.Parachute.Message+jsonOf(v), token) {
		t.Fatal("token in status")
	}
	content := bodies[0]["content"].(string)
	if !strings.Contains(content, "_No transcript yet.") || !strings.Contains(content, "[[Ana]], [[Ben]]") {
		t.Fatalf("content %s", content)
	}
	info, err := os.Stat(filepath.Join(svc.StateDir, "parachute-receipts.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipts %v %v", info, err)
	}
	// After transcribing, a re-send updates the same note with the transcript.
	_ = svc.StartTranscript("take1")
	svc.Wait()
	_ = svc.StartParachute("take1")
	svc.Wait()
	if methods[1] != "PATCH /vault/test/api/notes/note-1" || bodies[1]["if_updated_at"] != "v1" {
		t.Fatalf("resend %v %v", methods, bodies[1])
	}
	if !strings.Contains(bodies[1]["content"].(string), "**Ana** (0:00): Welcome back.") {
		t.Fatalf("transcript missing from update: %s", bodies[1]["content"])
	}
	if v := svc.View("take1"); !strings.HasPrefix(v.Parachute.Message, "Updated in the vault") {
		t.Fatalf("update message %q", v.Parachute.Message)
	}
	unconfigured := &Service{Package: &fakePackage{dir: t.TempDir()}}
	defer unconfigured.Close()
	if err := unconfigured.StartParachute("t"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unconfigured: %v", err)
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestAutoTranscribeFollowsSessionSetting(t *testing.T) {
	svc, fm, _, _ := setup(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateSession(&store.Session{ID: "sess", Title: "Pilot", CreatedAt: time.Now(), PresetJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTake(&store.Take{ID: "take1", SessionID: "sess", Number: 1, Status: "complete", WallClockStart: time.Now()}); err != nil {
		t.Fatal(err)
	}
	svc.Store = st
	svc.TakeExported("take1")
	svc.Wait()
	if len(fm.calls) != 0 {
		t.Fatal("auto-transcribe ran while off")
	}
	settings := store.DefaultSessionSettings()
	settings.AutoTranscribe = true
	raw, _ := json.Marshal(settings)
	if err := st.UpdateSessionSettings("sess", string(raw)); err != nil {
		t.Fatal(err)
	}
	if !svc.View("take1").AutoTranscribe {
		t.Fatal("view must report the setting")
	}
	svc.TakeExported("take1")
	svc.Wait()
	if len(fm.calls) != 2 {
		t.Fatalf("auto-transcribe calls %v", fm.calls)
	}
}
