package sfu

import (
	"strings"
)

// Cross-engine codec guard [SL-P1-12]. The SFU forwards each publisher's RTP
// unchanged, so a subscriber must decode the publisher's codec. Clients
// prefer VP8 (web/src/lib/rtc.ts universalVideoFirst), which all three
// engines decode, but an older client or an engine that refuses
// setCodecPreferences may still publish e.g. Safari's H.264 High profile to a
// Firefox without OpenH264. Adding such a track to that subscriber would fail
// its whole renegotiation (every other subscription with it), so the track is
// skipped for that subscriber only and logged.

// sdpHasCodec reports whether an SDP lists mimeType ("video/VP8") in an
// a=rtpmap line of a media section of the same kind. An SDP that lists no
// codec of that kind at all (no description yet, or no such media section)
// counts as accepting: nothing is known to refuse.
func sdpHasCodec(sdp, mimeType string) bool {
	if strings.TrimSpace(sdp) == "" {
		return true
	}
	kind, name, ok := strings.Cut(mimeType, "/")
	if !ok || name == "" {
		return true
	}
	section, sawKind := "", false
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if m, found := strings.CutPrefix(line, "m="); found {
			section, _, _ = strings.Cut(m, " ")
			continue
		}
		if !strings.EqualFold(section, kind) {
			continue
		}
		rtpmap, found := strings.CutPrefix(line, "a=rtpmap:")
		if !found {
			continue
		}
		_, encoding, _ := strings.Cut(rtpmap, " ")
		codec, _, _ := strings.Cut(encoding, "/")
		if strings.EqualFold(codec, name) {
			return true
		}
		sawKind = true
	}
	return !sawKind
}

// subscriberAcceptsLocked: the subscriber's last negotiated description lists
// the publication's codec. s.mu must be held.
func subscriberAcceptsLocked(p *peer, pub *publication) bool {
	desc := p.pc.RemoteDescription()
	if desc == nil {
		return true
	}
	return sdpHasCodec(desc.SDP, pub.track.Codec().MimeType)
}
