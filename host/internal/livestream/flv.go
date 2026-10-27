package livestream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FLV tag types (also the RTMP message type ids for the same payloads).
const (
	tagAudio  byte = 8
	tagVideo  byte = 9
	tagScript byte = 18
)

// maxTagSize bounds one FLV tag. The format allows 16 MiB; a 1080p keyframe
// at the presets' bitrates is a few hundred KiB.
const maxTagSize = 8 << 20

var errNotFLV = errors.New("encoder output is not FLV")

// flvTag is one FLV tag: its type, millisecond timestamp and body. The body
// is exactly the payload of the matching RTMP message.
type flvTag struct {
	kind byte
	ts   uint32
	data []byte
}

// readFLVHeader consumes the 9-byte file header and PreviousTagSize0.
func readFLVHeader(r io.Reader) error {
	var h [13]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	if h[0] != 'F' || h[1] != 'L' || h[2] != 'V' {
		return errNotFLV
	}
	if off := binary.BigEndian.Uint32(h[5:9]); off != 9 {
		return fmt.Errorf("%w: header size %d", errNotFLV, off)
	}
	return nil
}

// readFLVTag reads one tag and its trailing PreviousTagSize.
func readFLVTag(r io.Reader) (flvTag, error) {
	var h [11]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return flvTag{}, err
	}
	kind := h[0] & 0x1f
	size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if size > maxTagSize {
		return flvTag{}, fmt.Errorf("%w: %d-byte tag", errNotFLV, size)
	}
	ts := uint32(h[4])<<16 | uint32(h[5])<<8 | uint32(h[6]) | uint32(h[7])<<24
	data := make([]byte, size+4)
	if _, err := io.ReadFull(r, data); err != nil {
		return flvTag{}, err
	}
	return flvTag{kind: kind, ts: ts, data: data[:size]}, nil
}

// isKeyframe reports a video tag that starts a GOP (legacy and enhanced FLV).
func (t flvTag) isKeyframe() bool {
	if t.kind != tagVideo || len(t.data) == 0 {
		return false
	}
	return (t.data[0]>>4)&0x7 == 1
}

// isSequenceHeader reports codec configuration (AVC decoder configuration
// record or AAC AudioSpecificConfig), which every new RTMP connection needs
// before media.
func (t flvTag) isSequenceHeader() bool {
	if len(t.data) < 2 {
		return false
	}
	switch t.kind {
	case tagVideo:
		if t.data[0]&0x80 != 0 { // enhanced RTMP: PacketTypeSequenceStart
			return t.data[0]&0x0f == 0
		}
		return t.data[0]&0x0f == 7 && t.data[1] == 0 // AVC, AVCPacketType 0
	case tagAudio:
		return t.data[0]>>4 == 10 && t.data[1] == 0 // AAC, AACPacketType 0
	}
	return false
}

// streamInit is what a destination needs before the first keyframe: the
// metadata and both sequence headers of the current encoder run.
type streamInit struct {
	epoch    int // increments whenever the encoder restarts
	meta     []byte
	videoSeq []byte
	audioSeq []byte
}
