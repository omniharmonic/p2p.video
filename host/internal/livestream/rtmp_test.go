package livestream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		d                        Destination
		addr, app, stream, tcURL string
		secure                   bool
	}{
		// Platform presets: server URL verbatim as the app, key separate.
		{Destination{URL: "rtmps://a.rtmps.youtube.com/live2", Key: "yt-key"}, "a.rtmps.youtube.com:443", "live2", "yt-key", "rtmps://a.rtmps.youtube.com/live2", true},
		{Destination{URL: "rtmp://a.rtmp.youtube.com/live2/", Key: "yt-key"}, "a.rtmp.youtube.com:1935", "live2", "yt-key", "rtmp://a.rtmp.youtube.com/live2", false},
		{Destination{URL: "rtmps://ingest.global-contribute.live-video.net/app", Key: "live_1_abc"}, "ingest.global-contribute.live-video.net:443", "app", "live_1_abc", "rtmps://ingest.global-contribute.live-video.net/app", true},
		// Facebook keys carry their own query string.
		{Destination{URL: "rtmps://live-api-s.facebook.com:443/rtmp/", Key: "FB-1-0-Ab?s_bl=1&s_sw=0"}, "live-api-s.facebook.com:443", "rtmp", "FB-1-0-Ab?s_bl=1&s_sw=0", "rtmps://live-api-s.facebook.com:443/rtmp", true},
		// A query on the server URL belongs to the app (YouTube backup ingest).
		{Destination{URL: "rtmp://b.rtmp.youtube.com/live2?backup=1", Key: "k"}, "b.rtmp.youtube.com:1935", "live2?backup=1", "k", "rtmp://b.rtmp.youtube.com/live2?backup=1", false},
		// No separate key: the last path segment is the key.
		{Destination{URL: "rtmp://127.0.0.1:19350/live/inline-key?token=z"}, "127.0.0.1:19350", "live", "inline-key?token=z", "rtmp://127.0.0.1:19350/live", false},
		{Destination{URL: "rtmp://owncast.local/live/sub/key"}, "owncast.local:1935", "live/sub", "key", "rtmp://owncast.local/live/sub", false},
		{Destination{URL: "rtmps://[::1]:8443/app", Key: "k"}, "[::1]:8443", "app", "k", "rtmps://[::1]:8443/app", true},
	}
	for _, c := range cases {
		got, err := parseTarget(c.d)
		if err != nil {
			t.Errorf("%s: %v", c.d.URL, err)
			continue
		}
		if got.addr != c.addr || got.app != c.app || got.stream != c.stream || got.tcURL != c.tcURL || got.secure != c.secure {
			t.Errorf("%s: got %+v", c.d.URL, got)
		}
	}
	for _, d := range []Destination{
		{URL: ""}, {URL: "http://x/live", Key: "k"}, {URL: "rtmp:///live", Key: "k"}, {URL: "rtmp://h", Key: "k"},
		{URL: "rtmp://h/live"}, {URL: "rtmp://h/live/"}, {URL: "rtmp://user:pw@h/live", Key: "k"}, {URL: "rtmp://h/live#x", Key: "k"},
		{URL: "rtmp://h/live", Key: "k ey"}, {URL: "rtmp://h/live", Key: "k\n"}, {URL: "rtmp://h/live", Key: "kë"},
		{URL: "rtmp://h:99999/live", Key: "k"}, {URL: "rtmp://h/?x=1", Key: "k"}, {URL: " rtmp://h/li ve", Key: "k"},
	} {
		if _, err := parseTarget(d); !errors.Is(err, errBadDestination) {
			t.Errorf("%q/%q accepted: %v", d.URL, d.Key, err)
		} else if len(d.Key) > 2 && strings.Contains(err.Error(), d.Key) {
			t.Errorf("error echoes the key: %v", err)
		}
	}
}

func TestAMFRoundTrip(t *testing.T) {
	enc := amfEncode("connect", 1.0, amfObj{"app": "live2", "tcUrl": "rtmp://h/live2", "n": 3.5, "ok": true}, nil)
	got, err := amfDecodeAll(enc)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{"connect", 1.0, map[string]any{"app": "live2", "tcUrl": "rtmp://h/live2", "n": 3.5, "ok": true}, nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	// ECMA array and strict array, as servers send in onMetaData/_result.
	ecma := []byte{amfECMAArray, 0, 0, 0, 1, 0, 1, 'x', amfNumber, 0x3f, 0xf0, 0, 0, 0, 0, 0, 0, 0, 0, amfObjectEnd}
	strict := []byte{amfStrictArray, 0, 0, 0, 2, amfNull, amfBoolean, 1}
	got, err = amfDecodeAll(append(ecma, strict...))
	if err != nil || !reflect.DeepEqual(got, []any{map[string]any{"x": 1.0}, []any{nil, true}}) {
		t.Fatalf("got %#v, %v", got, err)
	}
	for _, bad := range [][]byte{{amfString, 0, 9, 'a'}, {amfNumber, 1}, {0x42}, {amfObject, 0, 1}} {
		if _, err := amfDecodeAll(bad); err == nil {
			t.Errorf("% x accepted", bad)
		}
	}
}

func TestFLVReaderAndClassification(t *testing.T) {
	stream := flvStart()
	stream = append(stream, flvTagBytes(tagVideo, 0x01000010, fxKey(1))...) // timestamp with extension byte
	stream = append(stream, flvTagBytes(tagVideo, 40, fxInter(2))...)
	r := bytes.NewReader(stream)
	if err := readFLVHeader(r); err != nil {
		t.Fatal(err)
	}
	var tags []flvTag
	for {
		tag, err := readFLVTag(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		tags = append(tags, tag)
	}
	if len(tags) != 5 || tags[0].kind != tagScript || !tags[1].isSequenceHeader() || !tags[2].isSequenceHeader() {
		t.Fatalf("tags %+v", tags)
	}
	if !tags[3].isKeyframe() || tags[3].isSequenceHeader() || tags[3].ts != 0x01000010 || tags[4].isKeyframe() {
		t.Fatalf("media %+v", tags[3:])
	}
	if (flvTag{kind: tagAudio, data: fxAudio(1)}).isSequenceHeader() {
		t.Fatal("raw AAC treated as config")
	}
	// Enhanced RTMP (ex-header): keyframe + SequenceStart.
	if ex := (flvTag{kind: tagVideo, data: []byte{0x90, 'h', 'v', 'c', '1'}}); !ex.isKeyframe() || !ex.isSequenceHeader() {
		t.Fatal("enhanced FLV sequence start not recognised")
	}
	if err := readFLVHeader(bytes.NewReader([]byte("WEBM\x01\x05\x00\x00\x00\x09\x00\x00\x00\x00"))); !errors.Is(err, errNotFLV) {
		t.Fatalf("non-FLV accepted: %v", err)
	}
	huge := []byte{tagVideo, 0xff, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0}
	if _, err := readFLVTag(bytes.NewReader(huge)); !errors.Is(err, errNotFLV) {
		t.Fatalf("oversized tag accepted: %v", err)
	}
}

// Messages larger than the chunk size, and with extended timestamps, survive
// chunking and reassembly.
func TestRTMPChunkRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w := &rtmpConn{conn: a, w: bufio.NewWriter(a)}
	r := &rtmpConn{conn: b, r: bufio.NewReader(b), inChunk: outChunkSize, chunks: map[uint32]*chunkState{}}
	big := bytes.Repeat([]byte{0xab}, 3*outChunkSize+17)
	msgs := []struct {
		csid uint32
		typ  byte
		ts   uint32
		body []byte
	}{
		{csidVideo, tagVideo, 33, big},
		{csidAudio, tagAudio, 0x01000000, []byte{0xaf, 1, 2}},
		{csidVideo, tagVideo, 0x01000021, big[:outChunkSize]},
		{csidCommand, msgAMF0Command, 0, amfEncode("onStatus", 0.0, nil)},
	}
	go func() {
		for _, m := range msgs {
			if err := w.writeMessage(m.csid, m.typ, m.ts, 1, m.body); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i, m := range msgs {
		typ, ts, body, err := r.readMessage()
		if err != nil {
			t.Fatal(err)
		}
		if typ != m.typ || ts != m.ts || !bytes.Equal(body, m.body) {
			t.Fatalf("message %d: type %d ts %#x len %d", i, typ, ts, len(body))
		}
	}
}

// scriptedServer is a minimal RTMP server for unit tests: handshake, then a
// handler per command.
func scriptedServer(t *testing.T, onPublish func(c *rtmpConn, txn float64, stream string), media chan<- []byte) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		nc, err := l.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		c := &rtmpConn{conn: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc), inChunk: 128, chunks: map[uint32]*chunkState{}}
		c0c1 := make([]byte, 1+handshakeSize)
		if _, err := io.ReadFull(c.r, c0c1); err != nil {
			return
		}
		s := append([]byte{rtmpVersion}, make([]byte, handshakeSize)...)
		s = append(s, c0c1[1:]...) // S2 echoes C1
		if _, err := nc.Write(s); err != nil {
			return
		}
		c2 := make([]byte, handshakeSize)
		if _, err := io.ReadFull(c.r, c2); err != nil || !bytes.Equal(c2, make([]byte, handshakeSize)) {
			t.Errorf("C2 must echo S1: %v", err)
			return
		}
		// Ask the client to answer a ping; a dead client would stall here.
		_ = c.writeMessage(csidControl, msgUserControl, 0, 0, []byte{0, userPingRequest, 0, 0, 0, 7})
		for {
			typ, _, body, err := c.readMessage()
			if err != nil {
				return
			}
			switch typ {
			case msgUserControl:
				if binary.BigEndian.Uint16(body) != userPingResponse {
					t.Errorf("unexpected user control % x", body)
				}
			case msgAMF0Command:
				vals, _ := amfDecodeAll(body)
				name, _ := vals[0].(string)
				txn, _ := vals[1].(float64)
				switch name {
				case "connect":
					obj, _ := vals[2].(map[string]any)
					if obj["app"] != "live" || obj["tcUrl"] != "rtmp://"+l.Addr().String()+"/live" {
						t.Errorf("connect %v", obj)
					}
					_ = c.writeMessage(csidCommand, msgAMF0Command, 0, 0, amfEncode("_result", txn, nil, amfObj{"code": "NetConnection.Connect.Success", "level": "status"}))
				case "createStream":
					_ = c.writeMessage(csidCommand, msgAMF0Command, 0, 0, amfEncode("_result", txn, nil, 1.0))
				case "publish":
					stream, _ := vals[3].(string)
					onPublish(c, txn, stream)
				}
			case tagVideo, tagAudio, msgAMF0Data:
				if media != nil {
					media <- append([]byte{typ}, body...)
				}
			}
		}
	}()
	return l.Addr().String()
}

func TestRTMPPublishHandshakeAndMedia(t *testing.T) {
	media := make(chan []byte, 8)
	addr := scriptedServer(t, func(c *rtmpConn, txn float64, stream string) {
		if stream != "the-key" {
			t.Errorf("publish name %q", stream)
		}
		_ = c.writeMessage(csidCommand, msgAMF0Command, 0, 1, amfEncode("onStatus", 0.0, nil, amfObj{"code": "NetStream.Publish.Start", "level": "status"}))
	}, media)
	target, err := parseTarget(Destination{URL: "rtmp://" + addr + "/live", Key: "the-key"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := dialRTMP(context.Background(), target, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.writeTag(tagScript, 0, fxMeta); err != nil {
		t.Fatal(err)
	}
	if err := c.writeTag(tagVideo, 0, fxKey(1)); err != nil {
		t.Fatal(err)
	}
	meta := <-media
	vals, _ := amfDecodeAll(meta[1:])
	if meta[0] != msgAMF0Data || len(vals) < 2 || vals[0] != "@setDataFrame" || vals[1] != "onMetaData" {
		t.Fatalf("metadata %v", vals)
	}
	if v := <-media; v[0] != tagVideo || !bytes.Equal(v[1:], fxKey(1)) {
		t.Fatalf("video % x", v)
	}
}

func TestRTMPPublishRejected(t *testing.T) {
	addr := scriptedServer(t, func(c *rtmpConn, txn float64, stream string) {
		_ = c.writeMessage(csidCommand, msgAMF0Command, 0, 1, amfEncode("onStatus", 0.0, nil,
			amfObj{"code": "NetStream.Publish.BadName", "level": "error", "description": "bad stream " + stream}))
	}, nil)
	target, _ := parseTarget(Destination{URL: "rtmp://" + addr + "/live", Key: "wrong-key"})
	_, err := dialRTMP(context.Background(), target, nil, time.Second)
	if !errors.Is(err, errRejected) || !strings.Contains(err.Error(), "NetStream.Publish.BadName") {
		t.Fatalf("rejection not reported: %v", err)
	}
	// The server echoed the key; the Redactor removes it before display.
	r := NewRedactor([]Destination{{URL: "rtmp://" + addr + "/live", Key: "wrong-key"}})
	if out := r.Redact(err.Error()); strings.Contains(out, "wrong-key") {
		t.Fatalf("not redacted: %s", out)
	}
}

func TestRTMPDialTimesOutOnSilentServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	target, _ := parseTarget(Destination{URL: "rtmp://" + l.Addr().String() + "/live", Key: "k"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := dialRTMP(ctx, target, nil, time.Second); err == nil {
		t.Fatal("silent server accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("dial ignored its context")
	}
}
