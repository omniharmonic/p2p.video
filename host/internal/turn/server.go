package turn

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	pturn "github.com/pion/turn/v5"
)

// DefaultListenAddr is the loopback TCP address the relay listens on. Tailscale
// Funnel terminates TLS on :8443 and forwards plain TCP here
// (`tailscale serve --tls-terminated-tcp=8443 tcp://127.0.0.1:3478`).
const DefaultListenAddr = "127.0.0.1:3478"

// DefaultRealm is the TURN realm advertised in 401 challenges.
const DefaultRealm = "studiolink"

var errTCPRelayDisabled = errors.New("turn: TCP relay allocations (RFC 6062) are disabled")

// Config configures the embedded relay.
type Config struct {
	// ListenAddr is the TCP address for TURN-over-TCP clients. Keep it on loopback.
	ListenAddr string
	Realm      string
	// Secret is the shared HMAC secret used for credentials. Never log it.
	Secret []byte
	// RelayIP is where relay sockets are bound and the address returned to the
	// client as its relay candidate. Defaults to 127.0.0.1: the only peer a relay
	// needs to reach is the SFU on the same host.
	RelayIP net.IP
	// AllowPeer restricts where relayed datagrams may be sent. Nil allows any
	// port on a loopback address. Production narrows this to the SFU UDP port.
	AllowPeer func(*net.UDPAddr) bool
	// Authorize, when set, is consulted on every authenticated TURN request so
	// a denied/removed participant's still-valid credentials stop working.
	Authorize func(participantID string) bool
}

// Server is a running embedded TURN relay.
type Server struct {
	srv    *pturn.Server
	ln     net.Listener
	relays *loopbackRelay
}

// Start listens on cfg.ListenAddr (TCP) and serves TURN.
func Start(cfg Config) (*Server, error) {
	if len(cfg.Secret) < 16 {
		return nil, errors.New("turn: secret too short")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if cfg.Realm == "" {
		cfg.Realm = DefaultRealm
	}
	if cfg.RelayIP == nil {
		cfg.RelayIP = net.IPv4(127, 0, 0, 1)
	}
	allow := cfg.AllowPeer
	if allow == nil {
		allow = func(a *net.UDPAddr) bool { return a.IP.IsLoopback() }
	}
	relayIP := cfg.RelayIP
	ln, err := net.Listen("tcp", cfg.ListenAddr) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("turn listen %s: %w", cfg.ListenAddr, err)
	}
	secret := append([]byte(nil), cfg.Secret...)
	authorize := cfg.Authorize
	relays := &loopbackRelay{
		static: pturn.RelayAddressGeneratorStatic{RelayAddress: relayIP, Address: relayIP.String()},
		allow:  allow,
		byUser: map[string]map[*filteredConn]struct{}{},
	}
	srv, err := pturn.NewServer(pturn.ServerConfig{
		Realm:         cfg.Realm,
		LoggerFactory: slogFactory{},
		AuthHandler: func(ra *pturn.RequestAttributes) (string, []byte, bool) {
			pid, err := ParseUsername(ra.Username, time.Now())
			if err != nil {
				slog.Debug("TURN auth rejected", "reason", err, "src", ra.SrcAddr)
				return "", nil, false
			}
			if authorize != nil && !authorize(pid) {
				slog.Info("TURN auth rejected for unadmitted participant", "participantId", pid)
				return "", nil, false
			}
			return pid, pturn.GenerateAuthKey(ra.Username, ra.Realm, password(secret, ra.Username)), true
		},
		ListenerConfigs: []pturn.ListenerConfig{{
			Listener:              ln,
			RelayAddressGenerator: relays,
			// Permissions are per peer IP; the relay only ever needs the host itself.
			PermissionHandler: func(_ net.Addr, peer net.IP) bool {
				return peer.IsLoopback() || peer.Equal(relayIP)
			},
		}},
	})
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("start turn server: %w", err)
	}
	return &Server{srv: srv, ln: ln, relays: relays}, nil
}

// Addr is the TCP address the relay listens on.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Revoke closes every live relay allocation of a participant (the TURN
// username's participant ID) right away. Without it a denied or removed
// participant's existing allocation would keep relaying until its next
// refresh is refused by Authorize (up to the allocation lifetime) [SL-SEC-1].
// It returns how many relay sockets were closed.
func (s *Server) Revoke(participantID string) int {
	return s.relays.revoke(participantID)
}

// RelayCount reports live relay sockets for a participant (tests, diagnostics).
func (s *Server) RelayCount(participantID string) int {
	s.relays.mu.Lock()
	defer s.relays.mu.Unlock()
	return len(s.relays.byUser[participantID])
}

// Close stops the relay and all allocations.
func (s *Server) Close() error {
	if err := s.srv.Close(); err != nil {
		return fmt.Errorf("close turn server: %w", err)
	}
	return nil
}

// loopbackRelay binds UDP relay sockets on the host (loopback by default) and
// drops any relayed datagram whose destination is not an allowed peer. Since
// the SFU runs in the same process host, a relay bound to 127.0.0.1 reaches it
// through the SFU's loopback host candidate without ever opening a public
// relay port, and cannot be used to send traffic off-host.
type loopbackRelay struct {
	static pturn.RelayAddressGeneratorStatic
	allow  func(*net.UDPAddr) bool
	// byUser tracks live relay sockets per authenticated participant so they
	// can be closed when the participant is revoked.
	mu     sync.Mutex
	byUser map[string]map[*filteredConn]struct{}
}

func (r *loopbackRelay) track(c *filteredConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.byUser[c.user]
	if set == nil {
		set = map[*filteredConn]struct{}{}
		r.byUser[c.user] = set
	}
	set[c] = struct{}{}
}

func (r *loopbackRelay) untrack(c *filteredConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if set := r.byUser[c.user]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(r.byUser, c.user)
		}
	}
}

func (r *loopbackRelay) revoke(user string) int {
	r.mu.Lock()
	conns := make([]*filteredConn, 0, len(r.byUser[user]))
	for c := range r.byUser[user] {
		conns = append(conns, c)
	}
	delete(r.byUser, user)
	r.mu.Unlock()
	for _, c := range conns {
		c.revoked.Store(true)
		_ = c.PacketConn.Close()
	}
	if len(conns) > 0 {
		slog.Info("TURN allocations closed for revoked participant", "participantId", user, "relays", len(conns))
	}
	return len(conns)
}

func (r *loopbackRelay) Validate() error {
	if err := r.static.Validate(); err != nil {
		return fmt.Errorf("relay address generator: %w", err)
	}
	return nil
}

func (r *loopbackRelay) AllocatePacketConn(conf pturn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	conn, addr, err := r.static.AllocatePacketConn(conf)
	if err != nil {
		return nil, nil, fmt.Errorf("allocate relay: %w", err)
	}
	fc := &filteredConn{PacketConn: conn, allow: r.allow, user: conf.UserID, owner: r}
	r.track(fc)
	return fc, addr, nil
}

func (r *loopbackRelay) AllocateListener(pturn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	return nil, nil, errTCPRelayDisabled
}

func (r *loopbackRelay) AllocateConn(pturn.AllocateConnConfig) (net.Conn, error) {
	return nil, errTCPRelayDisabled
}

type filteredConn struct {
	net.PacketConn
	allow   func(*net.UDPAddr) bool
	user    string
	owner   *loopbackRelay
	revoked atomic.Bool
}

func (c *filteredConn) Close() error {
	c.owner.untrack(c)
	if err := c.PacketConn.Close(); err != nil {
		return fmt.Errorf("close relay: %w", err)
	}
	return nil
}

func (c *filteredConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.revoked.Load() {
		return 0, net.ErrClosed
	}
	u, ok := addr.(*net.UDPAddr)
	if !ok || !c.allow(u) {
		// Silently drop, like a firewall; the TURN client sees no delivery.
		return len(p), nil
	}
	n, err := c.PacketConn.WriteTo(p, addr)
	if err != nil {
		return n, fmt.Errorf("relay write: %w", err)
	}
	return n, nil
}

// slogFactory routes pion/turn's internal logs to slog. Pion only logs
// usernames (expiry:participantId), never the credential or secret.
type slogFactory struct{}

func (slogFactory) NewLogger(scope string) logging.LeveledLogger {
	return slogLogger{l: slog.Default().With("component", "turn", "scope", scope)}
}

type slogLogger struct{ l *slog.Logger }

func (slogLogger) Trace(string)          {}
func (slogLogger) Tracef(string, ...any) {}
func (s slogLogger) Debug(m string)      { s.l.Debug(m) }
func (s slogLogger) Debugf(f string, a ...any) {
	s.l.Debug(fmt.Sprintf(f, a...))
}
func (s slogLogger) Info(m string) { s.l.Debug(m) }
func (s slogLogger) Infof(f string, a ...any) {
	s.l.Debug(fmt.Sprintf(f, a...))
}
func (s slogLogger) Warn(m string) { s.l.Warn(m) }
func (s slogLogger) Warnf(f string, a ...any) {
	s.l.Warn(fmt.Sprintf(f, a...))
}
func (s slogLogger) Error(m string) { s.l.Warn(m) }
func (s slogLogger) Errorf(f string, a ...any) {
	s.l.Warn(fmt.Sprintf(f, a...))
}
