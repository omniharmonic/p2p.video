package captions

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
)

type fakeHub struct {
	mu       sync.Mutex
	admitted map[string]map[string]string // session -> participant -> name
	captions map[string][]signaling.CaptionPayload
	states   map[string][]signaling.CaptionStatePayload
}

func newFakeHub() *fakeHub {
	return &fakeHub{admitted: map[string]map[string]string{}, captions: map[string][]signaling.CaptionPayload{}, states: map[string][]signaling.CaptionStatePayload{}}
}

func (h *fakeHub) admit(sid, pid, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.admitted[sid] == nil {
		h.admitted[sid] = map[string]string{}
	}
	h.admitted[sid][pid] = name
}
func (h *fakeHub) remove(sid, pid string) { h.mu.Lock(); delete(h.admitted[sid], pid); h.mu.Unlock() }
func (h *fakeHub) BroadcastCaption(sid string, p signaling.CaptionPayload) {
	h.mu.Lock()
	h.captions[sid] = append(h.captions[sid], p)
	h.mu.Unlock()
}
func (h *fakeHub) BroadcastCaptionState(sid string, p signaling.CaptionStatePayload) {
	h.mu.Lock()
	h.states[sid] = append(h.states[sid], p)
	h.mu.Unlock()
}
func (h *fakeHub) AdmittedParticipant(sid, pid string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.admitted[sid][pid]
	return n, ok
}
func (h *fakeHub) CaptionSessions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{}
	for sid := range h.admitted {
		out = append(out, sid)
	}
	return out
}

type fakeProvider struct {
	mu     sync.Mutex
	text   []ProviderMessage
	frames [][]byte
	full   bool
}

func (p *fakeProvider) SendText(b []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var m ProviderMessage
	_ = json.Unmarshal(b, &m)
	p.text = append(p.text, m)
	return true
}
func (p *fakeProvider) SendBinary(b []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.full {
		return false
	}
	p.frames = append(p.frames, append([]byte(nil), b...))
	return true
}
func (p *fakeProvider) Close() {}
func (p *fakeProvider) starts() []ProviderMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []ProviderMessage
	for _, m := range p.text {
		if m.T == MsgStreamStart {
			out = append(out, m)
		}
	}
	return out
}
func (p *fakeProvider) ends() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, m := range p.text {
		if m.T == MsgStreamEnd {
			out = append(out, m.StreamID)
		}
	}
	return out
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// sine100ms is 100 ms of a 440 Hz tone (synthetic signal) as s16le.
func sine100ms(phase *float64) []byte {
	var buf bytes.Buffer
	for i := 0; i < SampleRate/10; i++ {
		v := int16(math.Sin(*phase) * 12000)
		*phase += 2 * math.Pi * 440 / SampleRate
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}
	return buf.Bytes()
}

func setup(t *testing.T) (*Manager, *fakeHub, *fakeProvider, *testClock) {
	t.Helper()
	hub := newFakeHub()
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	m := newManager(hub, clk.now)
	p := &fakeProvider{}
	if err := m.AttachProvider(p, ProviderMessage{T: MsgHello, Protocol: 1, Name: "Test", Engine: "fake"}); err != nil {
		t.Fatal(err)
	}
	return m, hub, p, clk
}

func result(stream, seg, text string, final bool, start, end int64) []byte {
	b, _ := json.Marshal(ProviderMessage{T: MsgResult, StreamID: stream, SegmentID: seg, Text: text, IsFinal: final, StartMs: start, EndMs: end})
	return b
}

func TestFrameRoundTripAndValidation(t *testing.T) {
	pcm := []byte{1, 2, 3, 4}
	key, got, err := DecodeFrame(EncodeFrame("p_abc-123", pcm))
	if err != nil || key != "p_abc-123" || !bytes.Equal(got, pcm) {
		t.Fatalf("round trip: %q %v %v", key, got, err)
	}
	bad := map[string][]byte{
		"empty":       {},
		"version":     append([]byte{2, 1, 'a'}, pcm...),
		"no key":      append([]byte{1, 0}, pcm...),
		"key too big": append([]byte{1, 65}, bytes.Repeat([]byte{'a'}, 65)...),
		"key chars":   append([]byte{1, 3, 'a', '/', 'b'}, pcm...),
		"odd pcm":     {1, 1, 'a', 1, 2, 3},
		"no pcm":      {1, 1, 'a'},
		"pcm > 1 s":   append([]byte{1, 1, 'a'}, make([]byte, 2*MaxFrameSamples+2)...),
	}
	for name, b := range bad {
		if _, _, err := DecodeFrame(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The golden frame and message fixture are shared with the Swift provider
// (macapp/Tests/StudioLinkCoreTests/CaptionProviderTests.swift) so the two
// implementations of the protocol cannot drift.
func TestFrameGoldenAndFixture(t *testing.T) {
	var pcm bytes.Buffer
	for _, s := range []int16{0, 1, -1, 32767, -32768} {
		_ = binary.Write(&pcm, binary.LittleEndian, s)
	}
	if got := hex.EncodeToString(EncodeFrame("s1", pcm.Bytes())); got != "0102733100000100ffffff7f0080" {
		t.Fatalf("golden frame %s", got)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "macapp", "Tests", "StudioLinkCoreTests", "Fixtures", "caption-provider-messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	var generic []json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, m := range generic {
		dec := json.NewDecoder(bytes.NewReader(m))
		dec.DisallowUnknownFields()
		var pm ProviderMessage
		if err := dec.Decode(&pm); err != nil {
			t.Fatalf("fixture message %s: %v", m, err)
		}
		again, _ := json.Marshal(pm)
		var a, b any
		_ = json.Unmarshal(m, &a)
		_ = json.Unmarshal(again, &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("round trip changed %s -> %s", m, again)
		}
	}
	if _, err := ParseProviderHello(generic[0]); err != nil {
		t.Fatalf("fixture hello rejected: %v", err)
	}
}

func TestAudioRoutedPerParticipantWithOpaqueStreamIDs(t *testing.T) {
	m, hub, p, clk := setup(t)
	hub.admit("s1", "p_host", "Host")
	hub.admit("s1", "p_guest", "Guest")
	if _, err := m.Control("s1", signaling.CaptionControlPayload{Enabled: true, Language: "en-GB"}); err != nil {
		t.Fatal(err)
	}
	var ph, pg float64
	for i := 0; i < 5; i++ {
		clk.advance(100 * time.Millisecond)
		for _, pid := range []string{"p_host", "p_guest"} {
			ph := &ph
			if pid == "p_guest" {
				ph = &pg
			}
			if err := m.Ingest("s1", EncodeFrame(pid, sine100ms(ph))); err != nil {
				t.Fatal(err)
			}
		}
	}
	starts := p.starts()
	if len(starts) != 2 || starts[0].StreamID == starts[1].StreamID {
		t.Fatalf("want two distinct streams, got %+v", starts)
	}
	for _, s := range starts {
		if strings.Contains(s.StreamID, "p_") || s.Language != "en-GB" || s.SampleRate != SampleRate || s.Encoding != "pcm_s16le" {
			t.Fatalf("stream start leaks identity or lacks format: %+v", s)
		}
	}
	perStream := map[string]int{}
	for _, f := range p.frames {
		key, pcm, err := DecodeFrame(f)
		if err != nil || len(pcm) != SampleRate/10*2 {
			t.Fatalf("bad relayed frame: %v", err)
		}
		perStream[key]++
	}
	if perStream[starts[0].StreamID] != 5 || perStream[starts[1].StreamID] != 5 {
		t.Fatalf("frames per stream: %v", perStream)
	}
}

func TestCaptionsOffOrNoProviderDropsAudio(t *testing.T) {
	hub := newFakeHub()
	hub.admit("s1", "p1", "A")
	m := newManager(hub, time.Now)
	var ph float64
	if err := m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph))); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Control("s1", signaling.CaptionControlPayload{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Enabled, but no provider: nothing to send to, no error.
	if err := m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph))); err != nil {
		t.Fatal(err)
	}
	if st := m.State("s1"); !st.Enabled || st.Provider {
		t.Fatalf("state %+v", st)
	}
	p := &fakeProvider{}
	_ = m.AttachProvider(p, ProviderMessage{T: MsgHello, Protocol: 1})
	if _, err := m.Control("s1", signaling.CaptionControlPayload{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	if len(p.frames) != 0 {
		t.Fatalf("audio relayed while captions off: %d frames", len(p.frames))
	}
}

func TestAudioOnlyForAdmittedParticipants(t *testing.T) {
	m, hub, p, clk := setup(t)
	hub.admit("s1", "p_host", "Host")
	_, _ = m.Control("s1", signaling.CaptionControlPayload{Enabled: true})
	var ph float64
	if err := m.Ingest("s1", EncodeFrame("p_waiting", sine100ms(&ph))); err != ErrNotAdmitted {
		t.Fatalf("pending participant audio: %v", err)
	}
	if err := m.Ingest("s2", EncodeFrame("p_host", sine100ms(&ph))); err != nil {
		t.Fatalf("other session (captions off) should drop silently: %v", err)
	}
	hub.admit("s1", "p_g", "Guest")
	clk.advance(100 * time.Millisecond)
	_ = m.Ingest("s1", EncodeFrame("p_g", sine100ms(&ph)))
	streamID := p.starts()[0].StreamID
	hub.remove("s1", "p_g")
	clk.advance(100 * time.Millisecond)
	if err := m.Ingest("s1", EncodeFrame("p_g", sine100ms(&ph))); err != ErrNotAdmitted {
		t.Fatalf("removed participant: %v", err)
	}
	if ends := p.ends(); len(ends) != 1 || ends[0] != streamID {
		t.Fatalf("removed participant's stream not ended: %v", ends)
	}
	// Results for the removed participant are not fanned out.
	m.HandleProviderMessage(p, result(streamID, "a", "hello", true, 0, 100))
	if len(hub.captions["s1"]) != 0 {
		t.Fatal("caption fanned out for a removed participant")
	}
}

func TestRateAndStreamLimits(t *testing.T) {
	m, hub, p, clk := setup(t)
	_, _ = m.Control("s1", signaling.CaptionControlPayload{Enabled: true})
	hub.admit("s1", "p1", "A")
	var ph float64
	// Faster than real time: 100 frames of 100 ms in one instant.
	var limited int
	for i := 0; i < 100; i++ {
		if err := m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph))); err == ErrRateLimited {
			limited++
		}
	}
	if sent := len(p.frames); sent > int(MaxAhead/(100*time.Millisecond))+1 || limited == 0 {
		t.Fatalf("burst not limited: sent %d, limited %d", sent, limited)
	}
	for i := 0; i < MaxStreamsPerSession+2; i++ {
		hub.admit("s1", "q"+string(rune('a'+i)), "x")
	}
	var tooMany int
	for i := 0; i < MaxStreamsPerSession+2; i++ {
		if m.Ingest("s1", EncodeFrame("q"+string(rune('a'+i)), sine100ms(&ph))) == ErrTooManyStreams {
			tooMany++
		}
	}
	if tooMany != 3 { // p1 already holds one of the 16
		t.Fatalf("stream cap: %d refused", tooMany)
	}
	_ = clk
}

func TestGapAndIdleRestartStreams(t *testing.T) {
	m, hub, p, clk := setup(t)
	hub.admit("s1", "p1", "A")
	_, _ = m.Control("s1", signaling.CaptionControlPayload{Enabled: true})
	var ph float64
	_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	clk.advance(MaxGap + time.Second)
	_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	if len(p.starts()) != 2 || len(p.ends()) != 1 {
		t.Fatalf("gap should restart the stream: starts %d ends %d", len(p.starts()), len(p.ends()))
	}
	clk.advance(StreamIdle + time.Second)
	m.reapIdle()
	if len(p.ends()) != 2 {
		t.Fatalf("idle stream not ended: %v", p.ends())
	}
	// A full provider queue restarts the stream instead of skewing timestamps.
	p.full = true
	_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	if len(p.ends()) != 3 {
		t.Fatalf("dropped frame should end the stream: %v", p.ends())
	}
}

func TestResultsBecomeCaptionsWithHostTimesAndStableIDs(t *testing.T) {
	m, hub, p, clk := setup(t)
	hub.admit("s1", "p1", "Ada <Admin>")
	_, _ = m.Control("s1", signaling.CaptionControlPayload{Enabled: true})
	var ph float64
	for i := 0; i < 30; i++ {
		clk.advance(100 * time.Millisecond)
		_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	}
	sid := p.starts()[0].StreamID
	origin := clk.t.Add(-3 * time.Second).UnixMilli()
	m.HandleProviderMessage(p, result(sid, "seg-1", " Welcome", false, 0, 800))
	m.HandleProviderMessage(p, result(sid, "seg-1", "Welcome to the", false, 0, 1200)) // throttled
	clk.advance(PartialInterval)
	m.HandleProviderMessage(p, result(sid, "seg-1", "Welcome to the show\nline2\x00", false, 0, 1500))
	m.HandleProviderMessage(p, result(sid, "seg-1", "Welcome to the show.", true, 0, 2220))
	m.HandleProviderMessage(p, result(sid, "seg-2", "Next", false, 2220, 9_999_999))
	m.HandleProviderMessage(p, result("s999", "x", "stale stream", true, 0, 1))
	m.HandleProviderMessage(&fakeProvider{}, result(sid, "y", "not the provider", true, 0, 1))
	got := hub.captions["s1"]
	if len(got) != 4 {
		t.Fatalf("captions: %+v", got)
	}
	if got[0].Text != "Welcome" || got[1].Text != "Welcome to the show line2" || got[0].ID != got[2].ID || got[3].ID == got[2].ID {
		t.Fatalf("partial/final ids or text: %+v", got)
	}
	if !got[2].IsFinal || got[2].Name != "Ada <Admin>" || got[2].ParticipantID != "p1" {
		t.Fatalf("final attribution: %+v", got[2])
	}
	if got[2].StartMs != origin || got[2].EndMs != origin+2220 {
		t.Fatalf("host times: %d..%d want %d..%d", got[2].StartMs, got[2].EndMs, origin, origin+2220)
	}
	if got[3].EndMs > origin+8000 {
		t.Fatalf("end not clamped to audio sent: %d", got[3].EndMs-origin)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatal("seq not increasing")
		}
	}
	tr, ok := m.Transcript("s1")
	if !ok || len(tr.Segments) != 1 || tr.Segments[0].Text != "Welcome to the show." {
		t.Fatalf("transcript: %+v", tr)
	}
	vtt := tr.VTT()
	want := "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.220\n<v Ada &lt;Admin&gt;>Welcome to the show.\n\n"
	if vtt != want {
		t.Fatalf("vtt:\n%q\nwant\n%q", vtt, want)
	}
}

func TestControlValidationEndAndRetention(t *testing.T) {
	m, hub, p, clk := setup(t)
	hub.admit("s1", "p1", "A")
	if _, err := m.Control("s1", signaling.CaptionControlPayload{Enabled: true, Language: "en_US; rm -rf"}); err != ErrLanguage {
		t.Fatalf("bad language accepted: %v", err)
	}
	st, _ := m.Control("s1", signaling.CaptionControlPayload{Enabled: true})
	if st.Language != "en-US" || !st.Enabled || !st.Provider || st.ProviderName != "Test" {
		t.Fatalf("state %+v", st)
	}
	if states := hub.states["s1"]; len(states) == 0 || !states[len(states)-1].Enabled {
		t.Fatal("state not broadcast")
	}
	var ph float64
	_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	sid := p.starts()[0].StreamID
	m.HandleProviderMessage(p, result(sid, "a", "kept", true, 0, 500))
	m.SessionEnded("s1")
	if len(p.ends()) != 1 {
		t.Fatal("session end did not end streams")
	}
	if _, err := m.Control("s1", signaling.CaptionControlPayload{Enabled: true}); err != ErrSessionEnded {
		t.Fatalf("control after end: %v", err)
	}
	if tr, ok := m.Transcript("s1"); !ok || len(tr.Segments) != 1 {
		t.Fatal("transcript lost at meeting end")
	}
	for i := 0; i < MaxRetainedSessions+5; i++ {
		clk.advance(time.Second)
		id := "old" + string(rune('a'+i))
		_, _ = m.Control(id, signaling.CaptionControlPayload{Enabled: true})
		m.SessionEnded(id)
	}
	if _, ok := m.Transcript("s1"); ok {
		t.Fatal("oldest ended meeting not evicted")
	}
}

func TestSingleProviderAndDetach(t *testing.T) {
	m, hub, p, _ := setup(t)
	hub.admit("s1", "p1", "A")
	if err := m.AttachProvider(&fakeProvider{}, ProviderMessage{}); err != ErrProviderBusy {
		t.Fatalf("second provider: %v", err)
	}
	if p.text[0].T != MsgReady || p.text[0].Protocol != ProviderProtocol {
		t.Fatalf("no ready: %+v", p.text)
	}
	_, _ = m.Control("s1", signaling.CaptionControlPayload{Enabled: true})
	var ph float64
	_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	m.DetachProvider(p)
	if st := m.State("s1"); st.Provider || !st.Enabled {
		t.Fatalf("after detach: %+v", st)
	}
	if c := m.Counts(); c["captionStreams"] != 0 || c["captionProvider"] != 0 {
		t.Fatalf("counts %v", c)
	}
	release, err := m.OpenIngest("s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.OpenIngest("s1"); err != ErrIngestBusy {
		t.Fatalf("second ingest: %v", err)
	}
	release()
	if _, err := m.OpenIngest("s1"); err != nil {
		t.Fatalf("ingest after release: %v", err)
	}
}

func TestProviderHelloValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"t":"hello"}`, `{"t":"hello","protocol":2}`, `not json`, `{"t":"result","protocol":1}`} {
		if _, err := ParseProviderHello([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	h, err := ParseProviderHello([]byte(`{"t":"hello","protocol":1,"name":"Mac\nSpeech","engine":"apple","languages":["en-US","bad lang","fr-FR"]}`))
	if err != nil || h.Name != "Mac Speech" || len(h.Languages) != 2 {
		t.Fatalf("hello %+v %v", h, err)
	}
}

// No audio or caption text ever reaches the logs.
func TestNoAudioOrTextInLogs(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	m, hub, p, clk := setup(t)
	hub.admit("s1", "p1", "Secret Name")
	_, _ = m.Control("s1", signaling.CaptionControlPayload{Enabled: true})
	var ph float64
	for i := 0; i < 5; i++ {
		clk.advance(100 * time.Millisecond)
		_ = m.Ingest("s1", EncodeFrame("p1", sine100ms(&ph)))
	}
	sid := p.starts()[0].StreamID
	m.HandleProviderMessage(p, result(sid, "a", "the launch code is swordfish", true, 0, 400))
	errMsg, _ := json.Marshal(ProviderMessage{T: MsgError, StreamID: sid, Code: "recognizer_failed", Message: "heard swordfish"})
	m.HandleProviderMessage(p, errMsg)
	m.DetachProvider(p)
	for _, secret := range []string{"swordfish", "Secret Name"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain %q:\n%s", secret, logs.String())
		}
	}
}
