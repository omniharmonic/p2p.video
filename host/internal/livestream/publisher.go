package livestream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Destination states reported in DestinationStatus.State.
const (
	DestPending      = "pending"      // waiting for the encoder's first media
	DestConnecting   = "connecting"   // dialing / RTMP handshake
	DestLive         = "live"         // media flowing to the platform
	DestReconnecting = "reconnecting" // lost or refused; retrying with backoff
	DestFailed       = "failed"       // gave up after MaxAttempts
	DestStopped      = "stopped"
)

// mediaConn is a publishing connection (rtmpConn, or a fake in tests).
type mediaConn interface {
	writeTag(kind byte, ts uint32, data []byte) error
	Done() <-chan struct{}
	Err() error
	Close()
	closeGracefully()
}

type dialFunc func(ctx context.Context, t rtmpTarget) (mediaConn, error)

func rtmpDialer(tlsConf *tls.Config, writeTimeout time.Duration) dialFunc {
	return func(ctx context.Context, t rtmpTarget) (mediaConn, error) {
		c, err := dialRTMP(ctx, t, tlsConf, writeTimeout)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// publisherConfig is shared by a stream's destinations.
type publisherConfig struct {
	dial        dialFunc
	backoffBase time.Duration
	backoffMax  time.Duration
	maxAttempts int
	stableAfter time.Duration
	maxBacklog  time.Duration
	now         func() time.Time
	logger      *slog.Logger
	sessionID   string
	redact      *Redactor
	onFailed    func()
}

// backoff is the wait before retry n (1-based): base·2^(n-1), capped.
func (c *publisherConfig) backoff(n int) time.Duration {
	d := c.backoffBase << uint(min(n-1, 16))
	if d <= 0 || d > c.backoffMax {
		return c.backoffMax
	}
	return d
}

// publisher sends the encoder's FLV tags to one destination, reconnecting
// on its own: a destination that fails, stalls or reconnects never blocks
// the encoder or the other destinations (each has its own bounded queue).
type publisher struct {
	index  int
	name   string
	server string
	cfg    *publisherConfig

	mu        sync.Mutex
	cond      *sync.Cond
	target    rtmpTarget // holds the key; cleared on close
	init      *streamInit
	initDirty bool
	queue     []flvTag
	connected bool
	waitKey   bool
	closed    bool
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}

	state      string
	lastErr    string
	attempts   int // consecutive failures
	reconnects int
	retryAt    time.Time
	liveSince  time.Time
	bytesOut   int64
	rate       *rateMeter
	dropped    int64
	lastDropAt time.Time
	backlogMs  int64
}

func newPublisher(index int, d Destination, t rtmpTarget, cfg *publisherConfig) *publisher {
	ctx, cancel := context.WithCancel(context.Background())
	p := &publisher{index: index, name: d.Name, server: ServerLabel(d.URL), cfg: cfg, target: t,
		ctx: ctx, cancel: cancel, done: make(chan struct{}), state: DestPending, rate: newRateMeter(RateWindow)}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *publisher) start() { go p.run() }

// setInit announces a new encoder run (or a mid-stream codec change).
func (p *publisher) setInit(in streamInit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	cp := in
	p.init, p.initDirty = &cp, true
	p.queue, p.waitKey, p.backlogMs = nil, true, 0
	p.cond.Broadcast()
}

// push queues one media tag without ever blocking. A destination whose
// backlog exceeds maxBacklog drops what it has and resumes at the next
// keyframe, as a hardware encoder would.
func (p *publisher) push(tag flvTag) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.connected {
		return
	}
	if p.waitKey {
		if !tag.isKeyframe() {
			return
		}
		p.waitKey = false
	}
	p.queue = append(p.queue, tag)
	p.backlogMs = int64(tag.ts) - int64(p.queue[0].ts)
	if time.Duration(p.backlogMs)*time.Millisecond > p.cfg.maxBacklog {
		for _, q := range p.queue {
			if q.kind == tagVideo {
				p.dropped++
			}
		}
		p.queue, p.waitKey, p.backlogMs = nil, true, 0
		p.lastDropAt = p.cfg.now()
	}
	p.cond.Signal()
}

// close stops the destination; its key is forgotten immediately.
func (p *publisher) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.target.stream = ""
	p.queue = nil
	if p.state != DestFailed {
		p.state = DestStopped
	}
	p.cancel()
	p.cond.Broadcast()
}

func (p *publisher) run() {
	defer close(p.done)
	for {
		p.mu.Lock()
		for p.init == nil && !p.closed {
			p.cond.Wait()
		}
		if p.closed {
			p.mu.Unlock()
			return
		}
		if p.attempts == 0 {
			p.state = DestConnecting
		}
		target := p.target
		p.mu.Unlock()

		conn, err := p.cfg.dial(p.ctx, target)
		if err != nil {
			if p.retry(err) {
				return
			}
			continue
		}
		p.mu.Lock()
		p.connected, p.initDirty, p.waitKey, p.queue = true, true, true, nil
		p.mu.Unlock()
		err = p.stream(conn)
		p.mu.Lock()
		p.connected, p.queue, p.backlogMs = false, nil, 0
		closed, liveFor := p.closed, time.Duration(0)
		if !p.liveSince.IsZero() {
			liveFor = p.cfg.now().Sub(p.liveSince)
		}
		p.liveSince = time.Time{}
		if liveFor >= p.cfg.stableAfter {
			p.attempts = 0
		}
		p.mu.Unlock()
		if closed {
			conn.closeGracefully()
			return
		}
		conn.Close()
		if p.retry(err) {
			return
		}
	}
}

// retry records a failure and waits out the backoff. It reports true when
// the publisher is finished (closed, or out of attempts).
func (p *publisher) retry(err error) bool {
	msg := p.cfg.redact.Redact(describeErr(err))
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return true
	}
	p.attempts++
	p.lastErr = msg
	if p.attempts > p.cfg.maxAttempts {
		p.state = DestFailed
		p.mu.Unlock()
		p.cfg.logger.Warn("livestream destination failed", "session", p.cfg.sessionID, "destination", p.index+1, "server", p.server, "err", msg)
		if p.cfg.onFailed != nil {
			p.cfg.onFailed()
		}
		return true
	}
	wait := p.cfg.backoff(p.attempts)
	p.state = DestReconnecting
	p.reconnects++
	p.retryAt = p.cfg.now().Add(wait)
	attempt := p.attempts
	p.mu.Unlock()
	p.cfg.logger.Warn("livestream destination reconnecting", "session", p.cfg.sessionID, "destination", p.index+1, "server", p.server, "attempt", attempt, "in", wait.String(), "err", msg)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.ctx.Done():
		return true
	case <-t.C:
	}
	p.mu.Lock()
	p.retryAt = time.Time{}
	p.mu.Unlock()
	return false
}

// describeErr is a short, human reason. Callers redact it.
func describeErr(err error) string {
	if err == nil {
		return "the server closed the connection"
	}
	s := err.Error()
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the server closed the connection"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "the connection was reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "i/o timeout"):
		return "timed out: " + s
	}
	return s
}

// stream writes queued tags to conn until it breaks or the publisher closes.
func (p *publisher) stream(conn mediaConn) error {
	stopWake := make(chan struct{})
	defer close(stopWake)
	go func() {
		select {
		case <-conn.Done():
		case <-stopWake:
		}
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
	var (
		offset    int64
		started   bool
		rebase    bool
		lastAudio int64 = -1
		lastVideo int64 = -1
	)
	connDone := func() bool {
		select {
		case <-conn.Done():
			return true
		default:
			return false
		}
	}
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.closed && !p.initDirty && !connDone() {
			p.cond.Wait()
		}
		if p.closed {
			p.mu.Unlock()
			return nil
		}
		if connDone() {
			p.mu.Unlock()
			if err := conn.Err(); err != nil {
				return err
			}
			return errors.New("the server closed the connection")
		}
		if p.initDirty {
			in := *p.init
			p.initDirty = false
			p.mu.Unlock()
			base := uint32(max(lastAudio, lastVideo, 0))
			for _, h := range []struct {
				kind byte
				data []byte
			}{{tagScript, in.meta}, {tagVideo, in.videoSeq}, {tagAudio, in.audioSeq}} {
				if len(h.data) == 0 {
					continue
				}
				if err := conn.writeTag(h.kind, base, h.data); err != nil {
					return err
				}
			}
			rebase = true
			continue
		}
		batch := p.queue
		p.queue = nil
		p.backlogMs = 0
		p.mu.Unlock()
		for _, tag := range batch {
			if rebase {
				// Each connection starts at 0; an encoder restart continues
				// one frame after the last timestamp sent.
				next := int64(0)
				if started {
					next = max(lastAudio, lastVideo) + 33
				}
				offset, rebase, started = next-int64(tag.ts), false, true
			}
			out := int64(tag.ts) + offset
			last := &lastVideo
			if tag.kind == tagAudio {
				last = &lastAudio
			}
			if out < *last {
				out = *last
			}
			if out < 0 {
				out = 0
			}
			*last = out
			if err := conn.writeTag(tag.kind, uint32(min(out, math.MaxUint32)), tag.data); err != nil {
				return err
			}
			p.sent(len(tag.data))
		}
	}
}

func (p *publisher) sent(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.now()
	p.bytesOut += int64(n)
	p.rate.observe(now, p.bytesOut)
	if p.state != DestLive {
		p.state, p.lastErr, p.liveSince = DestLive, "", now
		p.cfg.logger.Info("livestream destination live", "session", p.cfg.sessionID, "destination", p.index+1, "server", p.server, "reconnects", p.reconnects)
	}
}

// wait blocks until the publisher's goroutine has exited.
func (p *publisher) wait(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *publisher) status() DestinationStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.now()
	st := DestinationStatus{Name: p.name, Server: p.server, State: p.state, Error: p.lastErr,
		Reconnects: p.reconnects, BytesOut: p.bytesOut, DroppedFrames: p.dropped, BacklogMs: p.backlogMs}
	if p.state == DestLive {
		st.Kbps = p.rate.kbps(now)
	}
	if p.state == DestReconnecting {
		st.Attempt = p.attempts
		st.MaxAttempts = p.cfg.maxAttempts
		if !p.retryAt.IsZero() {
			st.RetryInSec = math.Max(0, math.Ceil(p.retryAt.Sub(now).Seconds()))
		}
	}
	st.Behind = !p.lastDropAt.IsZero() && now.Sub(p.lastDropAt) < RateWindow
	return st
}

// warnings explains a destination's trouble in one line each.
func (s DestinationStatus) warnings() []string {
	var out []string
	switch s.State {
	case DestReconnecting:
		out = append(out, fmt.Sprintf("%s: reconnecting (attempt %d of %d)%s", s.Name, s.Attempt, s.MaxAttempts, reasonSuffix(s.Error)))
	case DestFailed:
		out = append(out, fmt.Sprintf("%s: gave up reconnecting%s", s.Name, reasonSuffix(s.Error)))
	}
	if s.Behind {
		out = append(out, fmt.Sprintf("%s: the upload can't keep up, so frames were skipped to the next keyframe", s.Name))
	} else if s.BacklogMs > 1500 {
		out = append(out, fmt.Sprintf("%s: %.1f s behind", s.Name, float64(s.BacklogMs)/1000))
	}
	return out
}

func reasonSuffix(err string) string {
	if err == "" {
		return ""
	}
	return ": " + err
}
