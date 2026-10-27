package livestream

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// A minimal RTMP publisher (Adobe RTMP 1.0 spec): simple handshake, connect,
// releaseStream/FCPublish/createStream/publish, then FLV tag bodies as audio,
// video and data messages. This is the client half only; the daemon never
// listens for RTMP. rtmps:// is the same protocol inside crypto/tls, with the
// system trust store and hostname verification (SNI set to the host).

const (
	rtmpVersion        = 3
	handshakeSize      = 1536
	outChunkSize       = 4096
	maxInChunkSize     = 1 << 24
	maxMessageSize     = 16 << 20
	defaultWindowAck   = 2500000
	csidControl        = 2
	csidCommand        = 3
	csidAudio          = 4
	csidData           = 5
	csidVideo          = 6
	msgSetChunkSize    = 1
	msgAck             = 3
	msgUserControl     = 4
	msgWindowAckSize   = 5
	msgSetPeerBW       = 6
	msgAMF0Data        = 18
	msgAMF0Command     = 20
	userPingRequest    = 6
	userPingResponse   = 7
	flashVer           = "FMLE/3.0 (compatible; p2p.video)"
	rtmpCommandTimeout = 15 * time.Second
)

// errRejected marks a server that answered but refused the stream (wrong
// key, app or permissions); the message is the server's status code.
var errRejected = errors.New("server rejected the stream")

type chunkState struct {
	ts      uint32
	tsDelta uint32
	length  uint32
	typ     byte
	sid     uint32
	extTS   bool
	buf     []byte
}

// rtmpConn is one publishing connection.
type rtmpConn struct {
	conn net.Conn
	r    *bufio.Reader

	wmu          sync.Mutex
	w            *bufio.Writer
	writeTimeout time.Duration

	// reader state (single reader goroutine after publish)
	inChunk   int
	chunks    map[uint32]*chunkState
	bytesIn   uint64
	lastAck   uint64
	windowAck uint64

	streamID uint32
	txn      float64

	closeOnce sync.Once
	done      chan struct{}
	errMu     sync.Mutex
	err       error
}

// dialRTMP connects, handshakes and publishes. The returned connection is
// ready for media; its reader goroutine runs until Close or a server error.
func dialRTMP(ctx context.Context, t rtmpTarget, tlsConf *tls.Config, writeTimeout time.Duration) (*rtmpConn, error) {
	ctx, cancel := context.WithTimeout(ctx, rtmpCommandTimeout)
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	conn := raw
	if t.secure {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if tlsConf != nil {
			cfg = tlsConf.Clone()
		}
		cfg.ServerName = t.host
		tc := tls.Client(raw, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("TLS: %w", err)
		}
		conn = tc
	}
	c := &rtmpConn{conn: conn, r: bufio.NewReaderSize(conn, 64*1024), w: bufio.NewWriterSize(conn, 64*1024),
		writeTimeout: writeTimeout, inChunk: 128, chunks: map[uint32]*chunkState{}, windowAck: defaultWindowAck, done: make(chan struct{})}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err := c.publish(t); err != nil {
		stop()
		_ = conn.Close()
		if ctx.Err() != nil && !errors.Is(err, errRejected) {
			return nil, fmt.Errorf("RTMP handshake timed out: %w", ctx.Err())
		}
		return nil, err
	}
	if !stop() {
		_ = conn.Close()
		return nil, fmt.Errorf("RTMP handshake timed out: %w", ctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	go c.readLoop()
	return c, nil
}

func (c *rtmpConn) handshake() error {
	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = rtmpVersion
	if _, err := rand.Read(c0c1[9:]); err != nil {
		return err
	}
	if _, err := c.conn.Write(c0c1); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	s := make([]byte, 1+2*handshakeSize)
	if _, err := io.ReadFull(c.r, s); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	if s[0] != rtmpVersion {
		return fmt.Errorf("handshake: server speaks RTMP version %d", s[0])
	}
	if _, err := c.conn.Write(s[1 : 1+handshakeSize]); err != nil { // C2 echoes S1
		return fmt.Errorf("handshake: %w", err)
	}
	return nil
}

func (c *rtmpConn) publish(t rtmpTarget) error {
	if err := c.handshake(); err != nil {
		return err
	}
	if err := c.writeMessage(csidControl, msgSetChunkSize, 0, 0, binary.BigEndian.AppendUint32(nil, outChunkSize)); err != nil {
		return err
	}
	if err := c.commandOn(0, "connect", amfObj{"app": t.app, "type": "nonprivate", "flashVer": flashVer, "swfUrl": t.tcURL, "tcUrl": t.tcURL}); err != nil {
		return err
	}
	if _, err := c.awaitResult(1); err != nil {
		return err
	}
	// Some servers answer releaseStream/FCPublish with _error; that is fine.
	_ = c.commandOn(0, "releaseStream", nil, t.stream)
	_ = c.commandOn(0, "FCPublish", nil, t.stream)
	if err := c.commandOn(0, "createStream", nil); err != nil {
		return err
	}
	res, err := c.awaitResult(c.txn)
	if err != nil {
		return err
	}
	if len(res) < 4 {
		return errors.New("createStream: no stream id")
	}
	id, ok := res[3].(float64)
	if !ok || id < 0 {
		return errors.New("createStream: bad stream id")
	}
	c.streamID = uint32(id)
	if err := c.commandOn(c.streamID, "publish", nil, t.stream, "live"); err != nil {
		return err
	}
	return c.awaitPublishStart()
}

// commandOn sends an AMF0 command with the next transaction id.
func (c *rtmpConn) commandOn(streamID uint32, name string, args ...any) error {
	c.txn++
	body := amfEncode(name, c.txn)
	for _, a := range args {
		body = amfAppend(body, a)
	}
	return c.writeMessage(csidCommand, msgAMF0Command, 0, streamID, body)
}

// awaitResult reads until the _result (or _error) for txn arrives.
func (c *rtmpConn) awaitResult(txn float64) ([]any, error) {
	for {
		typ, _, body, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		if typ != msgAMF0Command {
			continue
		}
		vals, _ := amfDecodeAll(body)
		if len(vals) < 2 {
			continue
		}
		name, _ := vals[0].(string)
		id, _ := vals[1].(float64)
		if id != txn {
			continue
		}
		switch name {
		case "_result":
			return vals, nil
		case "_error":
			return nil, fmt.Errorf("%w: %s", errRejected, statusText(vals))
		}
	}
}

func (c *rtmpConn) awaitPublishStart() error {
	for {
		typ, _, body, err := c.readMessage()
		if err != nil {
			return err
		}
		if typ != msgAMF0Command {
			continue
		}
		vals, _ := amfDecodeAll(body)
		if len(vals) == 0 {
			continue
		}
		switch name, _ := vals[0].(string); name {
		case "onStatus":
			code, level := statusFields(vals)
			if code == "NetStream.Publish.Start" {
				return nil
			}
			if level == "error" || strings.Contains(code, "Failed") || strings.Contains(code, "BadName") || strings.Contains(code, "Rejected") {
				return fmt.Errorf("%w: %s", errRejected, statusText(vals))
			}
		case "_error":
			return fmt.Errorf("%w: %s", errRejected, statusText(vals))
		}
	}
}

func statusFields(vals []any) (code, level string) {
	for _, v := range vals {
		if m, ok := v.(map[string]any); ok {
			code, _ = m["code"].(string)
			level, _ = m["level"].(string)
			if code != "" {
				return code, level
			}
		}
	}
	return "", ""
}

// statusText is the server's code (and description) for an error; callers
// redact it, because servers may echo the stream name.
func statusText(vals []any) string {
	for _, v := range vals {
		if m, ok := v.(map[string]any); ok {
			code, _ := m["code"].(string)
			desc, _ := m["description"].(string)
			if len(desc) > 200 {
				desc = desc[:200]
			}
			switch {
			case code != "" && desc != "":
				return code + " (" + desc + ")"
			case code != "":
				return code
			}
		}
	}
	return "no reason given"
}

// readMessage reads chunks until one message completes, handling protocol
// control messages along the way.
func (c *rtmpConn) readMessage() (byte, uint32, []byte, error) {
	for {
		typ, ts, body, err := c.readChunk()
		if err != nil {
			return 0, 0, nil, err
		}
		if body == nil {
			continue
		}
		if err := c.handleControl(typ, body); err != nil {
			return 0, 0, nil, err
		}
		return typ, ts, body, nil
	}
}

func (c *rtmpConn) handleControl(typ byte, body []byte) error {
	switch typ {
	case msgSetChunkSize:
		if len(body) >= 4 {
			n := int(binary.BigEndian.Uint32(body) & 0x7fffffff)
			if n < 1 || n > maxInChunkSize {
				return errors.New("server sent an invalid chunk size")
			}
			c.inChunk = n
		}
	case msgWindowAckSize:
		if len(body) >= 4 {
			c.windowAck = uint64(binary.BigEndian.Uint32(body))
		}
	case msgSetPeerBW:
		if len(body) >= 4 {
			return c.writeMessage(csidControl, msgWindowAckSize, 0, 0, body[:4])
		}
	case msgUserControl:
		if len(body) >= 6 && binary.BigEndian.Uint16(body) == userPingRequest {
			return c.writeMessage(csidControl, msgUserControl, 0, 0, append(binary.BigEndian.AppendUint16(nil, userPingResponse), body[2:6]...))
		}
	}
	return nil
}

// readChunk reads one chunk; it returns a body only when a message completes.
func (c *rtmpConn) readChunk() (byte, uint32, []byte, error) {
	b0, err := c.readByte()
	if err != nil {
		return 0, 0, nil, err
	}
	fmtType := b0 >> 6
	csid := uint32(b0 & 0x3f)
	switch csid {
	case 0:
		b, err := c.readN(1)
		if err != nil {
			return 0, 0, nil, err
		}
		csid = 64 + uint32(b[0])
	case 1:
		b, err := c.readN(2)
		if err != nil {
			return 0, 0, nil, err
		}
		csid = 64 + uint32(b[0]) + uint32(b[1])<<8
	}
	st := c.chunks[csid]
	if st == nil {
		if fmtType != 0 {
			return 0, 0, nil, errors.New("server chunk stream started without a full header")
		}
		st = &chunkState{}
		c.chunks[csid] = st
	}
	newMsg := len(st.buf) == 0
	switch fmtType {
	case 0:
		h, err := c.readN(11)
		if err != nil {
			return 0, 0, nil, err
		}
		st.ts = be24(h[0:3])
		st.length = be24(h[3:6])
		st.typ = h[6]
		st.sid = binary.LittleEndian.Uint32(h[7:11])
		st.tsDelta = 0
		st.extTS = st.ts == 0xffffff
		if st.extTS {
			if st.ts, err = c.readUint32(); err != nil {
				return 0, 0, nil, err
			}
		}
	case 1, 2:
		n := 7
		if fmtType == 2 {
			n = 3
		}
		h, err := c.readN(n)
		if err != nil {
			return 0, 0, nil, err
		}
		st.tsDelta = be24(h[0:3])
		if fmtType == 1 {
			st.length = be24(h[3:6])
			st.typ = h[6]
		}
		st.extTS = st.tsDelta == 0xffffff
		if st.extTS {
			if st.tsDelta, err = c.readUint32(); err != nil {
				return 0, 0, nil, err
			}
		}
		if newMsg {
			st.ts += st.tsDelta
		}
	case 3:
		if st.extTS {
			if _, err := c.readUint32(); err != nil {
				return 0, 0, nil, err
			}
		}
		if newMsg && st.tsDelta != 0 {
			st.ts += st.tsDelta
		}
	}
	if st.length > maxMessageSize {
		return 0, 0, nil, errors.New("server message too large")
	}
	remaining := int(st.length) - len(st.buf)
	n := min(remaining, c.inChunk)
	part, err := c.readN(n)
	if err != nil {
		return 0, 0, nil, err
	}
	st.buf = append(st.buf, part...)
	if len(st.buf) < int(st.length) {
		return 0, 0, nil, nil
	}
	body := st.buf
	st.buf = nil
	if err := c.maybeAck(); err != nil {
		return 0, 0, nil, err
	}
	return st.typ, st.ts, body, nil
}

func be24(b []byte) uint32 { return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]) }

func (c *rtmpConn) readByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.bytesIn++
	}
	return b, err
}

func (c *rtmpConn) readN(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(c.r, b); err != nil {
		return nil, err
	}
	c.bytesIn += uint64(n)
	return b, nil
}

func (c *rtmpConn) readUint32() (uint32, error) {
	b, err := c.readN(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (c *rtmpConn) maybeAck() error {
	if c.windowAck == 0 || c.bytesIn-c.lastAck < c.windowAck {
		return nil
	}
	c.lastAck = c.bytesIn
	return c.writeMessage(csidControl, msgAck, 0, 0, binary.BigEndian.AppendUint32(nil, uint32(c.bytesIn)))
}

// writeMessage chunks one message: a full (type 0) header, then type 3
// continuation headers every outChunkSize bytes.
func (c *rtmpConn) writeMessage(csid uint32, typ byte, ts, streamID uint32, body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.writeTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	ext := ts >= 0xffffff
	var h [16]byte
	h[0] = byte(csid & 0x3f) // csid < 64 for every stream we use
	hts := ts
	if ext {
		hts = 0xffffff
	}
	h[1], h[2], h[3] = byte(hts>>16), byte(hts>>8), byte(hts)
	l := len(body)
	h[4], h[5], h[6] = byte(l>>16), byte(l>>8), byte(l)
	h[7] = typ
	binary.LittleEndian.PutUint32(h[8:12], streamID)
	n := 12
	if ext {
		binary.BigEndian.PutUint32(h[12:16], ts)
		n = 16
	}
	if _, err := c.w.Write(h[:n]); err != nil {
		return err
	}
	for off := 0; ; {
		end := min(off+outChunkSize, len(body))
		if _, err := c.w.Write(body[off:end]); err != nil {
			return err
		}
		off = end
		if off >= len(body) {
			break
		}
		cont := []byte{0xc0 | byte(csid&0x3f)}
		if ext {
			cont = binary.BigEndian.AppendUint32(cont, ts)
		}
		if _, err := c.w.Write(cont); err != nil {
			return err
		}
	}
	return c.w.Flush()
}

// writeTag sends one FLV tag body as the matching RTMP message.
func (c *rtmpConn) writeTag(kind byte, ts uint32, data []byte) error {
	switch kind {
	case tagAudio:
		return c.writeMessage(csidAudio, tagAudio, ts, c.streamID, data)
	case tagVideo:
		return c.writeMessage(csidVideo, tagVideo, ts, c.streamID, data)
	case tagScript:
		// onMetaData is sent as @setDataFrame so the server keeps it.
		body := append(amfEncode("@setDataFrame"), data...)
		return c.writeMessage(csidData, msgAMF0Data, ts, c.streamID, body)
	}
	return nil
}

// readLoop services the server after publish: acks, pings, chunk size, and
// an onStatus error ends the connection.
func (c *rtmpConn) readLoop() {
	for {
		typ, _, body, err := c.readMessage()
		if err != nil {
			c.fail(err)
			return
		}
		if typ != msgAMF0Command {
			continue
		}
		vals, _ := amfDecodeAll(body)
		if len(vals) == 0 {
			continue
		}
		if name, _ := vals[0].(string); name == "onStatus" {
			if code, level := statusFields(vals); level == "error" || code == "NetStream.Publish.Unpublish" || strings.Contains(code, "Failed") {
				c.fail(fmt.Errorf("%w: %s", errRejected, statusText(vals)))
				return
			}
		}
	}
}

func (c *rtmpConn) fail(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
	c.Close()
}

// Err is the reason the connection ended, if it has.
func (c *rtmpConn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// Done is closed when the connection ends.
func (c *rtmpConn) Done() <-chan struct{} { return c.done }

// Close ends the connection at once.
func (c *rtmpConn) Close() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
		close(c.done)
	})
}

// closeGracefully tells the server the stream ended, then closes.
func (c *rtmpConn) closeGracefully() {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.commandOn(0, "deleteStream", nil, float64(c.streamID))
	c.Close()
}
