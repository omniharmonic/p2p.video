// Package livestream publishes a host-composited call to RTMP(S) destinations.
//
// The host's browser composites the call and streams MediaRecorder output
// over a WebSocket; this package pipes those bytes into one ffmpeg process per
// session that re-encodes them to a single FLV stream on its stdout. The
// daemon parses that FLV and publishes it to every destination with its own
// RTMP client (rtmp.go), one independent publisher per destination with its
// own queue and reconnect backoff (publisher.go) [SL-P3-3]. It is
// deliberately independent of the recording pipeline: it never reads
// recording, upload or post-processing state, and recording never reads from
// it.
//
// Stream keys live only in daemon memory for the life of a stream. They are
// never logged, persisted, returned or passed to a child process (so they
// never appear in `ps`): status exposes scheme://host only, and all ffmpeg
// and server diagnostics pass through a Redactor first.
package livestream

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
)

// Limits on a start request.
const (
	MaxDestinations = 4
	maxNameLen      = 64
	maxKeyLen       = 512
	stderrTailLines = 12
)

// States reported in Status.State.
const (
	StateWaitingIngest = "waiting_ingest" // started, browser not yet connected
	StateStarting      = "starting"       // ffmpeg running, no output yet
	StateLive          = "live"
	StateReconnecting  = "reconnecting" // ingest dropped, within grace period
	StateStopped       = "stopped"
	StateFailed        = "failed"
)

// Encoder health thresholds behind Status.Warnings.
const (
	slowSpeed      = 0.95            // below this the encoder is behind real time
	slowFor        = 3 * time.Second // ...for at least this long
	dropWarnFrames = 15              // dropped in one RateWindow (half a second at 30 fps)
)

var (
	ErrInvalidRequest = errors.New("invalid livestream request")
	ErrAlreadyActive  = errors.New("a livestream is already active for this session")
	ErrNotActive      = errors.New("no active livestream for this session")
	ErrIngestBusy     = errors.New("an ingest connection is already active")
	ErrUnavailable    = errors.New("livestreaming is unavailable on this host")
)

// Destination is one RTMP(S) target. Key is a secret.
type Destination struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Key  string `json:"key"`
}

// publishURL is the URL+key form, used only to build redaction patterns.
func (d Destination) publishURL() string {
	if d.Key == "" {
		return d.URL
	}
	return strings.TrimRight(d.URL, "/") + "/" + d.Key
}

// StartRequest configures a stream.
type StartRequest struct {
	Destinations []Destination `json:"destinations"`
	Preset       string        `json:"preset"`
}

// Progress is the latest ffmpeg -progress snapshot.
type Progress struct {
	Frames      int64   `json:"frames"`
	FPS         float64 `json:"fps"`
	BitrateKbps float64 `json:"bitrateKbps"` // ffmpeg's own figure (0 when it reports N/A)
	// TotalSizeBytes is ffmpeg's total_size: bytes written so far, 0 when it
	// reports N/A [SL-P3-2].
	TotalSizeBytes int64   `json:"totalSizeBytes"`
	OutTimeSec     float64 `json:"outTimeSec"`
	DupFrames      int64   `json:"dupFrames"`
	DropFrames     int64   `json:"dropFrames"`
	Speed          float64 `json:"speed"`
}

// DestinationStatus is the redacted, client-visible state of one target.
type DestinationStatus struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	// State is pending | connecting | live | reconnecting | failed | stopped.
	State string `json:"state"`
	// Error is the latest failure reason, redacted (no key, no URL path).
	Error string `json:"error,omitempty"`
	// Reconnects counts every retry scheduled since the stream started.
	Reconnects int `json:"reconnects"`
	// Attempt/MaxAttempts/RetryInSec describe the pending retry while
	// reconnecting.
	Attempt     int     `json:"attempt,omitempty"`
	MaxAttempts int     `json:"maxAttempts,omitempty"`
	RetryInSec  float64 `json:"retryInSec,omitempty"`
	// Kbps is what this destination is actually sending over RateWindow.
	Kbps     float64 `json:"kbps"`
	BytesOut int64   `json:"bytesOut"`
	// BacklogMs is media queued for this destination but not yet sent;
	// DroppedFrames counts video frames skipped because the backlog passed
	// Options.MaxBacklog; Behind is true for RateWindow after a skip.
	BacklogMs     int64 `json:"backlogMs"`
	DroppedFrames int64 `json:"droppedFrames"`
	Behind        bool  `json:"behind"`
}

// Status is safe to return to the host UI and to log.
type Status struct {
	State           string     `json:"state"`
	Preset          string     `json:"preset"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	BytesIn         int64      `json:"bytesIn"`
	IngestKbps      float64    `json:"ingestKbps"` // browser→daemon throughput over RateWindow
	IngestConnected bool       `json:"ingestConnected"`
	// OutputKbps is the encoder's FLV output rate over RateWindow, measured
	// by the daemon as it reads ffmpeg's stdout [SL-P3-2, SL-P3-3].
	OutputKbps float64 `json:"outputKbps"`
	// BitrateKbps is the figure to show: OutputKbps when known, else
	// IngestKbps. BitrateSource says which ("output", "ingest", or "" when
	// nothing is flowing).
	BitrateKbps   float64             `json:"bitrateKbps"`
	BitrateSource string              `json:"bitrateSource"`
	Restarts      int                 `json:"restarts"`
	Progress      Progress            `json:"progress"`
	Destinations  []DestinationStatus `json:"destinations"`
	// Warnings are short, redacted health notes for the panel: encoder
	// slower than real time, dropped frames, destinations reconnecting,
	// failed or behind.
	Warnings  []string `json:"warnings"`
	LastError string   `json:"lastError,omitempty"`
}

// Process is a running ffmpeg encoder. Tests substitute a fake.
type Process struct {
	Stdin    io.WriteCloser
	Stdout   io.Reader // FLV
	Progress io.Reader // -progress key=value lines (fd 3)
	Stderr   io.Reader
	Wait     func() error
	Kill     func() error
	Pid      int // 0 for fakes
}

// Launcher starts ffmpeg with argv (argv excludes the program name). The
// encoder argv never contains a destination or key.
type Launcher func(argv []string) (*Process, error)

// ExecLauncher runs the ffmpeg binary at path directly (no shell). File
// descriptor 3 is a pipe for -progress.
func ExecLauncher(path string) Launcher {
	return func(argv []string) (*Process, error) {
		cmd := exec.Command(path, argv...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("ffmpeg stdin: %w", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("ffmpeg stdout: %w", err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("ffmpeg stderr: %w", err)
		}
		progR, progW, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("ffmpeg progress pipe: %w", err)
		}
		cmd.ExtraFiles = []*os.File{progW}
		if err := cmd.Start(); err != nil {
			_ = progR.Close()
			_ = progW.Close()
			return nil, fmt.Errorf("start ffmpeg: %w", err)
		}
		_ = progW.Close() // the child holds its own copy
		wait := func() error {
			err := cmd.Wait()
			_ = progR.Close()
			return err
		}
		return &Process{Stdin: stdin, Stdout: stdout, Progress: progR, Stderr: stderr, Wait: wait, Kill: cmd.Process.Kill, Pid: cmd.Process.Pid}, nil
	}
}

// Options tune a Manager.
type Options struct {
	// Preflight reports whether this host can encode (VideoToolbox).
	Preflight func() error
	// IngestWait is how long a started stream waits for its first ingest.
	IngestWait time.Duration
	// Grace is how long a dropped ingest may reconnect before auto-stop.
	Grace time.Duration
	// ExitWait bounds a graceful ffmpeg shutdown before it is killed.
	ExitWait time.Duration
	Logger   *slog.Logger
	// Now is the clock for rate windows; nil is time.Now (tests).
	Now func() time.Time

	// TLS configures rtmps:// connections. nil means the system trust store
	// with hostname verification, as in production. Only tests (and the
	// daemon's test-only --livestream-test-ca flag) add RootCAs here.
	TLS *tls.Config
	// Reconnect policy per destination: retry n waits BackoffBase·2^(n-1)
	// capped at BackoffMax; after MaxAttempts consecutive failures the
	// destination is failed. A connection that stayed live for StableAfter
	// resets the count.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	MaxAttempts int
	StableAfter time.Duration
	// MaxBacklog is how far a slow destination may fall behind before it
	// skips to the next keyframe.
	MaxBacklog time.Duration
	// WriteTimeout bounds one RTMP write; a stalled connection is dropped
	// and retried.
	WriteTimeout time.Duration
}

// Manager owns at most one livestream per session.
type Manager struct {
	launch Launcher
	opts   Options
	dial   dialFunc // RTMP publisher; tests substitute a fake
	mu     sync.Mutex
	byID   map[string]*stream
}

// NewManager returns a Manager that launches ffmpeg via launch.
func NewManager(launch Launcher, opts Options) *Manager {
	if opts.IngestWait <= 0 {
		opts.IngestWait = 60 * time.Second
	}
	if opts.Grace <= 0 {
		opts.Grace = 15 * time.Second
	}
	if opts.ExitWait <= 0 {
		opts.ExitWait = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = time.Second
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 30 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 8
	}
	if opts.StableAfter <= 0 {
		opts.StableAfter = 30 * time.Second
	}
	if opts.MaxBacklog <= 0 {
		opts.MaxBacklog = 4 * time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 10 * time.Second
	}
	return &Manager{launch: launch, opts: opts, dial: rtmpDialer(opts.TLS, opts.WriteTimeout), byID: map[string]*stream{}}
}

type stream struct {
	sessionID string
	preset    ffmpeg.LivestreamPreset
	dests     []Destination // holds keys; cleared on finish
	redact    *Redactor
	status    Status
	timer     *time.Timer
	proc      *Process
	abort     func() // closes the active ingest connection
	ingesting bool
	finished  bool
	stderr    []string
	pubs      []*publisher
	epoch     int // encoder runs; each new FLV header starts one
	flvBytes  int64
	// Sliding-window rates [SL-P3-2]: bytes received from the browser, and
	// FLV bytes read from the encoder.
	now    func() time.Time
	ingest *rateMeter
	output *rateMeter
	drops  *rateMeter // cumulative dropped frames, for the warning window
	// slowSince is when encoder speed first fell below slowSpeed (zero when
	// it is keeping up).
	slowSince time.Time
}

// Start validates and registers a stream; ffmpeg launches when ingest
// connects, and each destination connects once the encoder has media.
func (m *Manager) Start(sessionID string, req StartRequest) (Status, error) {
	preset, err := ffmpeg.LivestreamPresetByName(req.Preset)
	if err != nil {
		return Status{}, fmt.Errorf("%w: unknown preset", ErrInvalidRequest)
	}
	if err := validateDestinations(req.Destinations); err != nil {
		return Status{}, err
	}
	if m.opts.Preflight != nil {
		if err := m.opts.Preflight(); err != nil {
			return Status{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.byID[sessionID]; ok && !s.finished {
		return Status{}, ErrAlreadyActive
	}
	dests := append([]Destination(nil), req.Destinations...)
	s := &stream{sessionID: sessionID, preset: preset, dests: dests, redact: NewRedactor(dests),
		now: m.opts.Now, ingest: newRateMeter(RateWindow), output: newRateMeter(RateWindow), drops: newRateMeter(RateWindow)}
	now := time.Now().UTC()
	s.status = Status{State: StateWaitingIngest, Preset: preset.Name, StartedAt: &now}
	cfg := &publisherConfig{dial: m.dial, backoffBase: m.opts.BackoffBase, backoffMax: m.opts.BackoffMax, maxAttempts: m.opts.MaxAttempts,
		stableAfter: m.opts.StableAfter, maxBacklog: m.opts.MaxBacklog, now: m.opts.Now, logger: m.opts.Logger, sessionID: sessionID,
		redact: s.redact, onFailed: func() { go m.checkAllFailed(s) }}
	for i, d := range dests {
		t, _ := parseTarget(d) // validated above
		p := newPublisher(i, d, t, cfg)
		s.pubs = append(s.pubs, p)
		p.start()
	}
	s.timer = time.AfterFunc(m.opts.IngestWait, func() { m.expire(s, "no compositor connected") })
	m.byID[sessionID] = s
	m.opts.Logger.Info("livestream started", "session", sessionID, "preset", preset.Name, "destinations", len(dests))
	return s.snapshot(), nil
}

func validateDestinations(dests []Destination) error {
	if len(dests) == 0 || len(dests) > MaxDestinations {
		return fmt.Errorf("%w: between 1 and %d destinations required", ErrInvalidRequest, MaxDestinations)
	}
	for i, d := range dests {
		if d.Name == "" || len(d.Name) > maxNameLen || strings.IndexFunc(d.Name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
			return fmt.Errorf("%w: destination %d needs a short printable name", ErrInvalidRequest, i+1)
		}
		if len(d.Key) > maxKeyLen {
			return fmt.Errorf("%w: destination %d stream key is too long", ErrInvalidRequest, i+1)
		}
		// Never echo the URL or key in the error.
		if _, err := parseTarget(d); err != nil {
			return fmt.Errorf("%w: destination %d: %w", ErrInvalidRequest, i+1, err)
		}
	}
	return nil
}

// Status returns the current or last stream status for a session.
func (m *Manager) Status(sessionID string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[sessionID]
	if !ok {
		return Status{State: "idle", Destinations: []DestinationStatus{}, Warnings: []string{}}, false
	}
	return s.snapshot(), true
}

// Stop ends a session's stream: closes ingest, lets ffmpeg flush and
// disconnects every destination.
func (m *Manager) Stop(sessionID string) (Status, error) {
	m.mu.Lock()
	s, ok := m.byID[sessionID]
	if !ok || s.finished {
		m.mu.Unlock()
		return Status{}, ErrNotActive
	}
	m.finishLocked(s, StateStopped, "")
	st := s.snapshot()
	m.mu.Unlock()
	m.opts.Logger.Info("livestream stopped", "session", sessionID, "bytesIn", st.BytesIn)
	return st, nil
}

// StopAll ends every stream (daemon shutdown).
func (m *Manager) StopAll() {
	m.mu.Lock()
	for _, s := range m.byID {
		if !s.finished {
			m.finishLocked(s, StateStopped, "")
		}
	}
	m.mu.Unlock()
}

// AnyActive reports whether any stream has not finished (desktop app status).
func (m *Manager) AnyActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.byID {
		if !s.finished {
			return true
		}
	}
	return false
}

// finishLocked marks the stream terminal, drops its keys, disconnects the
// destinations and unblocks ingest. The ingest goroutine closes ffmpeg's
// stdin and reaps the process.
func (m *Manager) finishLocked(s *stream, state, reason string) {
	s.finished = true
	s.status.State = state
	if reason != "" {
		s.status.LastError = s.redact.Redact(reason)
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	for i := range s.dests {
		s.dests[i].Key = ""
	}
	s.dests = nil
	for _, p := range s.pubs {
		p.close()
	}
	if s.abort != nil {
		s.abort()
	}
}

// checkAllFailed ends the stream once every destination has given up.
func (m *Manager) checkAllFailed(s *stream) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.finished {
		return
	}
	for _, p := range s.pubs {
		if p.status().State != DestFailed {
			return
		}
	}
	m.finishLocked(s, StateFailed, "every destination failed")
	m.opts.Logger.Warn("livestream failed: every destination failed", "session", s.sessionID)
}

// Wait blocks until every destination of a session's stream has
// disconnected (tests and shutdown). It reports false on timeout.
func (m *Manager) Wait(sessionID string, timeout time.Duration) bool {
	m.mu.Lock()
	var pubs []*publisher
	if s, ok := m.byID[sessionID]; ok {
		pubs = append(pubs, s.pubs...)
	}
	m.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for _, p := range pubs {
		if !p.wait(time.Until(deadline)) {
			return false
		}
	}
	return true
}

func (m *Manager) expire(s *stream, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.finished || s.ingesting {
		return
	}
	m.finishLocked(s, StateStopped, reason)
	m.opts.Logger.Info("livestream auto-stopped", "session", s.sessionID, "reason", reason)
}

// Ingest pipes compositor frames from next into ffmpeg until next fails, the
// stream is stopped, or ffmpeg exits. abort must unblock a pending next (for
// example by closing the WebSocket). A dropped ingest leaves the stream
// reconnecting for the grace period; a new Ingest relaunches ffmpeg, because
// a new MediaRecorder starts a new container. Destinations stay connected
// across the relaunch and continue with the new encoder run.
func (m *Manager) Ingest(sessionID string, next func() ([]byte, error), abort func()) error {
	m.mu.Lock()
	s, ok := m.byID[sessionID]
	if !ok || s.finished {
		m.mu.Unlock()
		return ErrNotActive
	}
	if s.ingesting {
		m.mu.Unlock()
		return ErrIngestBusy
	}
	argv, err := ffmpeg.BuildLivestreamEncodeArgs(s.preset)
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	proc, err := m.launch(argv)
	if err != nil {
		m.finishLocked(s, StateFailed, err.Error())
		m.mu.Unlock()
		m.opts.Logger.Error("livestream ffmpeg launch failed", "session", sessionID, "err", s.redact.Redact(err.Error()))
		return fmt.Errorf("%w: ffmpeg did not start", ErrUnavailable)
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.proc != nil {
		s.status.Restarts++
	}
	s.proc, s.abort, s.ingesting = proc, abort, true
	s.status.State, s.status.IngestConnected = StateStarting, true
	// A relaunched ffmpeg starts a new FLV stream (and new counters).
	s.output.reset()
	s.drops.reset()
	s.slowSince = time.Time{}
	s.ingest.observe(s.now(), s.status.BytesIn)
	m.mu.Unlock()

	var readers sync.WaitGroup
	readers.Add(3)
	go func() { defer readers.Done(); m.readFLV(s, proc.Stdout) }()
	go func() { defer readers.Done(); m.readProgress(s, proc.Progress) }()
	go func() { defer readers.Done(); m.readStderr(s, proc.Stderr) }()

	exited := make(chan error, 1)
	var copyErr error
	for {
		buf, err := next()
		if err != nil {
			break
		}
		if len(buf) == 0 {
			continue
		}
		if _, err := proc.Stdin.Write(buf); err != nil {
			copyErr = errors.New("ffmpeg stopped accepting media")
			break
		}
		m.mu.Lock()
		s.status.BytesIn += int64(len(buf))
		s.ingest.observe(s.now(), s.status.BytesIn)
		m.mu.Unlock()
	}
	_ = proc.Stdin.Close()
	go func() { exited <- proc.Wait() }()
	var waitErr error
	select {
	case waitErr = <-exited:
	case <-time.After(m.opts.ExitWait):
		_ = proc.Kill()
		waitErr = <-exited
	}
	readers.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	s.ingesting, s.abort = false, nil
	s.status.IngestConnected = false
	s.ingest.reset()
	s.output.reset()
	s.drops.reset()
	if s.finished {
		return nil
	}
	reason := "compositor disconnected"
	if copyErr != nil || waitErr != nil {
		reason = "ffmpeg exited: " + s.lastStderr()
		if copyErr != nil && waitErr == nil {
			reason = copyErr.Error()
		}
		s.status.LastError = s.redact.Redact(reason)
	}
	s.status.State = StateReconnecting
	s.timer = time.AfterFunc(m.opts.Grace, func() { m.expire(s, reason) })
	m.opts.Logger.Warn("livestream ingest ended", "session", sessionID, "reason", s.redact.Redact(reason))
	return nil
}

func (s *stream) snapshot() Status {
	st := s.status
	st.IngestKbps, st.OutputKbps, st.BitrateKbps, st.BitrateSource = 0, 0, 0, ""
	st.Warnings = []string{}
	if s.ingesting && !s.finished {
		now := s.now()
		st.IngestKbps = s.ingest.kbps(now)
		st.OutputKbps = s.output.kbps(now)
		if !s.slowSince.IsZero() && now.Sub(s.slowSince) >= slowFor {
			st.Warnings = append(st.Warnings, fmt.Sprintf("The encoder is running at %.2f× real time, so viewers will see stalls. Choose 720p30 or close other apps.", st.Progress.Speed))
		}
		if n := s.drops.delta(now); n >= dropWarnFrames {
			st.Warnings = append(st.Warnings, fmt.Sprintf("The encoder dropped %d frames in the last %d s.", n, int(RateWindow.Seconds())))
		}
	}
	st.BitrateKbps, st.BitrateSource = displayBitrate(st.OutputKbps, st.IngestKbps)
	st.Destinations = make([]DestinationStatus, 0, len(s.pubs))
	for _, p := range s.pubs {
		d := p.status()
		st.Destinations = append(st.Destinations, d)
		if !s.finished {
			st.Warnings = append(st.Warnings, d.warnings()...)
		}
	}
	return st
}

// displayBitrate picks the bitrate the panel shows: the encoder's measured
// output rate, otherwise (before any output) what the browser is sending.
func displayBitrate(outputKbps, ingestKbps float64) (float64, string) {
	switch {
	case outputKbps > 0:
		return outputKbps, "output"
	case ingestKbps > 0:
		return ingestKbps, "ingest"
	}
	return 0, ""
}

func (s *stream) lastStderr() string {
	if len(s.stderr) == 0 {
		return "no diagnostics"
	}
	return s.stderr[len(s.stderr)-1]
}

func (m *Manager) readProgress(s *stream, r io.Reader) {
	if r == nil {
		return
	}
	sc := bufio.NewScanner(r)
	var cur Progress
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		applyProgress(&cur, k, strings.TrimSpace(v))
		if k != "progress" {
			continue
		}
		m.mu.Lock()
		now := s.now()
		s.status.Progress = cur
		s.drops.observe(now, cur.DropFrames)
		if cur.Speed > 0 && cur.Speed < slowSpeed && cur.OutTimeSec > 3 {
			if s.slowSince.IsZero() {
				s.slowSince = now
			}
		} else {
			s.slowSince = time.Time{}
		}
		if !s.finished && s.ingesting && cur.Frames > 0 {
			s.status.State = StateLive
		}
		m.mu.Unlock()
	}
	_, _ = io.Copy(io.Discard, r)
}

func applyProgress(p *Progress, k, v string) {
	num := func(s string) float64 {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0
		}
		return f
	}
	switch k {
	case "frame":
		p.Frames = int64(num(v))
	case "fps":
		p.FPS = num(v)
	case "bitrate":
		p.BitrateKbps = num(strings.TrimSuffix(v, "kbits/s"))
	case "total_size":
		p.TotalSizeBytes = int64(num(v)) // "N/A" -> 0
	case "out_time_us", "out_time_ms": // both are microseconds in ffmpeg
		p.OutTimeSec = num(v) / 1e6
	case "dup_frames":
		p.DupFrames = int64(num(v))
	case "drop_frames":
		p.DropFrames = int64(num(v))
	case "speed":
		p.Speed = num(strings.TrimSuffix(v, "x"))
	}
}

func (m *Manager) readStderr(s *stream, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		line := s.redact.Redact(strings.TrimSpace(sc.Text()))
		if line == "" {
			continue
		}
		m.mu.Lock()
		s.stderr = append(s.stderr, line)
		if len(s.stderr) > stderrTailLines {
			s.stderr = s.stderr[len(s.stderr)-stderrTailLines:]
		}
		m.mu.Unlock()
		m.opts.Logger.Debug("livestream ffmpeg", "session", s.sessionID, "line", line)
	}
}

// readFLV parses the encoder's stdout and fans tags out to every
// destination. It never blocks on a destination.
func (m *Manager) readFLV(s *stream, r io.Reader) {
	br := bufio.NewReaderSize(r, 256*1024)
	defer func() { _, _ = io.Copy(io.Discard, br) }()
	if err := readFLVHeader(br); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			m.opts.Logger.Warn("livestream encoder output unreadable", "session", s.sessionID, "err", err)
		}
		return
	}
	m.mu.Lock()
	s.epoch++
	in := streamInit{epoch: s.epoch}
	pubs := append([]*publisher(nil), s.pubs...)
	m.mu.Unlock()
	announced := false
	announce := func() {
		for _, p := range pubs {
			p.setInit(in)
		}
		announced = true
	}
	for {
		tag, err := readFLVTag(br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				m.opts.Logger.Warn("livestream encoder output unreadable", "session", s.sessionID, "err", err)
			}
			return
		}
		m.mu.Lock()
		s.flvBytes += int64(len(tag.data)) + 15
		s.output.observe(s.now(), s.flvBytes)
		m.mu.Unlock()
		switch {
		case tag.kind == tagScript:
			if !announced {
				in.meta = tag.data
			}
		case tag.isSequenceHeader():
			if tag.kind == tagVideo {
				in.videoSeq = tag.data
			} else {
				in.audioSeq = tag.data
			}
			if announced { // codec parameters changed mid-stream
				announce()
			}
		case tag.kind == tagVideo || tag.kind == tagAudio:
			if !announced {
				announce()
			}
			for _, p := range pubs {
				p.push(tag)
			}
		}
	}
}

// ExecPreflight checks an ffmpeg runner for VideoToolbox H.264. ffmpeg only
// encodes to a pipe, so it needs no network protocols: the daemon publishes.
func ExecPreflight(r *ffmpeg.Runner) func() error {
	return func() error {
		if r == nil {
			return errors.New("ffmpeg not found")
		}
		if !r.HasVideoToolbox {
			return errors.New("h264_videotoolbox encoder unavailable")
		}
		return nil
	}
}
