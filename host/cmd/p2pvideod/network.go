package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/sfu"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/turn"
)

// applyNetworkFlags folds the ICE/TURN flags into cfg and validates them.
func applyNetworkFlags(cfg *config.Config, noPublicSTUN bool, turnURLs string, funnelTURN bool) error {
	if noPublicSTUN {
		cfg.PublicSTUN = false
	}
	urls, err := config.ParseTURNURLs(turnURLs)
	if err != nil {
		return fmt.Errorf("--turn-url: %w", err)
	}
	if funnelTURN {
		if cfg.NoFunnel {
			return errors.New("--funnel-turn conflicts with --no-funnel")
		}
		u, err := config.FunnelTURNURL(cfg.PublicURL)
		if err != nil {
			return fmt.Errorf("--funnel-turn: %w", err)
		}
		urls = append(urls, u)
	}
	cfg.TURNURLs = urls
	if cfg.TURNListen != "" {
		host, _, err := net.SplitHostPort(cfg.TURNListen)
		if err != nil {
			return fmt.Errorf("--turn-listen: %w", err)
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("--turn-listen %q must be a loopback address; expose it only via tailscale serve", cfg.TURNListen)
		}
	} else if len(cfg.TURNURLs) > 0 {
		return errors.New("TURN URLs are advertised but --turn-listen is empty")
	}
	if cfg.UDPPort < 0 || cfg.UDPPort > 65535 {
		return fmt.Errorf("--udp-port %d out of range", cfg.UDPPort)
	}
	return nil
}

// sfuNetworkOptions derives the SFU candidate-gathering policy. Loopback host
// candidates are included whenever the embedded relay may run, because relay
// sockets live on 127.0.0.1 and reach the SFU only through loopback.
func sfuNetworkOptions(cfg *config.Config) sfu.NetworkOptions {
	opt := sfu.NetworkOptions{UDPPort: cfg.UDPPort, IncludeLoopback: cfg.TURNListen != ""}
	if cfg.PublicSTUN {
		opt.STUNURLs = cfg.STUNURLs
	}
	return opt
}

// startTURN starts the embedded relay when configured. On failure TURN is
// simply not advertised (cfg.TURNSecret stays nil) and calls fall back to
// direct paths. The secret is per run, memory-only and never logged.
func startTURN(cfg *config.Config, s *sfu.SFU, hub *signaling.Hub) *turn.Server {
	if cfg.TURNListen == "" {
		return nil
	}
	secret, err := turn.NewSecret()
	if err != nil {
		slog.Error("TURN disabled: cannot generate secret", "err", err)
		return nil
	}
	sfuPort := s.UDPPort()
	allow, err := relayPeerFilter(sfuPort)
	if err != nil {
		slog.Warn("TURN relay disabled and not advertised; public guests behind UDP-blocking networks cannot connect", "err", err)
		return nil
	}
	relay, err := turn.Start(turn.Config{
		ListenAddr: cfg.TURNListen,
		Secret:     secret,
		AllowPeer:  allow,
		Authorize:  hub.ParticipantMayRelay,
	})
	if err != nil {
		slog.Error("TURN relay unavailable; public guests behind UDP-blocking networks cannot connect", "err", err)
		return nil
	}
	// Denying, removing or ending cuts live relay allocations at once.
	hub.OnParticipantRevoked = func(participantID string) { relay.Revoke(participantID) }
	cfg.TURNSecret = secret
	slog.Info("TURN relay listening", "addr", relay.Addr().String(), "advertised", cfg.TURNURLs, "sfuUdpPort", sfuPort)
	if len(cfg.TURNURLs) == 0 {
		slog.Info("TURN relay running but not advertised; pass --funnel-turn or --turn-url to offer it to guests")
	}
	return relay
}

// relayPeerFilter is the only destination relayed datagrams may reach: the
// SFU's single loopback UDP mux port. When the mux fell back to ephemeral
// ports (sfuPort 0) there is no fixed port to pin, and allowing "any loopback
// port" would let a guest's relay reach every local UDP service, so TURN fails
// closed instead [SL-SEC-1].
func relayPeerFilter(sfuPort int) (func(*net.UDPAddr) bool, error) {
	if sfuPort <= 0 {
		return nil, errors.New("the SFU is not on a single fixed UDP port (--udp-port busy or 0), so relay traffic cannot be pinned to it; free the port or pass a fixed --udp-port")
	}
	return func(a *net.UDPAddr) bool { return a.IP.IsLoopback() && a.Port == sfuPort }, nil
}
