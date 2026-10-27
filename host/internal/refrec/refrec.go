// Package refrec records the host's received copy of each participant's
// microphone during a take: the alignment reference of ADR-007 [SL-P0-10].
//
// It is the only part of the recording pipeline allowed to read
// WebRTC-received media (AGENTS.md), and it is used for alignment only: its
// audio never reaches an export. For every take it writes, per participant,
//
//	refs/<participantID>.ogg  the received Opus packets (pion oggwriter)
//	refs/<participantID>.pkt  one record per packet written to the Ogg file:
//	                          SSRC, RTP timestamp, host arrival time, samples
//
// The packet log is what makes the reference usable: decoded Ogg audio has no
// host-time origin, and the oggwriter's granule positions count samples
// without RTP gaps. The log maps every decoded sample back to its packet, and
// each packet's RTP timestamp to the host monotonic clock (clock package) via
// its arrival time (align.FitArrival).
package refrec

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/omniharmonic/studiolink/host/internal/clock"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// OggPreSkip is the pre-skip pion's oggwriter declares in the OpusHead
// (48 kHz samples). Decoders drop that many samples from the start of the
// stream, so decoded sample k is stream sample k+OggPreSkip.
const OggPreSkip = 3840

// packetMagic starts a packet log (version 1).
var packetMagic = [8]byte{'S', 'L', 'R', 'E', 'F', 'P', 'K', '1'}

const packetRecordSize = 4 + 4 + 8 + 4

// Packet is one Opus packet as written to the reference Ogg file.
type Packet struct {
	SSRC      uint32
	RTPTime   uint32 // 48 kHz RTP timestamp
	ArrivalUs int64  // host monotonic arrival time (clock.MonotonicNowUs)
	Samples   uint32 // decoded 48 kHz samples in the packet
}

// TrackRecorder writes one participant's reference audio for one take.
// Packets are queued and written by the recorder's own goroutine, so a slow
// disk never delays the SFU's forwarding loop: a full queue drops the packet
// (the reference just has a gap, like network loss).
type TrackRecorder struct {
	SourceID string
	FilePath string
	mu       sync.Mutex // guards closed and queue sends
	closed   bool
	queue    chan queuedPacket
	done     chan struct{}
	dropped  int64
	lossless bool
	// owned by the writer goroutine
	ogg     *oggwriter.OggWriter
	logFile *os.File
	log     *bufio.Writer
	packets int64
	err     error
}

type queuedPacket struct {
	packet    *rtp.Packet
	samples   uint32
	arrivalUs int64
}

// queueLen holds ~10 s of 20 ms packets.
const queueLen = 512

type takeRefs struct {
	dir       string
	recorders map[string]*TrackRecorder // participantID -> recorder
	denied    map[string]bool           // participants Allow refused
}

// Manager keeps the reference recorders of every session that has a take
// running. Recorders start on a participant's first packet during the take,
// so late joiners, phones and reconnects (new SSRC) are covered.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*takeRefs
	now      func() int64
	// Allow, when set, decides once per take whether a participant's
	// microphone may be kept as a reference (a producer is never
	// recorded, not even as a reference). It is called without the
	// manager's lock held. Set before the SFU forwards media.
	Allow func(sessionID, participantID string) bool
	// Lossless makes a full queue block instead of dropping (offline tools
	// and tests that write faster than real time; never the live SFU).
	Lossless bool
}

// NewManager creates a reference recording manager.
func NewManager() *Manager {
	return &Manager{sessions: make(map[string]*takeRefs), now: clock.MonotonicNowUs}
}

// SetClock replaces the arrival clock (tests and offline tools).
func (m *Manager) SetClock(now func() int64) {
	m.mu.Lock()
	m.now = now
	m.mu.Unlock()
}

// StartTake begins reference capture for a session's take into refsDir.
// A running take's recorders are closed first.
func (m *Manager) StartTake(sessionID, refsDir string) error {
	if err := os.MkdirAll(refsDir, 0o700); err != nil {
		return fmt.Errorf("create refs dir: %w", err)
	}
	m.StopTake(sessionID)
	m.mu.Lock()
	m.sessions[sessionID] = &takeRefs{dir: refsDir, recorders: make(map[string]*TrackRecorder), denied: make(map[string]bool)}
	m.mu.Unlock()
	return nil
}

// StopTake closes a session's reference recorders.
func (m *Manager) StopTake(sessionID string) {
	m.mu.Lock()
	refs := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	if refs == nil {
		return
	}
	for id, rec := range refs.recorders {
		if err := rec.close(); err != nil {
			slog.Warn("Close reference recording", "participantId", id, "err", err)
		}
	}
}

// WriteRTP records a participant's received microphone packet while the
// session has a take running. Errors never affect the call.
func (m *Manager) WriteRTP(sessionID, participantID string, packet *rtp.Packet) error {
	if packet == nil || len(packet.Payload) == 0 {
		return nil
	}
	m.mu.Lock()
	arrival := m.now()
	refs := m.sessions[sessionID]
	if refs == nil || refs.denied[participantID] {
		m.mu.Unlock()
		return nil
	}
	rec := refs.recorders[participantID]
	m.mu.Unlock()
	if rec == nil {
		allowed := m.Allow == nil || m.Allow(sessionID, participantID)
		m.mu.Lock()
		if m.sessions[sessionID] != refs { // the take stopped meanwhile
			m.mu.Unlock()
			return nil
		}
		if !allowed {
			refs.denied[participantID] = true
			m.mu.Unlock()
			return nil
		}
		if rec = refs.recorders[participantID]; rec == nil {
			var err error
			if rec, err = newTrackRecorder(participantID, refs.dir, m.Lossless); err != nil {
				refs.denied[participantID] = true // don't retry every packet
				m.mu.Unlock()
				return err
			}
			refs.recorders[participantID] = rec
		}
		m.mu.Unlock()
	}
	return rec.write(packet, arrival)
}

// StopAll stops every running reference capture.
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.StopTake(id)
	}
}

func newTrackRecorder(participantID, dir string, lossless bool) (*TrackRecorder, error) {
	if participantID == "" || filepath.Base(participantID) != participantID || participantID == "." || participantID == ".." {
		return nil, fmt.Errorf("invalid participant id for reference")
	}
	oggPath := filepath.Join(dir, participantID+".ogg")
	ogg, err := oggwriter.New(oggPath, 48000, 2)
	if err != nil {
		return nil, fmt.Errorf("create ogg writer: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, participantID+".pkt"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		_ = ogg.Close()
		return nil, fmt.Errorf("create packet log: %w", err)
	}
	w := bufio.NewWriterSize(f, 64<<10)
	if _, err := w.Write(packetMagic[:]); err != nil {
		_ = ogg.Close()
		_ = f.Close()
		return nil, fmt.Errorf("write packet log: %w", err)
	}
	slog.Info("Started reference recording", "participantId", participantID)
	r := &TrackRecorder{SourceID: participantID, FilePath: oggPath, ogg: ogg, logFile: f, log: w,
		queue: make(chan queuedPacket, queueLen), done: make(chan struct{}), lossless: lossless}
	go r.run()
	return r, nil
}

// write queues a packet without blocking. The payload is copied: the caller
// keeps forwarding the same packet.
func (r *TrackRecorder) write(packet *rtp.Packet, arrivalUs int64) error {
	samples, ok := OpusSamples(packet.Payload)
	if !ok {
		return nil // the oggwriter would reject it too; keep the log in step
	}
	cp := &rtp.Packet{Header: packet.Header, Payload: append([]byte(nil), packet.Payload...)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	q := queuedPacket{cp, samples, arrivalUs}
	if r.lossless {
		r.queue <- q // the writer never takes r.mu, so this cannot deadlock
		return nil
	}
	select {
	case r.queue <- q:
	default:
		r.dropped++
	}
	return nil
}

func (r *TrackRecorder) run() {
	defer close(r.done)
	for q := range r.queue {
		if r.err != nil {
			continue
		}
		if err := r.ogg.WriteRTP(q.packet); err != nil {
			continue // not in the Ogg file, so not in the log either
		}
		var rec [packetRecordSize]byte
		binary.LittleEndian.PutUint32(rec[0:], q.packet.SSRC)
		binary.LittleEndian.PutUint32(rec[4:], q.packet.Timestamp)
		binary.LittleEndian.PutUint64(rec[8:], uint64(q.arrivalUs))
		binary.LittleEndian.PutUint32(rec[16:], q.samples)
		r.packets++
		if _, err := r.log.Write(rec[:]); err != nil {
			r.err = err
		}
	}
}

func (r *TrackRecorder) close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.queue)
	r.mu.Unlock()
	<-r.done
	err := errors.Join(r.err, r.ogg.Close(), r.log.Flush(), r.logFile.Close())
	slog.Info("Closed reference recording", "participantId", r.SourceID, "packets", r.packets, "dropped", r.dropped)
	return err
}

// OpusSamples returns the 48 kHz sample count of an Opus packet (RFC 6716
// §3.1: TOC configuration and frame count code).
func OpusSamples(payload []byte) (uint32, bool) {
	if len(payload) == 0 {
		return 0, false
	}
	toc := payload[0]
	cfg := toc >> 3
	var frame uint32
	switch {
	case cfg < 12: // SILK: 10, 20, 40, 60 ms
		frame = [4]uint32{480, 960, 1920, 2880}[cfg&3]
	case cfg < 16: // hybrid: 10, 20 ms
		frame = [2]uint32{480, 960}[cfg&1]
	default: // CELT: 2.5, 5, 10, 20 ms
		frame = [4]uint32{120, 240, 480, 960}[cfg&3]
	}
	var frames uint32
	switch toc & 3 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	default:
		if len(payload) < 2 {
			return 0, false
		}
		frames = uint32(payload[1] & 0x3f)
	}
	n := frame * frames
	if n == 0 || n > 5760 { // at most 120 ms
		return 0, false
	}
	return n, true
}

// ReadPacketLog reads a reference packet log.
func ReadPacketLog(path string) ([]Packet, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var magic [8]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || magic != packetMagic {
		return nil, fmt.Errorf("not a reference packet log")
	}
	var out []Packet
	var rec [packetRecordSize]byte
	for {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out, nil // a torn final record (crash) is dropped
			}
			return nil, err
		}
		out = append(out, Packet{
			SSRC:      binary.LittleEndian.Uint32(rec[0:]),
			RTPTime:   binary.LittleEndian.Uint32(rec[4:]),
			ArrivalUs: int64(binary.LittleEndian.Uint64(rec[8:])),
			Samples:   binary.LittleEndian.Uint32(rec[16:]),
		})
	}
}
