// Package filespeech brokers post-take file transcription to the on-device
// recognizer in the p2p.video Mac app, the "transcription provider"
// [SL-P4-3]. The provider connects to the daemon on a loopback WebSocket
// (desktop token, like the caption provider) and the daemon sends it one job
// at a time: an absolute path to a WAV inside a published export package.
// The provider answers with progress, word-timed segments and a done or
// error message.
//
// This package only knows paths and recognizer output. It never imports the
// recording pipeline or live captions (filespeech_guardrail_test.go), and
// recognized text is never logged. The wire protocol is specified in
// docs/integrations/TRANSCRIPTS.md.
package filespeech

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Protocol is the transcription provider protocol version.
const Protocol = 1

// Message types (JSON text frames).
const (
	MsgHello    = "hello"    // P→D first message
	MsgReady    = "ready"    // D→P hello accepted
	MsgStatus   = "status"   // P→D availability changed
	MsgJob      = "job"      // D→P transcribe one file
	MsgCancel   = "cancel"   // D→P stop a job
	MsgProgress = "progress" // P→D seconds of audio processed
	MsgSegment  = "segment"  // P→D one final utterance
	MsgDone     = "done"     // P→D job finished
	MsgError    = "error"    // P→D job failed
)

// Availability states announced by the provider.
const (
	StateReady         = "ready"
	StateNotAuthorized = "not_authorized"
	StateUnavailable   = "unavailable"
)

// Error codes. Providers may send others; unknown codes are kept.
const (
	CodeNotAuthorized   = "not_authorized"
	CodeCancelled       = "cancelled"
	CodeBusy            = "busy"
	CodeNoProvider      = "no_provider"
	CodeProviderGone    = "provider_gone"
	CodeTimeout         = "timeout"
	CodeAudioUnreadable = "audio_unreadable"
)

// Limits on untrusted provider input.
const (
	MaxMessageBytes    = 256 << 10
	MaxSegmentsPerJob  = 50000
	MaxWordsPerSegment = 500
	MaxTextRunes       = 2000
	maxWordRunes       = 64
	maxNameRunes       = 64
	maxLangs           = 64
	maxMessageRunes    = 300
)

// Message is every provider message. Fields not used by a type are omitted.
type Message struct {
	T string `json:"t"`

	Protocol  int      `json:"protocol,omitempty"`
	Name      string   `json:"name,omitempty"`
	Engine    string   `json:"engine,omitempty"`
	Languages []string `json:"languages,omitempty"`
	State     string   `json:"state,omitempty"`
	Message   string   `json:"message,omitempty"`

	JobID        string   `json:"jobId,omitempty"`
	Path         string   `json:"path,omitempty"`
	Language     string   `json:"language,omitempty"`
	DurationSec  float64  `json:"durationSec,omitempty"`
	ProcessedSec float64  `json:"processedSec,omitempty"`
	Segment      *Segment `json:"segment,omitempty"`
	Locale       string   `json:"locale,omitempty"`
	ASR          string   `json:"asr,omitempty"`
	Code         string   `json:"code,omitempty"`
}

// Segment is one utterance, seconds from the start of the file.
type Segment struct {
	Start      float64  `json:"start"`
	End        float64  `json:"end"`
	Text       string   `json:"text"`
	Confidence *float64 `json:"confidence,omitempty"`
	Words      []Word   `json:"words,omitempty"`
}

// Word is one recognized word, seconds from the start of the file.
type Word struct {
	Word       string  `json:"word"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"`
}

// Result is a finished job.
type Result struct {
	Engine      string // provider engine id, e.g. apple-speechtranscriber
	ASR         string
	Locale      string
	DurationSec float64
	Segments    []Segment
}

// Error is a job failure reported by (or about) the provider. Message is
// safe to show to the host; it never contains audio, text or tokens.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// IsCode reports whether err is a filespeech Error with the code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Messages shown when the built-in engine cannot run.
const (
	NoProviderMessage = "Built-in transcription runs in the p2p.video Mac app. Open p2p.video (and keep Settings › Transcripts › Transcribe recordings on this Mac on), then try again."
	GoneMessage       = "The p2p.video app disconnected while transcribing. Reopen it, then retry."
	PermissionMessage = "Speech Recognition is not allowed for p2p.video. Open p2p.video › Settings › Transcripts and choose Allow, or turn it on in System Settings › Privacy & Security › Speech Recognition, then retry."
)

// ErrProviderBusy rejects a second provider connection.
var ErrProviderBusy = errors.New("filespeech: a transcription provider is already connected")

// ErrHello rejects a provider whose first message is not a valid hello.
var ErrHello = errors.New("filespeech: provider must start with a protocol 1 hello")

// Conn is a connected provider socket. Send must not block: a full queue
// returns false.
type Conn interface {
	Send(msg []byte) bool
	Close()
}

// Status describes the built-in engine for the host UI.
type Status struct {
	Connected bool   `json:"connected"`
	State     string `json:"state,omitempty"` // ready | not_authorized | unavailable
	Message   string `json:"message,omitempty"`
	Name      string `json:"name,omitempty"`
	Engine    string `json:"engine,omitempty"`
}

// Available reports whether a job can be sent now, and if not, why.
func (s Status) Available() (bool, string, string) {
	switch {
	case !s.Connected:
		return false, CodeNoProvider, NoProviderMessage
	case s.State == StateReady:
		return true, "", ""
	case s.State == StateNotAuthorized:
		return false, CodeNotAuthorized, firstNonEmpty(s.Message, PermissionMessage)
	default:
		return false, StateUnavailable, firstNonEmpty(s.Message, "On-device transcription is unavailable on this Mac.")
	}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

type provider struct {
	conn  Conn
	hello Message
	state string
	msg   string
}

type job struct {
	id       string
	duration float64
	progress func(processedSec float64)
	segments []Segment
	events   chan struct{} // signalled on every message (idle timer)
	done     chan struct{}
	result   *Result
	err      error
	finished bool
}

// Broker owns the provider connection and the running jobs.
type Broker struct {
	// IdleTimeout fails a job when the provider sends nothing for this long.
	IdleTimeout time.Duration
	// CancelWait bounds how long a cancel waits for the provider's reply.
	CancelWait time.Duration

	mu    sync.Mutex
	prov  *provider
	jobs  map[string]*job
	seq   uint64
	slot  chan struct{} // one job at a time per provider
	onChg []func()
}

// NewBroker returns a broker with production defaults.
func NewBroker() *Broker {
	return &Broker{IdleTimeout: 3 * time.Minute, jobs: map[string]*job{}, slot: make(chan struct{}, 1)}
}

// OnChange registers a callback for provider connect, disconnect and status
// changes (used to refresh capability caches). Callbacks run without locks.
func (b *Broker) OnChange(fn func()) {
	b.mu.Lock()
	b.onChg = append(b.onChg, fn)
	b.mu.Unlock()
}

func (b *Broker) changed() {
	b.mu.Lock()
	fns := append([]func(){}, b.onChg...)
	b.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

var languageRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8}){0,2}$`)

// ValidLanguage accepts a short BCP 47 tag such as "en-US".
func ValidLanguage(tag string) bool { return len(tag) <= 16 && languageRE.MatchString(tag) }

// ParseHello validates a provider's first message.
func ParseHello(raw []byte) (Message, error) {
	var m Message
	if len(raw) > MaxMessageBytes || json.Unmarshal(raw, &m) != nil || m.T != MsgHello || m.Protocol != Protocol {
		return Message{}, ErrHello
	}
	m.Name = cleanText(m.Name, maxNameRunes)
	m.Engine = cleanText(m.Engine, maxNameRunes)
	m.Message = cleanText(m.Message, maxMessageRunes)
	m.State = normalizeState(m.State)
	langs := []string{}
	for _, l := range m.Languages {
		if len(langs) == maxLangs {
			break
		}
		if ValidLanguage(l) {
			langs = append(langs, l)
		}
	}
	m.Languages = langs
	return m, nil
}

func normalizeState(s string) string {
	switch s {
	case StateReady, StateNotAuthorized, StateUnavailable:
		return s
	case "":
		return StateReady // a protocol 1 provider that says nothing is ready
	default:
		return StateUnavailable
	}
}

// Attach registers a provider after a valid hello.
func (b *Broker) Attach(c Conn, hello Message) error {
	b.mu.Lock()
	if b.prov != nil {
		b.mu.Unlock()
		return ErrProviderBusy
	}
	b.prov = &provider{conn: c, hello: hello, state: hello.State, msg: hello.Message}
	b.mu.Unlock()
	ready, _ := json.Marshal(Message{T: MsgReady, Protocol: Protocol})
	c.Send(ready)
	b.changed()
	return nil
}

// Detach removes the provider; its running jobs fail with provider_gone.
func (b *Broker) Detach(c Conn) {
	b.mu.Lock()
	if b.prov == nil || b.prov.conn != c {
		b.mu.Unlock()
		return
	}
	b.prov = nil
	for _, j := range b.jobs {
		b.finishLocked(j, nil, &Error{Code: CodeProviderGone, Message: GoneMessage})
	}
	b.mu.Unlock()
	b.changed()
}

// Close disconnects the provider.
func (b *Broker) Close() {
	b.mu.Lock()
	p := b.prov
	b.mu.Unlock()
	if p != nil {
		p.conn.Close()
		b.Detach(p.conn)
	}
}

// Status reports the provider connection.
func (b *Broker) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.prov == nil {
		return Status{}
	}
	return Status{Connected: true, State: b.prov.state, Message: b.prov.msg, Name: b.prov.hello.Name, Engine: b.prov.hello.Engine}
}

// Handle processes one text message from the provider. Malformed or
// unknown messages are ignored; out-of-range data fails the job.
func (b *Broker) Handle(c Conn, raw []byte) {
	if len(raw) > MaxMessageBytes {
		return
	}
	var m Message
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	b.mu.Lock()
	if b.prov == nil || b.prov.conn != c {
		b.mu.Unlock()
		return
	}
	if m.T == MsgStatus {
		b.prov.state, b.prov.msg = normalizeState(m.State), cleanText(m.Message, maxMessageRunes)
		b.mu.Unlock()
		b.changed()
		return
	}
	j := b.jobs[m.JobID]
	if j == nil || j.finished {
		b.mu.Unlock()
		return
	}
	select {
	case j.events <- struct{}{}:
	default:
	}
	var progress func(float64)
	var processed float64
	switch m.T {
	case MsgProgress:
		if finite(m.ProcessedSec) && m.ProcessedSec >= 0 {
			processed = m.ProcessedSec
			if j.duration > 0 {
				processed = math.Min(processed, j.duration)
			}
			progress = j.progress
		}
	case MsgSegment:
		seg, err := cleanSegment(m.Segment, j.duration)
		switch {
		case err != nil:
			b.finishLocked(j, nil, &Error{Code: "bad_result", Message: "The on-device recognizer sent an invalid result: " + err.Error()})
		case len(j.segments) >= MaxSegmentsPerJob:
			b.finishLocked(j, nil, &Error{Code: "bad_result", Message: "The on-device recognizer sent too many segments."})
		case seg != nil:
			j.segments = append(j.segments, *seg)
		}
	case MsgDone:
		dur := j.duration
		if finite(m.DurationSec) && m.DurationSec > 0 {
			dur = m.DurationSec
		}
		segs := j.segments
		if segs == nil {
			segs = []Segment{}
		}
		b.finishLocked(j, &Result{Engine: b.prov.hello.Engine, ASR: cleanText(m.ASR, maxNameRunes), Locale: cleanLocale(m.Locale), DurationSec: dur, Segments: segs}, nil)
	case MsgError:
		code := cleanText(m.Code, 32)
		if code == "" {
			code = "recognizer_failed"
		}
		msg := cleanText(m.Message, maxMessageRunes)
		if code == CodeNotAuthorized {
			msg = firstNonEmpty(msg, PermissionMessage)
		}
		if msg == "" {
			msg = "On-device transcription failed (" + code + ")."
		}
		b.finishLocked(j, nil, &Error{Code: code, Message: msg})
	}
	b.mu.Unlock()
	if progress != nil {
		progress(processed)
	}
}

func cleanLocale(l string) string {
	if ValidLanguage(l) {
		return l
	}
	return ""
}

func (b *Broker) finishLocked(j *job, r *Result, err error) {
	if j.finished {
		return
	}
	j.finished, j.result, j.err = true, r, err
	close(j.done)
}

// Request is one file to transcribe.
type Request struct {
	Path        string  // absolute path to the audio file
	Language    string  // BCP 47, default en-US
	DurationSec float64 // used for bounds checks and timeouts (0 = unknown)
	// Progress receives seconds of audio processed (may be nil).
	Progress func(processedSec float64)
}

// Transcribe sends one job to the provider and waits for its result. Jobs
// are serialized: a second caller waits for the first (respecting ctx).
// Cancelling ctx sends a cancel to the provider and returns ctx.Err().
func (b *Broker) Transcribe(ctx context.Context, req Request) (*Result, error) {
	if !filepath.IsAbs(req.Path) {
		return nil, fmt.Errorf("filespeech: audio path must be absolute")
	}
	lang := req.Language
	if lang == "" {
		lang = "en-US"
	}
	if !ValidLanguage(lang) {
		return nil, fmt.Errorf("filespeech: invalid language %q", lang)
	}
	if ok, code, msg := b.Status().Available(); !ok {
		return nil, &Error{Code: code, Message: msg}
	}
	select {
	case b.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-b.slot }()

	b.mu.Lock()
	if b.prov == nil {
		b.mu.Unlock()
		return nil, &Error{Code: CodeNoProvider, Message: NoProviderMessage}
	}
	b.seq++
	j := &job{id: "j" + strconv.FormatUint(b.seq, 10), duration: math.Max(0, req.DurationSec), progress: req.Progress,
		events: make(chan struct{}, 1), done: make(chan struct{})}
	b.jobs[j.id] = j
	conn := b.prov.conn
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.jobs, j.id)
		b.mu.Unlock()
	}()

	msg, _ := json.Marshal(Message{T: MsgJob, JobID: j.id, Path: req.Path, Language: lang, DurationSec: round3(j.duration)})
	if !conn.Send(msg) {
		return nil, &Error{Code: CodeProviderGone, Message: GoneMessage}
	}
	idle := b.IdleTimeout
	if idle <= 0 {
		idle = 3 * time.Minute
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case <-j.done:
			return j.result, j.err
		case <-j.events:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			b.cancel(conn, j)
			return nil, &Error{Code: CodeTimeout, Message: "The on-device recognizer stopped responding. Retry, or restart the p2p.video app."}
		case <-ctx.Done():
			b.cancel(conn, j)
			return nil, ctx.Err()
		}
	}
}

// cancel asks the provider to stop and waits briefly for it to acknowledge
// (its error/done), so the next job doesn't find the recognizer busy.
func (b *Broker) cancel(conn Conn, j *job) {
	msg, _ := json.Marshal(Message{T: MsgCancel, JobID: j.id})
	if conn.Send(msg) {
		wait := b.CancelWait
		if wait <= 0 {
			wait = 10 * time.Second
		}
		select {
		case <-j.done:
		case <-time.After(wait):
		}
	}
	b.mu.Lock()
	b.finishLocked(j, nil, context.Canceled)
	b.mu.Unlock()
}

func cleanSegment(s *Segment, duration float64) (*Segment, error) {
	if s == nil {
		return nil, errors.New("missing segment")
	}
	limit := math.Inf(1)
	if duration > 0 {
		limit = duration + 1 // recognizers may round the last range up
	}
	if !finite(s.Start) || !finite(s.End) || s.Start < 0 || s.End < s.Start || s.Start > limit {
		return nil, errors.New("segment times out of range")
	}
	out := &Segment{Start: round3(s.Start), End: round3(math.Min(s.End, limit)), Text: cleanText(s.Text, MaxTextRunes)}
	if s.Confidence != nil {
		if !finite(*s.Confidence) {
			return nil, errors.New("segment confidence out of range")
		}
		c := round3(clamp01(*s.Confidence))
		out.Confidence = &c
	}
	if len(s.Words) > MaxWordsPerSegment {
		return nil, errors.New("too many words in a segment")
	}
	for _, w := range s.Words {
		if !finite(w.Start) || !finite(w.End) || w.Start < 0 || w.End < w.Start || w.Start > limit || !finite(w.Confidence) {
			return nil, errors.New("word times out of range")
		}
		word := cleanText(w.Word, maxWordRunes)
		if word == "" {
			continue
		}
		out.Words = append(out.Words, Word{Word: word, Start: round3(w.Start), End: round3(math.Min(w.End, limit)), Confidence: round3(clamp01(w.Confidence))})
	}
	if out.Text == "" {
		return nil, nil // nothing recognized: dropped, not an error
	}
	return out, nil
}

func finite(v float64) bool     { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func clamp01(v float64) float64 { return math.Min(1, math.Max(0, v)) }
func round3(v float64) float64  { return math.Round(v*1000) / 1000 }

// cleanText makes untrusted recognizer text safe to store: valid UTF-8,
// control characters folded to spaces, trimmed and bounded.
func cleanText(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}
