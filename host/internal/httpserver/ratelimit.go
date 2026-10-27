package httpserver

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Guest join rate limits [SL-SEC-1]: token buckets applied to guest
// WebSocket upgrades before any socket, hub client or database row exists.
// Generous enough for reconnect storms on a bad network and for a group
// behind one NAT; tight enough that a leaked link cannot flood the host.
const (
	// Per client address: a burst of 20, then one join every 3 s.
	joinAddrBurst  = 20
	joinAddrPerSec = 1.0 / 3
	// Per session (all addresses): a burst of 60, then one per second.
	joinSessionBurst  = 60
	joinSessionPerSec = 1.0
	// limiterMaxKeys bounds memory; full (idle) buckets are dropped first.
	limiterMaxKeys = 10_000
)

type bucket struct {
	tokens float64
	at     time.Time
}

// limiter is a keyed token bucket.
type limiter struct {
	mu      sync.Mutex
	burst   float64
	perSec  float64
	buckets map[string]*bucket
	now     func() time.Time
}

func newLimiter(burst int, perSec float64) *limiter {
	return &limiter{burst: float64(burst), perSec: perSec, buckets: map[string]*bucket{}, now: time.Now}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= limiterMaxKeys {
			l.pruneLocked(now)
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.at).Seconds(); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.perSec)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// pruneLocked drops buckets that have refilled completely (no information).
func (l *limiter) pruneLocked(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// clientAddr is the address a join is attributed to. Serve and Funnel proxy
// from loopback and set X-Forwarded-For to the real client (a tailnet or
// public IP), so on a loopback connection the first X-Forwarded-For entry is
// used; anything else uses the TCP peer. A local process can spoof the
// header, which only lets it pick its own bucket.
func clientAddr(r *http.Request) string {
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	if ip := net.ParseIP(remote); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if fwd := net.ParseIP(strings.TrimSpace(first)); fwd != nil {
				return fwd.String()
			}
		}
	}
	return remote
}

// allowJoin applies the per-address and per-session join buckets.
func (s *Server) allowJoin(r *http.Request, sessionID string) bool {
	s.joinLimitsOnce.Do(func() {
		if s.joinByAddr == nil {
			s.joinByAddr = newLimiter(joinAddrBurst, joinAddrPerSec)
		}
		if s.joinBySession == nil {
			s.joinBySession = newLimiter(joinSessionBurst, joinSessionPerSec)
		}
	})
	// Check the address first so one noisy client does not drain the
	// session's shared bucket.
	return s.joinByAddr.allow(clientAddr(r)) && s.joinBySession.allow(sessionID)
}
