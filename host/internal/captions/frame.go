// Package captions relays call audio from the host's browser to an on-device
// speech recognizer (the "caption provider", normally the p2p.video Mac app)
// and fans recognized captions out to the call [SL-P4-2].
//
// Captions are deliberately separate from recording: this package never reads
// or writes takes, tracks, uploads or exports, and nothing in the recording
// pipeline reads from it (AGENTS.md guardrail; see captions_guardrail_test.go).
// Audio and caption text are never logged.
//
// The wire protocol is specified in docs/integrations/CAPTIONS.md.
package captions

import (
	"errors"
	"time"
)

// Audio framing shared by the ingest socket (browser → daemon) and the
// provider socket (daemon → recognizer). One binary WebSocket message is:
//
//	byte 0      FrameVersion (1)
//	byte 1      N, the stream key length (1..MaxKeyLen)
//	bytes 2..   the stream key, ASCII [A-Za-z0-9_.:-]
//	rest        PCM: signed 16-bit little-endian, mono, SampleRate Hz
//
// On ingest the key is the speaking participant's ID; towards the provider it
// is an opaque daemon-assigned stream ID, so the recognizer never learns who
// is speaking.
const (
	FrameVersion    = 1
	SampleRate      = 16000
	MaxKeyLen       = 64
	MaxFrameSamples = SampleRate // at most 1 s of audio per frame
	MaxFrameBytes   = 2 + MaxKeyLen + 2*MaxFrameSamples
)

var (
	ErrFrameVersion = errors.New("captions: unsupported frame version")
	ErrFrameKey     = errors.New("captions: invalid frame stream key")
	ErrFramePCM     = errors.New("captions: invalid frame audio length")
)

// EncodeFrame builds a binary audio frame. It does not validate its input;
// callers pass keys they generated or already decoded.
func EncodeFrame(key string, pcm []byte) []byte {
	out := make([]byte, 0, 2+len(key)+len(pcm))
	out = append(out, FrameVersion, byte(len(key)))
	out = append(out, key...)
	return append(out, pcm...)
}

// DecodeFrame validates and splits a binary audio frame. The returned PCM
// aliases b.
func DecodeFrame(b []byte) (key string, pcm []byte, err error) {
	if len(b) < 2 || b[0] != FrameVersion {
		return "", nil, ErrFrameVersion
	}
	n := int(b[1])
	if n == 0 || n > MaxKeyLen || len(b) < 2+n {
		return "", nil, ErrFrameKey
	}
	k := b[2 : 2+n]
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return "", nil, ErrFrameKey
		}
	}
	pcm = b[2+n:]
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > 2*MaxFrameSamples {
		return "", nil, ErrFramePCM
	}
	return string(k), pcm, nil
}

// samplesDuration converts a sample count at SampleRate to a duration.
func samplesDuration(samples int64) time.Duration {
	return time.Duration(samples) * time.Second / SampleRate
}
