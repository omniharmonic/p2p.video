package captions

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
)

// Broadcaster is the signaling hub as the manager uses it.
type Broadcaster interface {
	BroadcastCaption(sessionID string, p signaling.CaptionPayload)
	BroadcastCaptionState(sessionID string, p signaling.CaptionStatePayload)
	AdmittedParticipant(sessionID, participantID string) (name string, ok bool)
	CaptionSessions() []string
}

// Provider is a connected caption provider socket. Sends must not block: a
// full queue returns false.
type Provider interface {
	SendText(msg []byte) bool
	SendBinary(frame []byte) bool
	Close()
}

// Tunables. Exported values are documented in CAPTIONS.md.
const (
	MaxStreamsPerSession = 16
	// MaxAhead bounds how far a stream's audio may run ahead of wall time
	// (a sender pushing faster than real time is rate limited).
	MaxAhead = 2 * time.Second
	// MaxGap restarts a stream whose audio fell this far behind wall time
	// (a pause in the tap), so provider timestamps stay aligned.
	MaxGap = 2 * time.Second
	// StreamIdle ends a stream that has received no audio for this long.
	StreamIdle = 5 * time.Second
	// PartialInterval throttles partial results per stream.
	PartialInterval = 150 * time.Millisecond
	// MaxFinalsPerSession bounds a meeting's retained transcript.
	MaxFinalsPerSession = 20000
	// MaxRetainedSessions bounds how many ended meetings keep a transcript.
	MaxRetainedSessions = 20
)

var (
	ErrProviderBusy   = errors.New("captions: a caption provider is already connected")
	ErrIngestBusy     = errors.New("captions: this meeting already has a caption audio feed")
	ErrNotAdmitted    = errors.New("captions: audio for a participant who is not admitted")
	ErrTooManyStreams = errors.New("captions: too many caption streams in this meeting")
	ErrRateLimited    = errors.New("captions: audio arriving faster than real time")
	ErrSessionEnded   = errors.New("this meeting has ended")
	ErrLanguage       = errors.New("unsupported captions language")
)

type session struct {
	id       string
	enabled  bool
	language string
	ingest   bool
	ended    bool
	endedAt  time.Time
	seq      uint64
	finals   []signaling.CaptionPayload
	streams  map[string]*stream // participantID -> active stream
}

type stream struct {
	id            string
	sessionID     string
	participantID string
	origin        time.Time // host time of the stream's first sample
	samples       int64
	lastFrame     time.Time
	lastPartial   time.Time
	windowStart   time.Time
	windowCount   int
	segments      map[string]string // provider segment ID -> caption ID
	segmentN      int
}

type providerState struct {
	sink      Provider
	name      string
	engine    string
	languages []string
}

// Manager owns caption state for every meeting. Its lock is never held while
// calling the Broadcaster, so the hub may call into it from any context.
type Manager struct {
	out       Broadcaster
	now       func() time.Time
	mu        sync.Mutex
	sessions  map[string]*session
	streams   map[string]*stream // stream ID -> stream
	streamSeq uint64
	provider  *providerState
	dropped   uint64
	stop      chan struct{}
	closeOnce sync.Once
}

// NewManager starts a manager and its idle-stream reaper.
func NewManager(out Broadcaster) *Manager {
	m := newManager(out, time.Now)
	go m.reapLoop()
	return m
}

func newManager(out Broadcaster, now func() time.Time) *Manager {
	return &Manager{out: out, now: now, sessions: map[string]*session{}, streams: map[string]*stream{}, stop: make(chan struct{})}
}

// Close stops the reaper and disconnects the provider.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.stop)
		m.mu.Lock()
		p := m.provider
		m.provider = nil
		m.mu.Unlock()
		if p != nil {
			p.sink.Close()
		}
	})
}

func (m *Manager) sessionLocked(id string) *session {
	s := m.sessions[id]
	if s == nil {
		s = &session{id: id, language: signaling.CaptionDefaultLang, streams: map[string]*stream{}}
		m.sessions[id] = s
	}
	return s
}

func (m *Manager) stateLocked(id string) signaling.CaptionStatePayload {
	st := signaling.CaptionStatePayload{Language: signaling.CaptionDefaultLang, Provider: m.provider != nil}
	if m.provider != nil {
		st.ProviderName = m.provider.name
	}
	if s := m.sessions[id]; s != nil {
		st.Enabled = s.enabled && !s.ended
		st.Language = s.language
	}
	return st
}

// State implements signaling.CaptionController.
func (m *Manager) State(sessionID string) signaling.CaptionStatePayload {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked(sessionID)
}

// Control implements signaling.CaptionController: the host turns captions on
// or off and may change the language. Everyone in the meeting, including the
// waiting room, is told the new state.
func (m *Manager) Control(sessionID string, p signaling.CaptionControlPayload) (signaling.CaptionStatePayload, error) {
	if p.Language != "" && !ValidLanguage(p.Language) {
		return signaling.CaptionStatePayload{}, ErrLanguage
	}
	m.mu.Lock()
	s := m.sessionLocked(sessionID)
	if s.ended {
		m.mu.Unlock()
		return signaling.CaptionStatePayload{}, ErrSessionEnded
	}
	if p.Language != "" && p.Language != s.language {
		s.language = p.Language
		m.endSessionStreamsLocked(s)
	}
	if !p.Enabled {
		m.endSessionStreamsLocked(s)
	}
	s.enabled = p.Enabled
	st := m.stateLocked(sessionID)
	m.mu.Unlock()
	m.out.BroadcastCaptionState(sessionID, st)
	return st, nil
}

// SessionEnded implements signaling.CaptionController. Recognition stops; the
// final transcript stays available to the host until it is evicted.
func (m *Manager) SessionEnded(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[sessionID]
	if s == nil {
		return
	}
	m.endSessionStreamsLocked(s)
	s.enabled, s.ended, s.endedAt = false, true, m.now()
	m.evictLocked()
}

// evictLocked keeps at most MaxRetainedSessions ended meetings.
func (m *Manager) evictLocked() {
	for {
		var oldest *session
		n := 0
		for _, s := range m.sessions {
			if !s.ended {
				continue
			}
			n++
			if oldest == nil || s.endedAt.Before(oldest.endedAt) {
				oldest = s
			}
		}
		if n <= MaxRetainedSessions {
			return
		}
		delete(m.sessions, oldest.id)
	}
}

// AttachProvider registers the (single) caption provider after a valid hello.
func (m *Manager) AttachProvider(p Provider, hello ProviderMessage) error {
	m.mu.Lock()
	if m.provider != nil {
		m.mu.Unlock()
		return ErrProviderBusy
	}
	m.provider = &providerState{sink: p, name: hello.Name, engine: hello.Engine, languages: hello.Languages}
	m.mu.Unlock()
	ready, _ := json.Marshal(ProviderMessage{T: MsgReady, Protocol: ProviderProtocol})
	p.SendText(ready)
	slog.Info("Caption provider connected", "engine", hello.Engine, "languages", len(hello.Languages))
	m.broadcastAllStates()
	return nil
}

// DetachProvider forgets p (if it is the current provider) and its streams.
func (m *Manager) DetachProvider(p Provider) {
	m.mu.Lock()
	if m.provider == nil || m.provider.sink != p {
		m.mu.Unlock()
		return
	}
	m.provider = nil
	for id, st := range m.streams {
		delete(m.streams, id)
		if s := m.sessions[st.sessionID]; s != nil && s.streams[st.participantID] == st {
			delete(s.streams, st.participantID)
		}
	}
	m.mu.Unlock()
	slog.Info("Caption provider disconnected")
	m.broadcastAllStates()
}

func (m *Manager) broadcastAllStates() {
	for _, sid := range m.out.CaptionSessions() {
		m.out.BroadcastCaptionState(sid, m.State(sid))
	}
}

// OpenIngest claims a meeting's single caption audio feed.
func (m *Manager) OpenIngest(sessionID string) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessionLocked(sessionID)
	if s.ended {
		return nil, ErrSessionEnded
	}
	if s.ingest {
		return nil, ErrIngestBusy
	}
	s.ingest = true
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			s.ingest = false
			m.endSessionStreamsLocked(s)
		})
	}, nil
}

// Ingest routes one binary audio frame from the host's browser. Frames are
// dropped silently while captions are off or no provider is connected; an
// error is returned only for frames that must never be relayed.
func (m *Manager) Ingest(sessionID string, frame []byte) error {
	participantID, pcm, err := DecodeFrame(frame)
	if err != nil {
		return err
	}
	// Ask the hub before taking our lock (lock order: never hold m.mu while
	// calling out).
	_, admitted := m.out.AdmittedParticipant(sessionID, participantID)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[sessionID]
	if s == nil || !s.enabled || s.ended || m.provider == nil {
		return nil
	}
	st := s.streams[participantID]
	if !admitted {
		if st != nil {
			m.endStreamLocked(st)
		}
		return ErrNotAdmitted
	}
	now := m.now()
	n := int64(len(pcm) / 2)
	if st != nil && now.Sub(st.origin.Add(samplesDuration(st.samples))) > MaxGap {
		m.endStreamLocked(st) // the tap paused; restart to keep timing aligned
		st = nil
	}
	if st == nil {
		if len(s.streams) >= MaxStreamsPerSession {
			return ErrTooManyStreams
		}
		if st = m.startStreamLocked(s, participantID, now.Add(-samplesDuration(n))); st == nil {
			m.dropped++
			return nil
		}
	}
	if st.origin.Add(samplesDuration(st.samples + n)).After(now.Add(MaxAhead)) {
		m.dropped++
		return ErrRateLimited
	}
	if !m.provider.sink.SendBinary(EncodeFrame(st.id, pcm)) {
		// A lost frame would shift every later timestamp: restart instead.
		m.dropped++
		m.endStreamLocked(st)
		return nil
	}
	st.samples += n
	st.lastFrame = now
	return nil
}

func (m *Manager) startStreamLocked(s *session, participantID string, origin time.Time) *stream {
	m.streamSeq++
	st := &stream{
		id:            "s" + strconv.FormatUint(m.streamSeq, 10),
		sessionID:     s.id,
		participantID: participantID,
		origin:        origin,
		lastFrame:     origin,
		segments:      map[string]string{},
	}
	msg, _ := json.Marshal(ProviderMessage{T: MsgStreamStart, StreamID: st.id, Language: s.language, SampleRate: SampleRate, Encoding: "pcm_s16le"})
	if !m.provider.sink.SendText(msg) {
		return nil
	}
	m.streams[st.id] = st
	s.streams[participantID] = st
	return st
}

func (m *Manager) endStreamLocked(st *stream) {
	delete(m.streams, st.id)
	if s := m.sessions[st.sessionID]; s != nil && s.streams[st.participantID] == st {
		delete(s.streams, st.participantID)
	}
	if m.provider != nil {
		msg, _ := json.Marshal(ProviderMessage{T: MsgStreamEnd, StreamID: st.id})
		m.provider.sink.SendText(msg)
	}
}

func (m *Manager) endSessionStreamsLocked(s *session) {
	for _, st := range s.streams {
		m.endStreamLocked(st)
	}
}

func (m *Manager) reapLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.reapIdle()
		}
	}
}

// reapIdle ends streams that stopped receiving audio (a participant left or
// was muted at the source).
func (m *Manager) reapIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, st := range m.streams {
		if now.Sub(st.lastFrame) > StreamIdle {
			m.endStreamLocked(st)
		}
	}
}

// HandleProviderMessage processes one text message from provider p.
func (m *Manager) HandleProviderMessage(p Provider, raw []byte) {
	if len(raw) > maxProviderMsg {
		return
	}
	var msg ProviderMessage
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	switch msg.T {
	case MsgResult:
		m.handleResult(p, msg)
	case MsgError:
		m.mu.Lock()
		if m.provider != nil && m.provider.sink == p {
			if st := m.streams[msg.StreamID]; st != nil {
				m.endStreamLocked(st) // restarts on the next frame
			}
		}
		m.mu.Unlock()
		// The provider's message is not logged: it may quote speech.
		slog.Warn("Caption provider stream error", "code", cleanText(msg.Code, 32))
	}
}

func (m *Manager) handleResult(p Provider, msg ProviderMessage) {
	text := cleanText(msg.Text, signaling.CaptionMaxRunes)
	if msg.SegmentID == "" || len(msg.SegmentID) > maxSegmentIDLen {
		return
	}
	m.mu.Lock()
	st := m.streams[msg.StreamID]
	if m.provider == nil || m.provider.sink != p || st == nil {
		m.mu.Unlock()
		return
	}
	sessionID, participantID := st.sessionID, st.participantID
	m.mu.Unlock()
	name, admitted := m.out.AdmittedParticipant(sessionID, participantID)
	if !admitted {
		return
	}

	m.mu.Lock()
	s := m.sessions[sessionID]
	if m.streams[msg.StreamID] != st || s == nil || !s.enabled || s.ended {
		m.mu.Unlock()
		return
	}
	now := m.now()
	if now.Sub(st.windowStart) >= time.Second {
		st.windowStart, st.windowCount = now, 0
	}
	if st.windowCount >= maxResultsPerSec {
		m.dropped++
		m.mu.Unlock()
		return
	}
	id, known := st.segments[msg.SegmentID]
	if !msg.IsFinal && known && now.Sub(st.lastPartial) < PartialInterval {
		m.mu.Unlock()
		return
	}
	if !known {
		if text == "" {
			m.mu.Unlock()
			return
		}
		st.segmentN++
		id = st.id + "." + strconv.Itoa(st.segmentN)
		st.segments[msg.SegmentID] = id
	}
	st.windowCount++
	if msg.IsFinal {
		delete(st.segments, msg.SegmentID)
	} else {
		st.lastPartial = now
	}
	// Provider times are relative to the stream's first sample; clamp them to
	// the audio actually sent (plus slack for recognizer latency).
	limit := (samplesDuration(st.samples) + 5*time.Second).Milliseconds()
	startMs := min(max(msg.StartMs, 0), limit)
	endMs := min(max(msg.EndMs, startMs), limit)
	s.seq++
	c := signaling.CaptionPayload{
		ID:            id,
		Seq:           s.seq,
		ParticipantID: participantID,
		Name:          name,
		Text:          text,
		IsFinal:       msg.IsFinal,
		StartMs:       st.origin.UnixMilli() + startMs,
		EndMs:         st.origin.UnixMilli() + endMs,
	}
	if c.IsFinal && c.Text != "" {
		if len(s.finals) >= MaxFinalsPerSession {
			s.finals = s.finals[1:]
		}
		s.finals = append(s.finals, c)
	}
	m.mu.Unlock()
	m.out.BroadcastCaption(sessionID, c)
}

// Dropped reports how many audio frames or results were dropped (diagnostics).
func (m *Manager) Dropped() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped
}

// Counts reports live caption objects for local diagnostics.
func (m *Manager) Counts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	provider := 0
	if m.provider != nil {
		provider = 1
	}
	return map[string]int{"captionSessions": len(m.sessions), "captionStreams": len(m.streams), "captionProvider": provider}
}
