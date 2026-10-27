package sfu

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// NetworkOptions configures how the SFU's PeerConnections gather candidates
// (ARCHITECTURE §3, §4.1).
type NetworkOptions struct {
	// UDPPort is the single UDP port shared by every PeerConnection's host
	// candidates (one socket per local interface address). 0 keeps pion's
	// default of one ephemeral port per PeerConnection. If the port cannot be
	// bound the SFU logs a warning and falls back to ephemeral ports.
	UDPPort int
	// STUNURLs are given to the SFU's own PeerConnections so they gather
	// server-reflexive candidates for direct UDP to public guests. Only set
	// when public STUN is allowed by policy (ADR-005).
	STUNURLs []string
	// IncludeLoopback advertises 127.0.0.1 host candidates. Required for the
	// embedded TURN relay, whose relay sockets are bound to loopback and reach
	// the SFU only through its loopback candidate.
	IncludeLoopback bool
}

// Option customizes NewSFU.
type Option func(*options)

type options struct {
	network *NetworkOptions
}

// WithNetwork applies NetworkOptions. Without it the SFU uses pion defaults
// (ephemeral ports, host candidates only), which is what unit tests rely on.
func WithNetwork(n NetworkOptions) Option {
	return func(o *options) { o.network = &n }
}

type network struct {
	setting webrtc.SettingEngine
	config  webrtc.Configuration
	mux     io.Closer
	udpPort int
}

func (o options) build() network {
	var n network
	if o.network == nil {
		return n
	}
	opt := *o.network
	n.setting.SetIncludeLoopbackCandidate(opt.IncludeLoopback)
	if len(opt.STUNURLs) > 0 {
		n.config.ICEServers = []webrtc.ICEServer{{URLs: append([]string(nil), opt.STUNURLs...)}}
	}
	if opt.UDPPort > 0 {
		mux, err := newUDPMux(opt.UDPPort, opt.IncludeLoopback)
		if err != nil {
			slog.Warn("SFU UDP mux port unavailable; falling back to ephemeral UDP ports", "port", opt.UDPPort, "err", err)
		} else {
			n.setting.SetICEUDPMux(mux)
			n.mux = mux
			n.udpPort = opt.UDPPort
			slog.Info("SFU UDP mux listening", "port", opt.UDPPort, "loopback", opt.IncludeLoopback)
		}
	}
	return n
}

// newUDPMux binds port on every local interface address. IPv6 addresses can
// refuse binding (e.g. tentative/temporary addresses), so retry IPv4-only
// before giving up.
func newUDPMux(port int, loopback bool) (*ice.MultiUDPMuxDefault, error) {
	var opts []ice.UDPMuxFromPortOption
	if loopback {
		opts = append(opts, ice.UDPMuxFromPortWithLoopback())
	}
	mux, err := ice.NewMultiUDPMuxFromPort(port, opts...)
	if err == nil {
		return mux, nil
	}
	mux, err4 := ice.NewMultiUDPMuxFromPort(port, append(opts, ice.UDPMuxFromPortWithNetworks(ice.NetworkTypeUDP4))...)
	if err4 == nil {
		slog.Warn("SFU UDP mux bound IPv4 only", "port", port, "ipv6Err", err)
		return mux, nil
	}
	return nil, fmt.Errorf("bind udp mux port %d: %w", port, errors.Join(err, err4))
}

// UDPPort is the bound single UDP mux port, or 0 when using ephemeral ports.
func (s *SFU) UDPPort() int { return s.udpPort }

// Close releases the shared UDP mux sockets.
func (s *SFU) Close() error {
	if s.udpMux == nil {
		return nil
	}
	if err := s.udpMux.Close(); err != nil {
		return fmt.Errorf("close udp mux: %w", err)
	}
	return nil
}

// Counts reports live SFU objects for local diagnostics [SL-P0-6]: peers,
// forwarded publications, subscriptions (senders) and participants with ICE
// candidates queued before their offer.
func (s *SFU) Counts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	senders := 0
	for _, p := range s.peers {
		senders += len(p.senders)
	}
	return map[string]int{"sfuPeers": len(s.peers), "sfuPublications": len(s.tracks), "sfuSubscriptions": senders, "sfuPendingCandidateKeys": len(s.pendingCandidates)}
}
