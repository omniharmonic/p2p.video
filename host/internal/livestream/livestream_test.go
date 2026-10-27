package livestream

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeFFmpeg behaves like the encoder reading stdin: it consumes input until
// EOF, then exits. The test drives FLV output, progress and stderr through
// the pipe writers.
type fakeFFmpeg struct {
	argv     []string
	stdin    bytes.Buffer
	mu       sync.Mutex
	flv      *io.PipeWriter
	progress *io.PipeWriter
	stderr   *io.PipeWriter
	done     chan struct{}
	exitErr  error
	killed   bool
}

func (f *fakeFFmpeg) received() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.stdin.Bytes()...)
}

type fakeLauncher struct {
	mu    sync.Mutex
	procs []*fakeFFmpeg
	ready chan *fakeFFmpeg
}

func newFakeLauncher() *fakeLauncher { return &fakeLauncher{ready: make(chan *fakeFFmpeg, 8)} }

func (l *fakeLauncher) launch(argv []string) (*Process, error) {
	f := &fakeFFmpeg{argv: argv, done: make(chan struct{})}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	progR, progW := io.Pipe()
	errR, errW := io.Pipe()
	f.flv, f.progress, f.stderr = outW, progW, errW
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := inR.Read(buf)
			f.mu.Lock()
			f.stdin.Write(buf[:n])
			f.mu.Unlock()
			if err != nil {
				break
			}
		}
		_ = outW.Close()
		_ = progW.Close()
		_ = errW.Close()
		close(f.done)
	}()
	proc := &Process{
		Stdin: inW, Stdout: outR, Progress: progR, Stderr: errR,
		Wait: func() error { <-f.done; return f.exitErr },
		Kill: func() error { f.killed = true; _ = inR.Close(); return nil },
	}
	l.mu.Lock()
	l.procs = append(l.procs, f)
	l.mu.Unlock()
	l.ready <- f
	return proc, nil
}

// frameSource feeds frames to Ingest until closed/aborted.
type frameSource struct {
	ch     chan []byte
	closed chan struct{}
	once   sync.Once
}

func newFrameSource() *frameSource {
	return &frameSource{ch: make(chan []byte, 16), closed: make(chan struct{})}
}
func (f *frameSource) next() ([]byte, error) {
	select {
	case b := <-f.ch:
		return b, nil
	case <-f.closed:
		return nil, io.EOF
	}
}
func (f *frameSource) abort() { f.once.Do(func() { close(f.closed) }) }

// --- FLV fixtures -----------------------------------------------------------

func flvHeader() []byte {
	return []byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}
}

func flvTagBytes(kind byte, ts uint32, data []byte) []byte {
	b := []byte{kind, byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data)), byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24), 0, 0, 0}
	b = append(b, data...)
	return binary.BigEndian.AppendUint32(b, uint32(11+len(data)))
}

var (
	fxMeta     = amfEncode("onMetaData", amfObj{"width": 1280.0})
	fxVideoSeq = []byte{0x17, 0, 0, 0, 0, 1, 0x64}
	fxAudioSeq = []byte{0xaf, 0, 0x11, 0x90}
)

func fxKey(n byte) []byte   { return []byte{0x17, 1, 0, 0, 0, n} }
func fxInter(n byte) []byte { return []byte{0x27, 1, 0, 0, 0, n} }
func fxAudio(n byte) []byte { return []byte{0xaf, 1, n} }

// flvStart is an encoder run's preamble: header, metadata, both sequence
// headers.
func flvStart() []byte {
	b := flvHeader()
	b = append(b, flvTagBytes(tagScript, 0, fxMeta)...)
	b = append(b, flvTagBytes(tagVideo, 0, fxVideoSeq)...)
	return append(b, flvTagBytes(tagAudio, 0, fxAudioSeq)...)
}

// --- fake RTMP connections ----------------------------------------------------

type sentTag struct {
	kind byte
	ts   uint32
	data []byte
}

type fakeConn struct {
	mu       sync.Mutex
	tags     []sentTag
	failAt   int // fail the Nth write (1-based); 0 never
	writes   int
	block    chan struct{}
	done     chan struct{}
	once     sync.Once
	err      error
	graceful bool
}

func newFakeConn() *fakeConn { return &fakeConn{done: make(chan struct{})} }

func (c *fakeConn) writeTag(kind byte, ts uint32, data []byte) error {
	if c.block != nil {
		select {
		case <-c.block:
		case <-c.done:
			return errors.New("closed")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	if c.failAt > 0 && c.writes >= c.failAt {
		return errors.New("write tcp: broken pipe")
	}
	c.tags = append(c.tags, sentTag{kind, ts, append([]byte(nil), data...)})
	return nil
}
func (c *fakeConn) Done() <-chan struct{} { return c.done }
func (c *fakeConn) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *fakeConn) Close()                { c.once.Do(func() { close(c.done) }) }
func (c *fakeConn) closeGracefully()      { c.mu.Lock(); c.graceful = true; c.mu.Unlock(); c.Close() }
func (c *fakeConn) sent() []sentTag {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sentTag(nil), c.tags...)
}

// fakeNet hands out scripted connections per destination address.
type fakeNet struct {
	mu      sync.Mutex
	dials   map[string]int
	refuse  map[string]int // refuse the first N dials
	conns   map[string][]*fakeConn
	prepare func(addr string, n int, c *fakeConn)
	seen    []rtmpTarget
}

func newFakeNet() *fakeNet {
	return &fakeNet{dials: map[string]int{}, refuse: map[string]int{}, conns: map[string][]*fakeConn{}}
}

func (n *fakeNet) dial(ctx context.Context, t rtmpTarget) (mediaConn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dials[t.addr]++
	n.seen = append(n.seen, t)
	if n.refuse[t.addr] > 0 {
		n.refuse[t.addr]--
		return nil, errors.New("connect: dial tcp " + t.addr + ": connection refused; stream " + t.stream)
	}
	c := newFakeConn()
	if n.prepare != nil {
		n.prepare(t.addr, len(n.conns[t.addr])+1, c)
	}
	n.conns[t.addr] = append(n.conns[t.addr], c)
	return c, nil
}

func (n *fakeNet) conn(addr string, i int) *fakeConn {
	n.mu.Lock()
	defer n.mu.Unlock()
	if i >= len(n.conns[addr]) {
		return nil
	}
	return n.conns[addr][i]
}

func (n *fakeNet) count(addr string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.conns[addr])
}

const testKey = "abcd-SECRET-key-1234"

const (
	addrYT     = "a.rtmp.youtube.com:1935"
	addrCustom = "custom.example.test:443"
)

func testRequest() StartRequest {
	return StartRequest{Preset: "720p30", Destinations: []Destination{
		{Name: "YouTube", URL: "rtmp://a.rtmp.youtube.com/live2", Key: testKey},
		{Name: "Custom", URL: "rtmps://custom.example.test:443/app/", Key: "other-secret-9"},
	}}
}

func newTestManager(t *testing.T, l *fakeLauncher, logs io.Writer) (*Manager, *fakeNet) {
	t.Helper()
	m := NewManager(l.launch, Options{Grace: 150 * time.Millisecond, IngestWait: 300 * time.Millisecond, ExitWait: time.Second,
		BackoffBase: 10 * time.Millisecond, BackoffMax: 40 * time.Millisecond, MaxAttempts: 3, MaxBacklog: 2 * time.Second,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	fn := newFakeNet()
	m.dial = fn.dial
	t.Cleanup(m.StopAll)
	return m, fn
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func destState(m *Manager, sess string, i int) DestinationStatus {
	s, _ := m.Status(sess)
	if i >= len(s.Destinations) {
		return DestinationStatus{}
	}
	return s.Destinations[i]
}

func TestLifecyclePipesBytesPublishesAndKeepsKeysOutOfArgv(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	m, fn := newTestManager(t, l, &logs)
	st, err := m.Start("s1", testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateWaitingIngest || len(st.Destinations) != 2 || st.Destinations[0].Server != "rtmp://a.rtmp.youtube.com" || st.Destinations[0].State != DestPending {
		t.Fatalf("unexpected start status %+v", st)
	}
	if _, err := m.Start("s1", testRequest()); !errors.Is(err, ErrAlreadyActive) {
		t.Fatalf("second start: %v", err)
	}

	src := newFrameSource()
	ingestDone := make(chan error, 1)
	go func() { ingestDone <- m.Ingest("s1", src.next, src.abort) }()
	f := <-l.ready
	joined := strings.Join(f.argv, " ")
	if !strings.Contains(joined, "h264_videotoolbox") || strings.Contains(joined, testKey) || strings.Contains(joined, "other-secret") || strings.Contains(joined, "rtmp") {
		t.Fatalf("encoder argv must carry no destination or key: %q", f.argv)
	}
	if err := m.Ingest("s1", src.next, src.abort); !errors.Is(err, ErrIngestBusy) {
		t.Fatalf("concurrent ingest: %v", err)
	}
	src.ch <- []byte("EBML-header")
	src.ch <- []byte("cluster-1")
	waitFor(t, "bytes", func() bool { return string(f.received()) == "EBML-headercluster-1" })
	// Nothing connects before the encoder has media.
	if fn.count(addrYT) != 0 {
		t.Fatal("destination dialed before media existed")
	}

	_, _ = f.flv.Write(flvStart())
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 100, fxKey(1)))
	waitFor(t, "both connected", func() bool { return fn.count(addrYT) == 1 && fn.count(addrCustom) == 1 })
	// Connected after the first keyframe: they start at the next one.
	media := append(flvTagBytes(tagAudio, 2000, fxAudio(1)), flvTagBytes(tagVideo, 2033, fxKey(2))...)
	media = append(media, flvTagBytes(tagAudio, 2050, fxAudio(2))...)
	media = append(media, flvTagBytes(tagVideo, 2066, fxInter(3))...)
	_, _ = f.flv.Write(media)
	_, _ = io.WriteString(f.progress, "frame=90\nfps=30.0\nbitrate=N/A\nout_time_us=3000000\ndup_frames=1\ndrop_frames=2\nspeed=1.01x\nprogress=continue\n")
	waitFor(t, "live", func() bool {
		s, _ := m.Status("s1")
		return s.State == StateLive && s.Destinations[0].State == DestLive && s.Destinations[1].State == DestLive
	})
	for _, addr := range []string{addrYT, addrCustom} {
		waitFor(t, "all media sent", func() bool { return len(fn.conn(addr, 0).sent()) >= 6 })
		got := fn.conn(addr, 0).sent()
		if len(got) != 6 {
			t.Fatalf("%s: sent %d tags: %+v", addr, len(got), got)
		}
		// metadata, sequence headers, then the keyframe re-based to 0.
		if got[0].kind != tagScript || !bytes.Equal(got[1].data, fxVideoSeq) || !bytes.Equal(got[2].data, fxAudioSeq) {
			t.Fatalf("%s: preamble %+v", addr, got[:3])
		}
		if !bytes.Equal(got[3].data, fxKey(2)) || got[3].ts != 0 || got[4].kind != tagAudio || got[4].ts != 17 || got[5].ts != 33 {
			t.Fatalf("%s: media %+v", addr, got[3:])
		}
	}
	fn.mu.Lock()
	seen := append([]rtmpTarget(nil), fn.seen...)
	fn.mu.Unlock()
	for _, tg := range seen {
		if tg.addr == addrYT && (tg.app != "live2" || tg.stream != testKey || tg.secure || tg.tcURL != "rtmp://a.rtmp.youtube.com/live2") {
			t.Fatalf("youtube target %+v", tg)
		}
		if tg.addr == addrCustom && (!tg.secure || tg.app != "app" || tg.host != "custom.example.test") {
			t.Fatalf("custom target %+v", tg)
		}
	}
	s, _ := m.Status("s1")
	if s.BytesIn != int64(len("EBML-headercluster-1")) || s.Progress.Frames != 90 || s.Progress.DropFrames != 2 || s.Progress.Speed != 1.01 || !s.IngestConnected {
		t.Fatalf("status %+v", s)
	}
	if s.Destinations[0].BytesOut == 0 || len(s.Warnings) != 0 {
		t.Fatalf("destination status %+v", s)
	}

	if _, err := m.Stop("s1"); err != nil {
		t.Fatal(err)
	}
	if err := <-ingestDone; err != nil {
		t.Fatal(err)
	}
	<-f.done
	if !m.Wait("s1", time.Second) {
		t.Fatal("publishers still running")
	}
	if c := fn.conn(addrYT, 0); !c.graceful {
		t.Fatal("stop did not close the RTMP connection gracefully")
	}
	s, _ = m.Status("s1")
	if s.State != StateStopped || s.IngestConnected || s.Destinations[0].State != DestStopped {
		t.Fatalf("after stop %+v", s)
	}
	if _, err := m.Stop("s1"); !errors.Is(err, ErrNotActive) {
		t.Fatalf("double stop: %v", err)
	}
	if strings.Contains(logs.String(), "SECRET") || strings.Contains(logs.String(), "other-secret") {
		t.Fatalf("stream key leaked into logs:\n%s", logs.String())
	}
	// A finished stream can be replaced.
	if _, err := m.Start("s1", testRequest()); err != nil {
		t.Fatal(err)
	}
}

// One destination's connection drops: it alone reconnects with backoff,
// with a redacted reason, while the other keeps streaming [SL-P3-3].
func TestDestinationReconnectsIndependently(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	m, fn := newTestManager(t, l, &logs)
	m.opts.MaxAttempts = 50
	m.opts.BackoffBase, m.opts.BackoffMax = 30*time.Millisecond, 30*time.Millisecond
	fn.refuse[addrCustom] = 0
	fn.prepare = func(addr string, n int, c *fakeConn) {
		if addr == addrCustom && n == 1 {
			c.failAt = 6 // preamble (3) + 2 media tags, then the network breaks
		}
	}
	if _, err := m.Start("r1", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	go func() { _ = m.Ingest("r1", src.next, src.abort) }()
	f := <-l.ready
	_, _ = f.flv.Write(flvStart())
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 0, fxKey(0)))
	waitFor(t, "connected", func() bool { return fn.count(addrYT) == 1 && fn.count(addrCustom) == 1 })
	fn.mu.Lock()
	fn.refuse[addrCustom] = 2 // the next two dials are refused
	fn.mu.Unlock()
	ts := uint32(2000)
	write := func(key bool) {
		data := fxInter(byte(ts))
		if key {
			data = fxKey(byte(ts))
		}
		_, _ = f.flv.Write(flvTagBytes(tagVideo, ts, data))
		ts += 33
	}
	for i := 0; i < 4; i++ {
		write(i == 0)
	}
	waitFor(t, "custom reconnecting", func() bool { return destState(m, "r1", 1).State == DestReconnecting })
	d := destState(m, "r1", 1)
	if d.Error == "" || strings.Contains(d.Error, "other-secret") || d.Attempt < 1 || d.MaxAttempts != 50 {
		t.Fatalf("reconnecting status %+v", d)
	}
	s, _ := m.Status("r1")
	if !strings.Contains(strings.Join(s.Warnings, "|"), "Custom: reconnecting") {
		t.Fatalf("warnings %q", s.Warnings)
	}
	if destState(m, "r1", 0).State != DestLive {
		t.Fatal("healthy destination disturbed")
	}
	// Keep the stream going until the third dial succeeds, then send a GOP.
	waitFor(t, "redial", func() bool { return fn.count(addrCustom) == 2 })
	write(true)
	write(false)
	waitFor(t, "custom live again", func() bool { return destState(m, "r1", 1).State == DestLive })
	d = destState(m, "r1", 1)
	if d.Reconnects < 3 || d.Error != "" {
		t.Fatalf("after reconnect %+v", d)
	}
	got := fn.conn(addrCustom, 1).sent()
	if len(got) < 4 || got[0].kind != tagScript || !bytes.Equal(got[1].data, fxVideoSeq) || !got[3].isKey() || got[3].ts != 0 {
		t.Fatalf("reconnected stream must restart with headers and a keyframe at 0: %+v", got)
	}
	lastTag := fxInter(byte(ts - 33))
	waitFor(t, "healthy destination drained", func() bool {
		h := fn.conn(addrYT, 0).sent()
		return len(h) > 0 && bytes.Equal(h[len(h)-1].data, lastTag)
	})
	healthy := fn.conn(addrYT, 0).sent()
	if fn.count(addrYT) != 1 || len(healthy) < 3+4+2 {
		t.Fatalf("healthy destination: %d conns, %d tags", fn.count(addrYT), len(healthy))
	}
	for i := 4; i < len(healthy); i++ {
		if healthy[i].ts < healthy[i-1].ts {
			t.Fatalf("timestamps went backwards: %+v", healthy)
		}
	}
	if strings.Contains(logs.String(), "other-secret") {
		t.Fatal("key in logs")
	}
}

func (s sentTag) isKey() bool { return flvTag{kind: s.kind, data: s.data}.isKeyframe() }

// A destination that keeps refusing gives up after MaxAttempts; when every
// destination has failed the stream ends as failed.
func TestDestinationsFailAfterMaxAttempts(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	m, fn := newTestManager(t, l, &logs)
	fn.refuse[addrYT], fn.refuse[addrCustom] = 100, 100
	if _, err := m.Start("f1", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	done := make(chan error, 1)
	go func() { done <- m.Ingest("f1", src.next, src.abort) }()
	f := <-l.ready
	_, _ = f.flv.Write(flvStart())
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 0, fxKey(0)))
	waitFor(t, "stream failed", func() bool { s, _ := m.Status("f1"); return s.State == StateFailed })
	<-done
	s, _ := m.Status("f1")
	if s.LastError != "every destination failed" || s.Destinations[0].State != DestFailed || s.Destinations[1].State != DestFailed {
		t.Fatalf("status %+v", s)
	}
	fn.mu.Lock()
	dials := fn.dials[addrYT]
	fn.mu.Unlock()
	if dials != 4 { // first try + 3 retries
		t.Fatalf("dials %d", dials)
	}
	for _, d := range s.Destinations {
		if strings.Contains(d.Error, testKey) || strings.Contains(d.Error, "other-secret") || !strings.Contains(d.Error, "connection refused") {
			t.Fatalf("error not redacted or missing: %q", d.Error)
		}
	}
	if strings.Contains(logs.String(), testKey) {
		t.Fatal("key in logs")
	}
}

// A stalled destination never blocks the encoder or the others: its queue
// overflows, it skips to the next keyframe and reports that it is behind.
func TestStalledDestinationSkipsWithoutBlockingOthers(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	m, fn := newTestManager(t, l, &logs)
	release := make(chan struct{})
	fn.prepare = func(addr string, n int, c *fakeConn) {
		if addr == addrCustom {
			c.block = release
		}
	}
	if _, err := m.Start("b1", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	go func() { _ = m.Ingest("b1", src.next, src.abort) }()
	f := <-l.ready
	_, _ = f.flv.Write(flvStart())
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 0, fxKey(0)))
	waitFor(t, "connected", func() bool { return fn.count(addrYT) == 1 && fn.count(addrCustom) == 1 })
	// 5 s of video (a keyframe every second) arrives while Custom is stuck.
	for i := 0; i <= 150; i++ {
		ts := uint32(1000 + i*33)
		data := fxInter(byte(i))
		if i%30 == 0 {
			data = fxKey(byte(i))
		}
		_, _ = f.flv.Write(flvTagBytes(tagVideo, ts, data))
		time.Sleep(time.Millisecond) // arrives at 30x real time, not all at once
	}
	waitFor(t, "healthy destination got everything", func() bool {
		s := fn.conn(addrYT, 0).sent()
		return len(s) > 0 && bytes.Equal(s[len(s)-1].data, fxKey(150))
	})
	if h := destState(m, "b1", 0); h.DroppedFrames != 0 || h.Behind {
		t.Fatalf("healthy destination skipped: %+v", h)
	}
	d := destState(m, "b1", 1)
	if d.DroppedFrames == 0 || !d.Behind {
		t.Fatalf("stalled destination did not skip: %+v", d)
	}
	s, _ := m.Status("b1")
	if !strings.Contains(strings.Join(s.Warnings, "|"), "Custom: the upload can't keep up") {
		t.Fatalf("warnings %q", s.Warnings)
	}
	close(release)
	waitFor(t, "stalled destination resumes", func() bool { return destState(m, "b1", 1).State == DestLive })
}

// The browser reconnects (new MediaRecorder → new encoder run): RTMP stays
// up, the sequence headers are sent again and timestamps keep increasing.
func TestEncoderRestartKeepsDestinationsConnected(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	m, fn := newTestManager(t, l, &logs)
	m.opts.Grace = 5 * time.Second
	if _, err := m.Start("e1", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	done := make(chan error, 1)
	go func() { done <- m.Ingest("e1", src.next, src.abort) }()
	f := <-l.ready
	_, _ = f.flv.Write(flvStart())
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 0, fxKey(0)))
	waitFor(t, "connected", func() bool { return fn.count(addrYT) == 1 })
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 5000, fxKey(1)))
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 5033, fxInter(2)))
	lastIs := func(want []byte) func() bool {
		return func() bool { s := fn.conn(addrYT, 0).sent(); return len(s) > 0 && bytes.Equal(s[len(s)-1].data, want) }
	}
	waitFor(t, "media", lastIs(fxInter(2)))
	first := len(fn.conn(addrYT, 0).sent())
	src.abort()
	<-done

	src2 := newFrameSource()
	go func() { done <- m.Ingest("e1", src2.next, src2.abort) }()
	f2 := <-l.ready
	_, _ = f2.flv.Write(flvStart())
	_, _ = f2.flv.Write(flvTagBytes(tagVideo, 0, fxKey(3)))
	waitFor(t, "second run", lastIs(fxKey(3)))
	got := fn.conn(addrYT, 0).sent()
	if fn.count(addrYT) != 1 {
		t.Fatal("destination reconnected on an encoder restart")
	}
	if len(got) != first+4 || got[first].kind != tagScript || !bytes.Equal(got[first+1].data, fxVideoSeq) || got[first+3].ts <= got[first-1].ts {
		t.Fatalf("second run %+v", got)
	}
	src2.abort()
	<-done
}

func TestIngestDropReconnectsWithinGraceThenAutoStops(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	m, _ := newTestManager(t, l, &logs)
	if _, err := m.Start("s2", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	done := make(chan error, 1)
	go func() { done <- m.Ingest("s2", src.next, src.abort) }()
	<-l.ready
	src.abort() // browser dropped
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s, _ := m.Status("s2"); s.State != StateReconnecting {
		t.Fatalf("want reconnecting, got %+v", s)
	}
	// Reconnect within grace relaunches the encoder.
	src2 := newFrameSource()
	go func() { done <- m.Ingest("s2", src2.next, src2.abort) }()
	<-l.ready
	src2.abort()
	<-done
	if s, _ := m.Status("s2"); s.Restarts != 1 {
		t.Fatalf("restarts %+v", s)
	}
	waitFor(t, "auto-stop", func() bool { s, _ := m.Status("s2"); return s.State == StateStopped })
	if err := m.Ingest("s2", src2.next, src2.abort); !errors.Is(err, ErrNotActive) {
		t.Fatalf("ingest after auto-stop: %v", err)
	}
	if s, _ := m.Status("s2"); s.LastError != "compositor disconnected" {
		t.Fatalf("reason %+v", s)
	}
}

func TestNoIngestAutoStops(t *testing.T) {
	var logs lockedBuffer
	m, _ := newTestManager(t, newFakeLauncher(), &logs)
	if _, err := m.Start("s3", testRequest()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ingest timeout", func() bool { s, _ := m.Status("s3"); return s.State == StateStopped })
}

func TestFFmpegExitWhileIngestingIsReportedRedacted(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	m, _ := newTestManager(t, l, &logs)
	if _, err := m.Start("s4", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	done := make(chan error, 1)
	go func() { done <- m.Ingest("s4", src.next, src.abort) }()
	f := <-l.ready
	f.exitErr = errors.New("exit status 1")
	_, _ = io.WriteString(f.stderr, "Something mentioned rtmp://a.rtmp.youtube.com/live2/"+testKey+": I/O error\n")
	waitFor(t, "stderr", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.byID["s4"].stderr) > 0
	})
	src.abort()
	<-done
	s, _ := m.Status("s4")
	if s.State != StateReconnecting || !strings.Contains(s.LastError, "****") || strings.Contains(s.LastError, testKey) || strings.Contains(s.LastError, "live2") {
		t.Fatalf("status %+v", s)
	}
}

// Encoder health: slower than real time for 3 s, or many dropped frames,
// shows a warning.
func TestEncoderHealthWarnings(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	clk := &fakeClock{t: time.Unix(1000, 0)}
	m, _ := newTestManager(t, l, &logs)
	m.opts.Now = clk.now
	if _, err := m.Start("h1", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	go func() { _ = m.Ingest("h1", src.next, src.abort) }()
	f := <-l.ready
	report := func(frames, drops int, speed string) {
		_, _ = io.WriteString(f.progress, "frame="+strconv.Itoa(frames)+"\nout_time_us="+strconv.Itoa(frames*33333)+"\ndrop_frames="+strconv.Itoa(drops)+"\nspeed="+speed+"\nprogress=continue\n")
		waitFor(t, "progress", func() bool { s, _ := m.Status("h1"); return s.Progress.Frames == int64(frames) })
	}
	report(150, 0, "1.00x")
	if s, _ := m.Status("h1"); len(s.Warnings) != 0 {
		t.Fatalf("healthy encoder warned: %q", s.Warnings)
	}
	report(160, 0, "0.80x")
	clk.add(2 * time.Second)
	if s, _ := m.Status("h1"); len(s.Warnings) != 0 {
		t.Fatalf("warned before 3 s: %q", s.Warnings)
	}
	clk.add(1500 * time.Millisecond)
	report(170, 20, "0.81x")
	s, _ := m.Status("h1")
	w := strings.Join(s.Warnings, "|")
	if !strings.Contains(w, "0.81× real time") || !strings.Contains(w, "dropped 20 frames") {
		t.Fatalf("warnings %q", s.Warnings)
	}
	clk.add(time.Second)
	report(200, 20, "1.00x")
	clk.add(6 * time.Second)
	report(380, 20, "1.00x")
	if s, _ := m.Status("h1"); len(s.Warnings) != 0 {
		t.Fatalf("warnings did not clear: %q", s.Warnings)
	}
}

func TestStartValidation(t *testing.T) {
	m := NewManager(newFakeLauncher().launch, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	bad := []StartRequest{
		{},
		{Preset: "8k", Destinations: testRequest().Destinations},
		{Destinations: []Destination{{Name: "", URL: "rtmp://h/app", Key: "k"}}},
		{Destinations: []Destination{{Name: "x", URL: "http://h/app", Key: "k"}}},
		{Destinations: []Destination{{Name: "x", URL: "rtmp://h/app", Key: "k ey-with-space"}}},
		{Destinations: []Destination{{Name: "x", URL: "rtmp://h/app", Key: strings.Repeat("k", 600)}}},
		{Destinations: []Destination{{Name: "x", URL: "rtmp://h", Key: "k"}}},
		{Destinations: make([]Destination, MaxDestinations+1)},
	}
	for i, req := range bad {
		_, err := m.Start("v", req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("case %d accepted: %v", i, err)
		}
		if err != nil && strings.Contains(err.Error(), "with-space") {
			t.Errorf("case %d echoed the key: %v", i, err)
		}
	}
	unavailable := NewManager(newFakeLauncher().launch, Options{Preflight: func() error { return errors.New("no videotoolbox") }})
	if _, err := unavailable.Start("v", testRequest()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("preflight not enforced: %v", err)
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor([]Destination{
		{Name: "a", URL: "rtmp://live.twitch.tv/app", Key: "live_123/abc+def"},
		{Name: "b", URL: "rtmp://custom.test/app/inline-secret?token=zzz"},
	})
	in := "rtmp://live.twitch.tv/app/live_123/abc+def failed; key live_123%2Fabc%2Bdef; also rtmp://custom.test/app/inline-secret?token=zzz and token=zzz"
	out := r.Redact(in)
	for _, leak := range []string{"live_123", "abc+def", "inline-secret", "zzz"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q leaked in %q", leak, out)
		}
	}
	if ServerLabel("rtmps://a.rtmps.youtube.com:443/live2/KEY") != "rtmps://a.rtmps.youtube.com:443" {
		t.Fatal("server label leaks path")
	}
}

func TestDescribeErr(t *testing.T) {
	for err, want := range map[error]string{
		io.EOF:                  "the server closed the connection",
		syscall.ECONNRESET:      "the connection was reset",
		syscall.ECONNREFUSED:    "connection refused",
		errors.New("TLS: x509"): "TLS: x509",
	} {
		if got := describeErr(err); got != want {
			t.Errorf("%v: %q", err, got)
		}
	}
	if got := describeErr(nil); got != "the server closed the connection" {
		t.Errorf("nil: %q", got)
	}
	if got := describeErr(context.DeadlineExceeded); !strings.HasPrefix(got, "timed out") {
		t.Errorf("deadline: %q", got)
	}
}

func TestBackoffSchedule(t *testing.T) {
	c := &publisherConfig{backoffBase: time.Second, backoffMax: 30 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if got := c.backoff(i + 1); got != w {
			t.Fatalf("retry %d: %v want %v", i+1, got, w)
		}
	}
	if c.backoff(200) != 30*time.Second {
		t.Fatal("huge attempt count overflowed")
	}
}
