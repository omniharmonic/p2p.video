package filespeech

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
)

// fakeConn records daemon→provider messages and lets a test script replies.
type fakeConn struct {
	mu     sync.Mutex
	sent   []Message
	full   bool
	closed bool
	onSend func(Message)
}

func (c *fakeConn) Send(b []byte) bool {
	var m Message
	_ = json.Unmarshal(b, &m)
	c.mu.Lock()
	if c.full {
		c.mu.Unlock()
		return false
	}
	c.sent = append(c.sent, m)
	cb := c.onSend
	c.mu.Unlock()
	if cb != nil {
		go cb(m)
	}
	return true
}
func (c *fakeConn) Close() { c.mu.Lock(); c.closed = true; c.mu.Unlock() }
func (c *fakeConn) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.sent {
		out = append(out, m.T)
	}
	return out
}

func send(t *testing.T, b *Broker, c Conn, m Message) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b.Handle(c, raw)
}

func attach(t *testing.T, state string) (*Broker, *fakeConn) {
	t.Helper()
	b := NewBroker()
	c := &fakeConn{}
	hello, err := ParseHello([]byte(`{"t":"hello","protocol":1,"name":"Fake","engine":"fake-asr","languages":["en-US","bad tag"],"state":"` + state + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Attach(c, hello); err != nil {
		t.Fatal(err)
	}
	return b, c
}

func conf(v float64) *float64 { return &v }

func TestFixtureMessagesDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "macapp", "Tests", "StudioLinkCoreTests", "Fixtures", "transcription-provider-messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	var msgs []Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, m := range msgs {
		types = append(types, m.T)
	}
	if strings.Join(types, ",") != "hello,ready,job,progress,segment,done,cancel,error,status" {
		t.Fatalf("types %v", types)
	}
	if _, err := ParseHello(mustJSON(msgs[0])); err != nil {
		t.Fatal(err)
	}
	if msgs[4].Segment == nil || len(msgs[4].Segment.Words) != 4 || *msgs[4].Segment.Confidence != 0.995 {
		t.Fatalf("segment %+v", msgs[4].Segment)
	}
	if msgs[2].DurationSec != 9.5 || !filepath.IsAbs(msgs[2].Path) {
		t.Fatalf("job %+v", msgs[2])
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestHelloValidation(t *testing.T) {
	for _, bad := range []string{`{"t":"hello","protocol":2}`, `{"t":"result","protocol":1}`, `not json`} {
		if _, err := ParseHello([]byte(bad)); !errors.Is(err, ErrHello) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	m, err := ParseHello([]byte(`{"t":"hello","protocol":1,"name":"A\nB","languages":["en-US","x y"],"state":"weird"}`))
	if err != nil || m.Name != "A B" || len(m.Languages) != 1 || m.State != StateUnavailable {
		t.Fatalf("%+v %v", m, err)
	}
	if m, _ := ParseHello([]byte(`{"t":"hello","protocol":1}`)); m.State != StateReady {
		t.Fatalf("no state means ready: %+v", m)
	}
}

func TestStatusAvailability(t *testing.T) {
	b := NewBroker()
	if ok, code, msg := b.Status().Available(); ok || code != CodeNoProvider || !strings.Contains(msg, "Open p2p.video") {
		t.Fatalf("%v %s %s", ok, code, msg)
	}
	b2, c := attach(t, StateNotAuthorized)
	if ok, code, msg := b2.Status().Available(); ok || code != CodeNotAuthorized || !strings.Contains(msg, "Speech Recognition") {
		t.Fatalf("%v %s %s", ok, code, msg)
	}
	// The provider reports the permission was granted.
	send(t, b2, c, Message{T: MsgStatus, State: StateReady})
	if ok, _, _ := b2.Status().Available(); !ok {
		t.Fatal("status update not applied")
	}
}

func TestSecondProviderRejectedAndReadySent(t *testing.T) {
	b, c := attach(t, StateReady)
	if c.types()[0] != MsgReady {
		t.Fatalf("sent %v", c.types())
	}
	if err := b.Attach(&fakeConn{}, Message{T: MsgHello, Protocol: 1}); !errors.Is(err, ErrProviderBusy) {
		t.Fatal(err)
	}
}

func TestTranscribeCollectsSegmentsAndProgress(t *testing.T) {
	b, c := attach(t, StateReady)
	c.onSend = func(m Message) {
		if m.T != MsgJob {
			return
		}
		send(t, b, c, Message{T: MsgProgress, JobID: m.JobID, ProcessedSec: 2})
		send(t, b, c, Message{T: MsgSegment, JobID: m.JobID, Segment: &Segment{Start: 0.5, End: 1.5, Text: " Hello\tthere ", Confidence: conf(0.9),
			Words: []Word{{Word: "Hello", Start: 0.5, End: 0.9, Confidence: 0.95}, {Word: "there", Start: 1, End: 1.5, Confidence: 1.4}}}})
		send(t, b, c, Message{T: MsgSegment, JobID: m.JobID, Segment: &Segment{Start: 3, End: 3.2, Text: "   "}}) // empty: dropped
		send(t, b, c, Message{T: MsgSegment, JobID: "other", Segment: &Segment{Start: 3, End: 3.2, Text: "stray"}})
		send(t, b, c, Message{T: MsgDone, JobID: m.JobID, Locale: "en-US", ASR: "apple-speechtranscriber"})
	}
	var mu sync.Mutex
	var progress []float64
	res, err := b.Transcribe(context.Background(), Request{Path: "/pkg/audio/Ana.wav", DurationSec: 9.5, Progress: func(s float64) { mu.Lock(); progress = append(progress, s); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != "fake-asr" || res.Locale != "en-US" || res.DurationSec != 9.5 || len(res.Segments) != 1 {
		t.Fatalf("%+v", res)
	}
	s := res.Segments[0]
	if s.Text != "Hello there" || len(s.Words) != 2 || s.Words[1].Confidence != 1 {
		t.Fatalf("segment %+v", s)
	}
	if len(progress) != 1 || progress[0] != 2 {
		t.Fatalf("progress %v", progress)
	}
	job := c.sent[1]
	if job.T != MsgJob || job.Path != "/pkg/audio/Ana.wav" || job.Language != "en-US" || job.DurationSec != 9.5 {
		t.Fatalf("job %+v", job)
	}
}

func TestSilenceIsAnEmptySuccess(t *testing.T) {
	b, c := attach(t, StateReady)
	c.onSend = func(m Message) {
		if m.T == MsgJob {
			send(t, b, c, Message{T: MsgDone, JobID: m.JobID})
		}
	}
	res, err := b.Transcribe(context.Background(), Request{Path: "/s.wav", DurationSec: 30})
	if err != nil || res.Segments == nil || len(res.Segments) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestProviderErrorsAreExplicit(t *testing.T) {
	b, c := attach(t, StateReady)
	c.onSend = func(m Message) {
		if m.T == MsgJob {
			send(t, b, c, Message{T: MsgError, JobID: m.JobID, Code: CodeNotAuthorized})
		}
	}
	_, err := b.Transcribe(context.Background(), Request{Path: "/a.wav"})
	if !IsCode(err, CodeNotAuthorized) || !strings.Contains(err.Error(), "Speech Recognition") {
		t.Fatalf("%v", err)
	}
}

func TestInvalidResultsFailTheJob(t *testing.T) {
	for name, seg := range map[string]*Segment{
		"negative":      {Start: -1, End: 1, Text: "x"},
		"reversed":      {Start: 2, End: 1, Text: "x"},
		"past the file": {Start: 50, End: 51, Text: "x"},
		"bad word":      {Start: 0, End: 1, Text: "x", Words: []Word{{Word: "x", Start: 2, End: 1}}},
	} {
		b, c := attach(t, StateReady)
		c.onSend = func(m Message) {
			if m.T == MsgJob {
				send(t, b, c, Message{T: MsgSegment, JobID: m.JobID, Segment: seg})
			}
		}
		if _, err := b.Transcribe(context.Background(), Request{Path: "/a.wav", DurationSec: 10}); !IsCode(err, "bad_result") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDisconnectFailsRunningJob(t *testing.T) {
	b, c := attach(t, StateReady)
	c.onSend = func(m Message) {
		if m.T == MsgJob {
			b.Detach(c)
		}
	}
	_, err := b.Transcribe(context.Background(), Request{Path: "/a.wav"})
	if !IsCode(err, CodeProviderGone) {
		t.Fatalf("%v", err)
	}
	if _, err := b.Transcribe(context.Background(), Request{Path: "/a.wav"}); !IsCode(err, CodeNoProvider) {
		t.Fatalf("after detach: %v", err)
	}
}

func TestCancelSendsCancelAndWaitsForAck(t *testing.T) {
	b, c := attach(t, StateReady)
	ctx, cancel := context.WithCancel(context.Background())
	c.onSend = func(m Message) {
		switch m.T {
		case MsgJob:
			cancel()
		case MsgCancel:
			send(t, b, c, Message{T: MsgError, JobID: m.JobID, Code: CodeCancelled})
		}
	}
	_, err := b.Transcribe(ctx, Request{Path: "/a.wav"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if got := strings.Join(c.types(), ","); got != "ready,job,cancel" {
		t.Fatalf("sent %s", got)
	}
}

func TestIdleTimeout(t *testing.T) {
	b, c := attach(t, StateReady)
	b.IdleTimeout, b.CancelWait = 50*time.Millisecond, 10*time.Millisecond
	_, err := b.Transcribe(context.Background(), Request{Path: "/a.wav"})
	if !IsCode(err, CodeTimeout) {
		t.Fatalf("%v", err)
	}
	if got := c.types(); got[len(got)-1] != MsgCancel {
		t.Fatalf("sent %v", got)
	}
}

func TestJobsAreSerialized(t *testing.T) {
	b, c := attach(t, StateReady)
	release := make(chan struct{})
	var mu sync.Mutex
	active, maxActive := 0, 0
	c.onSend = func(m Message) {
		if m.T != MsgJob {
			return
		}
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		send(t, b, c, Message{T: MsgDone, JobID: m.JobID})
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = b.Transcribe(context.Background(), Request{Path: "/a.wav"}) }()
	}
	for i := 0; i < 3; i++ {
		time.Sleep(20 * time.Millisecond)
		release <- struct{}{}
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("max concurrent jobs %d", maxActive)
	}
}

func TestRejectsRelativePathAndBadLanguage(t *testing.T) {
	b, _ := attach(t, StateReady)
	if _, err := b.Transcribe(context.Background(), Request{Path: "a.wav"}); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := b.Transcribe(context.Background(), Request{Path: "/a.wav", Language: "en US"}); err == nil {
		t.Fatal("bad language accepted")
	}
}
