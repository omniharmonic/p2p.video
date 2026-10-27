package sfu

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Publication sources. A participant publishes its camera + microphone and,
// while presenting, a screen (video + optional system/tab audio) on the same
// PeerConnection.
const (
	SourceCamera = "camera"
	SourceScreen = "screen"
	// ScreenStreamSuffix is appended to the participant ID to form the stream
	// ID of forwarded screen tracks. Mirrors signaling.ScreenStreamSuffix.
	ScreenStreamSuffix = "_screen"
)

var (
	// ErrGlare: a client renegotiation offer arrived while the server's own
	// subscription offer is outstanding. The server (impolite peer) keeps its
	// offer; the client rolls back when that offer reaches it and retries.
	ErrGlare = errors.New("server offer outstanding")
	// ErrNoPeer: a renegotiation offer for a participant without a peer.
	ErrNoPeer = errors.New("no peer to renegotiate")
)

type peerKey struct{ session, participant string }
type publication struct {
	owner  *peer
	track  *webrtc.TrackLocalStaticRTP
	ssrc   uint32
	source string
}
type peer struct {
	key                        peerKey
	pc                         *webrtc.PeerConnection
	connected, awaiting, dirty bool
	senders                    map[*publication]*webrtc.RTPSender
	candidates                 []webrtc.ICECandidateInit
	// skipped: publications whose codec this subscriber can't decode (logged once).
	skipped map[*publication]bool
}

// SFU routes publications only between admitted peers in the same session.
// The server owns renegotiation after the client's initial offer.
type SFU struct {
	RefRec            *refrec.Manager
	api               *webrtc.API
	config            webrtc.Configuration
	udpMux            io.Closer // shared single-port UDP mux, nil when ephemeral
	udpPort           int
	mu                sync.Mutex
	peers             map[peerKey]*peer
	tracks            map[*publication]struct{}
	pendingCandidates map[peerKey][]webrtc.ICECandidateInit
	// screens holds, per participant, the incoming track IDs declared as a
	// screen share. Keyed by participant (not peer) so it survives media
	// recovery; cleared when the participant leaves or the share stops.
	screens map[peerKey]map[string]bool
	// publishOnly participants (phone sub-sources) publish but are never
	// subscribed to anyone else's tracks. Keyed by participant, so it
	// survives media recovery; cleared when the participant leaves.
	publishOnly map[peerKey]bool
	onCandidate func(sessionID, participantID, candidate string, sdpMid *string, sdpMLineIndex *uint16)
	onOffer     func(sessionID, participantID, sdp string)
}

func NewSFU(ref *refrec.Manager, opts ...Option) (*SFU, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("register codecs: %w", err)
	}
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, fmt.Errorf("register interceptors: %w", err)
	}
	n := o.build()
	return &SFU{RefRec: ref, api: webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(n.setting)), config: n.config, udpMux: n.mux, udpPort: n.udpPort, peers: make(map[peerKey]*peer), tracks: make(map[*publication]struct{}), pendingCandidates: make(map[peerKey][]webrtc.ICECandidateInit), screens: make(map[peerKey]map[string]bool), publishOnly: make(map[peerKey]bool)}, nil
}

func (s *SFU) SetOnCandidate(fn func(sessionID, participantID, candidate string, sdpMid *string, sdpMLineIndex *uint16)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCandidate = fn
}
func (s *SFU) SetOnOffer(fn func(sessionID, participantID, sdp string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onOffer = fn
}

// HandleOffer negotiates a client-initiated PeerConnection. An offer for a
// participant that already has a peer is a media-recovery request: the client
// replaced an RTCPeerConnection that failed. The stale peer is torn down and
// rebuilt instead of ICE-restarted because the server may have its own
// subscription offer in flight for it (glare), its DTLS/SRTP state may be
// unusable, and an earlier failure may already have removed it. Server offers
// still in flight belong to the discarded peer; the client ignores offers
// while its own is pending. The participant's reference recording continues.
func (s *SFU) HandleOffer(sessionID, participantID, offerSDP string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := peerKey{sessionID, participantID}
	if old := s.peers[key]; old != nil {
		slog.Info("replacing peer after client re-offer", "participantId", participantID)
		s.detachPeerLocked(old)
		go func() { _ = old.pc.Close() }()
	}
	pc, err := s.api.NewPeerConnection(s.config)
	if err != nil {
		return "", fmt.Errorf("new peer connection: %w", err)
	}
	p := &peer{key: key, pc: pc, senders: make(map[*publication]*webrtc.RTPSender)}
	s.peers[key] = p
	success := false
	defer func() {
		if !success {
			delete(s.peers, key)
			delete(s.pendingCandidates, key)
			go pc.Close()
		}
	}()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		s.mu.Lock()
		fn := s.onCandidate
		active := s.peers[key] == p
		s.mu.Unlock()
		if active && fn != nil {
			init := c.ToJSON()
			fn(sessionID, participantID, init.Candidate, init.SDPMid, init.SDPMLineIndex)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.peers[key] != p {
			return
		}
		if state == webrtc.PeerConnectionStateConnected {
			p.connected = true
			s.syncPeerLocked(p)
		}
		if state == webrtc.PeerConnectionStateFailed {
			go s.removePeer(p)
		}
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { s.forward(p, remote) })
	if err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		return "", fmt.Errorf("set offer: %w", err)
	}
	for _, candidate := range s.pendingCandidates[key] {
		if err := pc.AddICECandidate(candidate); err != nil {
			return "", fmt.Errorf("add queued candidate: %w", err)
		}
	}
	delete(s.pendingCandidates, key)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create answer: %w", err)
	}
	if err = pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set answer: %w", err)
	}
	success = true
	return pc.LocalDescription().SDP, nil
}

// HandleRenegotiation applies a client offer to the participant's existing
// PeerConnection, for example to add or remove screen-share tracks mid-call.
// Unlike HandleOffer it never replaces the peer, so the participant's other
// publications and subscriptions keep flowing.
//
// Glare: the server is the impolite peer. If its own subscription offer is
// outstanding, the client offer is rejected with ErrGlare and no reply is
// needed: the client is in have-local-offer when the server offer reaches
// it, rolls back, answers, and then re-offers.
func (s *SFU) HandleRenegotiation(sessionID, participantID, offerSDP string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[peerKey{sessionID, participantID}]
	if p == nil {
		return "", ErrNoPeer
	}
	if p.awaiting || p.pc.SignalingState() != webrtc.SignalingStateStable {
		return "", ErrGlare
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		return "", fmt.Errorf("set renegotiation offer: %w", err)
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("create renegotiation answer: %w", err)
	}
	if err = p.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set renegotiation answer: %w", err)
	}
	return p.pc.LocalDescription().SDP, nil
}

// SetScreenTracks declares which incoming track IDs of a participant are its
// screen share. Tracks published later with these IDs are forwarded as
// screen publications (stream ID participant+ScreenStreamSuffix) and never
// feed the participant's reference recording. Passing no IDs ends the share:
// existing screen publications are unpublished immediately and subscribers
// renegotiate, whatever the sharer's client does next (server-enforced stop).
func (s *SFU) SetScreenTracks(sessionID, participantID string, trackIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := peerKey{sessionID, participantID}
	declared := make(map[string]bool, len(trackIDs))
	for _, id := range trackIDs {
		declared[id] = true
	}
	if len(declared) == 0 {
		delete(s.screens, key)
	} else {
		s.screens[key] = declared
	}
	removed := false
	for pub := range s.tracks {
		if pub.owner.key == key && pub.source == SourceScreen && !declared[pub.track.ID()] {
			delete(s.tracks, pub)
			removed = true
		}
	}
	if removed {
		for _, other := range s.peers {
			if other.key.session == sessionID {
				s.syncPeerLocked(other)
			}
		}
	}
}

// SetPublishOnly marks a participant (a phone sub-source) as sending only:
// it receives no subscriptions, which saves the phone's downlink and battery.
func (s *SFU) SetPublishOnly(sessionID, participantID string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := peerKey{sessionID, participantID}
	if on {
		s.publishOnly[key] = true
	} else {
		delete(s.publishOnly, key)
	}
	if p := s.peers[key]; p != nil {
		s.syncPeerLocked(p)
	}
}

// sourceOfLocked classifies an incoming track by the participant's declared
// screen track IDs.
func (s *SFU) sourceOfLocked(key peerKey, trackID string) string {
	if s.screens[key][trackID] {
		return SourceScreen
	}
	return SourceCamera
}

// syncPeerLocked reconciles subscriptions and coalesces changes while an offer
// is in flight. Waiting for connection prevents an offer overtaking initial SDP.
func (s *SFU) syncPeerLocked(p *peer) {
	if !p.connected {
		return
	}
	for pub, sender := range p.senders {
		if _, exists := s.tracks[pub]; !exists {
			if err := p.pc.RemoveTrack(sender); err != nil {
				slog.Warn("remove subscription", "err", err)
				continue
			}
			delete(p.senders, pub)
			p.dirty = true
		}
	}
	for pub := range s.tracks {
		if pub.owner == p || pub.owner.key.session != p.key.session || s.publishOnly[p.key] {
			continue
		}
		if _, exists := p.senders[pub]; exists {
			continue
		}
		if !subscriberAcceptsLocked(p, pub) {
			if p.skipped == nil {
				p.skipped = make(map[*publication]bool)
			}
			if !p.skipped[pub] {
				p.skipped[pub] = true
				slog.Warn("subscriber cannot decode publisher codec; not forwarding this track to it",
					"codec", pub.track.Codec().MimeType, "publisher", pub.owner.key.participant, "subscriber", p.key.participant)
			}
			continue
		}
		sender, err := p.pc.AddTrack(pub.track)
		if err != nil {
			slog.Warn("add subscription", "err", err)
			continue
		}
		p.senders[pub] = sender
		p.dirty = true
		go func() {
			for {
				packets, _, err := sender.ReadRTCP()
				if err != nil {
					return
				}
				for _, packet := range packets {
					switch packet.(type) {
					case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
						_ = pub.owner.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: pub.ssrc}})
					}
				}
			}
		}()
	}
	if !p.dirty || p.awaiting || s.onOffer == nil {
		return
	}
	offer, err := p.pc.CreateOffer(nil)
	if err == nil {
		err = p.pc.SetLocalDescription(offer)
	}
	if err != nil {
		slog.Warn("renegotiate subscriptions", "err", err)
		return
	}
	p.dirty = false
	p.awaiting = true
	fn := s.onOffer
	go fn(p.key.session, p.key.participant, p.pc.LocalDescription().SDP)
}

func (s *SFU) HandleAnswer(sessionID, participantID, sdp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[peerKey{sessionID, participantID}]
	if p == nil || !p.awaiting {
		return fmt.Errorf("no pending offer")
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		return fmt.Errorf("set answer: %w", err)
	}
	p.awaiting = false
	// A newly negotiated video subscriber needs a fresh keyframe; the first
	// publisher frame may have arrived before the subscription existed.
	for pub := range p.senders {
		if pub.track.Kind() == webrtc.RTPCodecTypeVideo {
			_ = pub.owner.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: pub.ssrc}})
		}
	}
	for _, candidate := range p.candidates {
		if err := p.pc.AddICECandidate(candidate); err != nil {
			return fmt.Errorf("add queued candidate: %w", err)
		}
	}
	p.candidates = nil
	s.syncPeerLocked(p)
	return nil
}

func (s *SFU) HandleCandidate(sessionID, participantID, candidate string, mid *string, index *uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.peers[peerKey{sessionID, participantID}]
	init := webrtc.ICECandidateInit{Candidate: candidate, SDPMid: mid, SDPMLineIndex: index}
	if p == nil {
		key := peerKey{sessionID, participantID}
		if len(s.pendingCandidates[key]) >= 256 {
			return fmt.Errorf("too many pending ICE candidates")
		}
		s.pendingCandidates[key] = append(s.pendingCandidates[key], init)
		return nil
	}
	if p.pc.RemoteDescription() == nil {
		p.candidates = append(p.candidates, init)
		return nil
	}
	return p.pc.AddICECandidate(init)
}

func (s *SFU) forward(p *peer, remote *webrtc.TrackRemote) {
	s.mu.Lock()
	source := s.sourceOfLocked(p.key, remote.ID())
	s.mu.Unlock()
	streamID := p.key.participant
	if source == SourceScreen {
		streamID += ScreenStreamSuffix
	}
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, remote.ID(), streamID)
	if err != nil {
		slog.Warn("create forwarded track", "err", err)
		return
	}
	pub := &publication{owner: p, track: local, ssrc: uint32(remote.SSRC()), source: source}
	s.mu.Lock()
	if s.peers[p.key] != p {
		s.mu.Unlock()
		return
	}
	s.tracks[pub] = struct{}{}
	for _, other := range s.peers {
		if other.key.session == p.key.session {
			s.syncPeerLocked(other)
		}
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.tracks, pub)
		for _, other := range s.peers {
			if other.key.session == p.key.session {
				s.syncPeerLocked(other)
			}
		}
	}()
	_ = p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: pub.ssrc}})
	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			if err != io.EOF {
				slog.Debug("read publisher RTP", "err", err)
			}
			return
		}
		_ = local.WriteRTP(packet)
		// Only the microphone is the participant's alignment reference;
		// screen/system audio must never be written into it.
		if remote.Kind() == webrtc.RTPCodecTypeAudio && source == SourceCamera && s.RefRec != nil {
			_ = s.RefRec.WriteRTP(p.key.session, p.key.participant, packet)
		}
	}
}

func (s *SFU) OnParticipantLeave(sessionID, participantID string) {
	s.mu.Lock()
	p := s.peers[peerKey{sessionID, participantID}]
	delete(s.pendingCandidates, peerKey{sessionID, participantID})
	delete(s.screens, peerKey{sessionID, participantID})
	delete(s.publishOnly, peerKey{sessionID, participantID})
	s.mu.Unlock()
	if p != nil {
		s.removePeer(p)
	}
}

// detachPeerLocked unregisters p and its publications and renegotiates the
// remaining peers in its session so they drop its tracks. The caller closes
// p.pc outside the lock.
func (s *SFU) detachPeerLocked(p *peer) {
	delete(s.peers, p.key)
	for pub := range s.tracks {
		if pub.owner == p {
			delete(s.tracks, pub)
		}
	}
	for _, other := range s.peers {
		if other.key.session == p.key.session {
			s.syncPeerLocked(other)
		}
	}
}

func (s *SFU) removePeer(p *peer) {
	s.mu.Lock()
	if s.peers[p.key] != p {
		s.mu.Unlock()
		return
	}
	s.detachPeerLocked(p)
	s.mu.Unlock()
	_ = p.pc.Close()
	// The reference recording stays open until the take stops: a
	// reconnecting participant continues it (with a new SSRC).
}
