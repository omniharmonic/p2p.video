package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/filespeech"
	"github.com/omniharmonic/studiolink/host/internal/handoff"
	"github.com/omniharmonic/studiolink/host/internal/parachute"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

// fakeBuiltin stands in for filespeech.Broker (the Mac app's recognizer).
type fakeBuiltin struct {
	mu       sync.Mutex
	status   filespeech.Status
	byFile   map[string][]filespeech.Segment
	fail     map[string]error
	calls    []string
	langs    []string
	progress map[string][]float64 // reported before answering
	block    map[string]chan struct{}
	started  chan string
}

func (f *fakeBuiltin) Status() filespeech.Status { f.mu.Lock(); defer f.mu.Unlock(); return f.status }

func (f *fakeBuiltin) Transcribe(ctx context.Context, req filespeech.Request) (*filespeech.Result, error) {
	name := filepath.Base(req.Path)
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.langs = append(f.langs, req.Language)
	steps, block, fail, segs, started := f.progress[name], f.block[name], f.fail[name], f.byFile[name], f.started
	f.mu.Unlock()
	for _, p := range steps {
		req.Progress(p)
	}
	if started != nil {
		started <- name
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail != nil {
		return nil, fail
	}
	if segs == nil {
		segs = []filespeech.Segment{}
	}
	return &filespeech.Result{Engine: "apple-speechtranscriber", ASR: "apple-speechtranscriber", Locale: "en-US", DurationSec: req.DurationSec, Segments: segs}, nil
}

func readyBuiltin() *fakeBuiltin {
	return &fakeBuiltin{
		status: filespeech.Status{Connected: true, State: filespeech.StateReady, Name: "p2p.video on-device transcription", Engine: "apple-speechtranscriber"},
		byFile: map[string][]filespeech.Segment{
			"Ana.wav": {{Start: 0.3, End: 1.5, Text: "Welcome back.", Confidence: conf(0.9), Words: []filespeech.Word{{Word: "Welcome", Start: 0.3, End: 0.8, Confidence: 0.95}, {Word: "back.", Start: 0.8, End: 1.5, Confidence: 0.85}}}, {Start: 4, End: 5, Text: "Go on."}},
			"Ben.wav": {{Start: 1.0, End: 2.5, Text: "Thanks."}},
		},
	}
}

func setupBuiltin(t *testing.T) (*Service, *fakeBuiltin, string) {
	t.Helper()
	svc, _, _, dir := setup(t)
	fb := readyBuiltin()
	svc.Builtin = fb
	if err := svc.SetTranscriptionEngine(EngineBuiltin); err != nil {
		t.Fatal(err)
	}
	return svc, fb, dir
}

func TestBuiltinIsTheDefaultEngine(t *testing.T) {
	svc := &Service{Package: &fakePackage{dir: t.TempDir()}, StateDir: t.TempDir()}
	defer svc.Close()
	if svc.TranscriptionEngine() != EngineBuiltin {
		t.Fatalf("default %q", svc.TranscriptionEngine())
	}
}

func TestBuiltinTranscriptMatchesTheMinutesShape(t *testing.T) {
	svc, fb, dir := setupBuiltin(t)
	if err := svc.StartTranscript("take1"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	v := svc.View("take1")
	if v.Transcript.State != StateDone || !v.HasTranscript || !v.HasHandoff {
		t.Fatalf("view %+v", v)
	}
	if strings.Join(fb.calls, ",") != "Ana.wav,Ben.wav" || fb.langs[0] != "en-US" {
		t.Fatalf("calls %v %v", fb.calls, fb.langs)
	}
	doc, err := transcript.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Engine.Name != EngineNameBuiltin || doc.Engine.ASR != "apple-speechtranscriber" || doc.Engine.Locale != "en-US" || doc.Engine.Version != BuiltinEngineVersion {
		t.Fatalf("engine %+v", doc.Engine)
	}
	// Same merge as the Minutes path: offsets applied, speaker = participant.
	var got []string
	for _, s := range doc.Segments {
		got = append(got, s.Speaker+"@"+trimFloat(s.Start)+":"+s.Text)
	}
	if strings.Join(got, " | ") != "Ana@0.5:Welcome back. | Ben@2:Thanks. | Ana@4.2:Go on." {
		t.Fatalf("segments %v", got)
	}
	if w := doc.Segments[0].Words; len(w) != 2 || w[0].Start != 0.5 || w[1].End != 1.7 || *doc.Segments[0].Confidence != 0.9 {
		t.Fatalf("words %+v", doc.Segments[0])
	}
	for _, f := range []string{"transcript/transcript.md", "transcript/transcript.srt", "transcript/transcript.vtt", "handoff/ave/transcript.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
	var hd handoff.Document
	data, _ := os.ReadFile(filepath.Join(dir, "handoff", "clip-editor.json"))
	if json.Unmarshal(data, &hd) != nil || hd.Transcript == nil || hd.Transcript.Segments != 3 {
		t.Fatalf("handoff %+v", hd.Transcript)
	}
	tracks, _ := v.Transcript.Detail["tracks"].([]TrackProgress)
	if v.Transcript.Detail["engine"] != EngineBuiltin || len(tracks) != 2 || tracks[0].State != TrackDone || tracks[1].Progress != 1 {
		t.Fatalf("detail %+v", v.Transcript.Detail)
	}
	// Parachute metadata names the engine.
	note := parachute.SessionNote("Meetings", SessionFromManifest(mustManifest(t, dir), "take1", dir), doc)
	if note.Metadata["transcript"] != "builtin" {
		t.Fatalf("note metadata %v", note.Metadata)
	}
}

func trimFloat(v float64) string { b, _ := json.Marshal(v); return string(b) }

func mustManifest(t *testing.T, dir string) *export.Manifest {
	t.Helper()
	m, _, err := export.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSilenceOnlyTrackIsAnEmptySuccess(t *testing.T) {
	svc, fb, dir := setupBuiltin(t)
	fb.byFile["Ben.wav"] = []filespeech.Segment{}
	_ = svc.StartTranscript("take1")
	svc.Wait()
	v := svc.View("take1")
	doc, err := transcript.Read(dir)
	if err != nil || v.Transcript.State != StateDone || doc.Speakers[1].Status != "empty" || doc.Speakers[1].Error != "" {
		t.Fatalf("%+v %+v %v", v.Transcript, doc, err)
	}
	if tracks := v.Transcript.Detail["tracks"].([]TrackProgress); tracks[1].State != TrackEmpty {
		t.Fatalf("tracks %+v", tracks)
	}
	// Every track silent: still a valid (empty) transcript, not a failure.
	fb.byFile = map[string][]filespeech.Segment{}
	_ = svc.StartTranscript("take1")
	svc.Wait()
	if v := svc.View("take1"); v.Transcript.State != StateDone {
		t.Fatalf("all silent: %+v", v.Transcript)
	}
	if doc, _ := transcript.Read(dir); len(doc.Segments) != 0 {
		t.Fatalf("segments %+v", doc.Segments)
	}
}

func TestBuiltinUnavailableSaysWhatToDo(t *testing.T) {
	svc, fb, _ := setupBuiltin(t)
	fb.status = filespeech.Status{} // app not running
	v := svc.View("take1")
	if v.Capabilities.Transcript.Available || v.Capabilities.Transcript.Code != filespeech.CodeNoProvider || !strings.Contains(v.Capabilities.Transcript.Reason, "Open p2p.video") {
		t.Fatalf("no app: %+v", v.Capabilities.Transcript)
	}
	if err := svc.StartTranscript("take1"); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "Open p2p.video") {
		t.Fatalf("start without app: %v", err)
	}
	fb.status = filespeech.Status{Connected: true, State: filespeech.StateNotAuthorized}
	v = svc.View("take1")
	if v.Capabilities.Transcript.Code != filespeech.CodeNotAuthorized || !strings.Contains(v.Capabilities.Transcript.Reason, "Speech Recognition") {
		t.Fatalf("no permission: %+v", v.Capabilities.Transcript)
	}
	// The engine list always carries both engines with reasons.
	if len(v.Transcription.Engines) != 2 || v.Transcription.Engine != EngineBuiltin || !v.Transcription.Engines[1].Available {
		t.Fatalf("engines %+v", v.Transcription)
	}
}

func TestPermissionDeniedDuringJobIsAnExplicitError(t *testing.T) {
	svc, fb, dir := setupBuiltin(t)
	denied := &filespeech.Error{Code: filespeech.CodeNotAuthorized, Message: filespeech.PermissionMessage}
	fb.fail = map[string]error{"Ana.wav": denied, "Ben.wav": denied}
	_ = svc.StartTranscript("take1")
	svc.Wait()
	v := svc.View("take1")
	if v.Transcript.State != StateError || !strings.Contains(v.Transcript.Message, "Speech Recognition is not allowed") {
		t.Fatalf("%+v", v.Transcript)
	}
	tracks := v.Transcript.Detail["tracks"].([]TrackProgress)
	if tracks[0].State != TrackError || !strings.Contains(tracks[0].Error, "Speech Recognition") {
		t.Fatalf("tracks %+v", tracks)
	}
	if _, err := os.Stat(filepath.Join(dir, "transcript")); !os.IsNotExist(err) {
		t.Fatal("a failed job must not write transcript/")
	}
}

func TestProgressCancelAndResume(t *testing.T) {
	svc, fb, dir := setupBuiltin(t)
	fb.progress = map[string][]float64{"Ben.wav": {4.25}}
	fb.block = map[string]chan struct{}{"Ben.wav": make(chan struct{})}
	fb.started = make(chan string, 4)
	before, _ := os.ReadDir(dir)
	if err := svc.StartTranscript("take1"); err != nil {
		t.Fatal(err)
	}
	<-fb.started // Ana
	<-fb.started // Ben, blocked mid-file
	v := svc.View("take1")
	tracks := v.Transcript.Detail["tracks"].([]TrackProgress)
	if v.Transcript.State != StateRunning || tracks[0].State != TrackDone || tracks[1].State != TrackRunning || tracks[1].Progress != 0.5 {
		t.Fatalf("running %+v %+v", v.Transcript, tracks)
	}
	if !strings.Contains(v.Transcript.Message, "Ben (2 of 2)") {
		t.Fatalf("message %q", v.Transcript.Message)
	}
	if err := svc.CancelTranscript("take1"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	v = svc.View("take1")
	if v.Transcript.State != StateCancelled || !v.CanResume {
		t.Fatalf("cancelled %+v", v)
	}
	if tracks := v.Transcript.Detail["tracks"].([]TrackProgress); tracks[1].State != TrackCancelled {
		t.Fatalf("tracks %+v", tracks)
	}
	after, _ := os.ReadDir(dir)
	if len(after) != len(before) {
		t.Fatalf("cancel changed the package: %v -> %v", names(before), names(after))
	}
	if err := svc.CancelTranscript("take1"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("cancel idle: %v", err)
	}
	// Retry: only Ben runs again, Ana is reused.
	fb.mu.Lock()
	fb.calls, fb.block, fb.started = nil, nil, nil
	fb.mu.Unlock()
	_ = svc.StartTranscript("take1")
	svc.Wait()
	v = svc.View("take1")
	if strings.Join(fb.calls, ",") != "Ben.wav" || v.Transcript.State != StateDone || v.CanResume {
		t.Fatalf("resume %v %+v", fb.calls, v.Transcript)
	}
	if tracks := v.Transcript.Detail["tracks"].([]TrackProgress); !tracks[0].Reused || tracks[1].Reused {
		t.Fatalf("reuse flags %+v", tracks)
	}
	// A full success clears the kept tracks: the next run redoes everything.
	fb.mu.Lock()
	fb.calls = nil
	fb.mu.Unlock()
	_ = svc.StartTranscript("take1")
	svc.Wait()
	if strings.Join(fb.calls, ",") != "Ana.wav,Ben.wav" {
		t.Fatalf("fresh run %v", fb.calls)
	}
}

func names(es []os.DirEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestKeptTracksAreTiedToEngineAndManifest(t *testing.T) {
	svc, fb, dir := setupBuiltin(t)
	fb.fail = map[string]error{"Ben.wav": errors.New("boom")}
	_ = svc.StartTranscript("take1")
	svc.Wait()
	// A re-export changes the manifest: Ana must be transcribed again.
	raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, ' '), 0o644)
	fb.mu.Lock()
	fb.calls, fb.fail = nil, nil
	fb.mu.Unlock()
	_ = svc.StartTranscript("take1")
	svc.Wait()
	if strings.Join(fb.calls, ",") != "Ana.wav,Ben.wav" {
		t.Fatalf("calls after re-export %v", fb.calls)
	}
}

func TestEngineSelectionPersists(t *testing.T) {
	svc, _, _ := setupBuiltin(t)
	if err := svc.SetTranscriptionEngine(EngineMinutes); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(svc.StateDir, "transcription.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("pref file %v %v", info, err)
	}
	again := &Service{Package: svc.Package, StateDir: svc.StateDir}
	defer again.Close()
	if again.TranscriptionEngine() != EngineMinutes {
		t.Fatalf("reloaded %q", again.TranscriptionEngine())
	}
	if err := svc.SetTranscriptionEngine("cloud"); !errors.Is(err, ErrUnknownEngine) {
		t.Fatalf("unknown engine: %v", err)
	}
	if v := svc.View("take1"); v.Transcription.Engine != EngineMinutes || !v.Capabilities.Transcript.Available {
		t.Fatalf("view %+v", v.Transcription)
	}
}

func TestAutoTranscribeWithoutEngineShowsAnError(t *testing.T) {
	svc, fb, _ := setupBuiltin(t)
	fb.status = filespeech.Status{}
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings := store.DefaultSessionSettings()
	settings.AutoTranscribe = true
	raw, _ := json.Marshal(settings)
	if err := st.CreateSession(&store.Session{ID: "sess", Title: "Pilot", CreatedAt: time.Now(), PresetJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTake(&store.Take{ID: "take1", SessionID: "sess", Number: 1, Status: "complete", WallClockStart: time.Now()}); err != nil {
		t.Fatal(err)
	}
	svc.Store = st
	svc.TakeExported("take1")
	v := svc.View("take1")
	if v.Transcript.State != StateError || !strings.Contains(v.Transcript.Message, "Auto-transcribe could not start") || !strings.Contains(v.Transcript.Message, "Open p2p.video") {
		t.Fatalf("%+v", v.Transcript)
	}
}

// The real broker end to end, with a scripted provider connection.
type scriptConn struct {
	b       *filespeech.Broker
	replies func(m filespeech.Message) []filespeech.Message
}

func (c *scriptConn) Send(raw []byte) bool {
	var m filespeech.Message
	_ = json.Unmarshal(raw, &m)
	go func() {
		for _, r := range c.replies(m) {
			out, _ := json.Marshal(r)
			c.b.Handle(c, out)
		}
	}()
	return true
}
func (c *scriptConn) Close() {}

func TestBuiltinThroughTheRealBroker(t *testing.T) {
	svc, _, dir := setupBuiltin(t)
	b := filespeech.NewBroker()
	conn := &scriptConn{b: b, replies: func(m filespeech.Message) []filespeech.Message {
		if m.T != filespeech.MsgJob {
			return nil
		}
		if !filepath.IsAbs(m.Path) || !strings.HasPrefix(m.Path, dir) {
			return []filespeech.Message{{T: filespeech.MsgError, JobID: m.JobID, Code: "bad_job", Message: "unexpected path"}}
		}
		seg := &filespeech.Segment{Start: 0.1, End: 0.9, Text: "Hello from " + filepath.Base(m.Path), Words: []filespeech.Word{{Word: "Hello", Start: 0.1, End: 0.4, Confidence: 0.9}}}
		return []filespeech.Message{{T: filespeech.MsgProgress, JobID: m.JobID, ProcessedSec: 1}, {T: filespeech.MsgSegment, JobID: m.JobID, Segment: seg}, {T: filespeech.MsgDone, JobID: m.JobID, ASR: "fake", Locale: "en-US"}}
	}}
	if err := b.Attach(conn, filespeech.Message{T: filespeech.MsgHello, Protocol: 1, Name: "Script", Engine: "fake", State: filespeech.StateReady}); err != nil {
		t.Fatal(err)
	}
	svc.Builtin = b
	if err := svc.StartTranscript("take1"); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	doc, err := transcript.Read(dir)
	if err != nil || len(doc.Segments) != 2 || doc.Segments[0].Text != "Hello from Ana.wav" || doc.Segments[1].Speaker != "Ben" || doc.Engine.ASR != "fake" {
		t.Fatalf("%+v %v", doc, err)
	}
}
