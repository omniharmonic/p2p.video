package livestream

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// The bitrate shown is the encoder's FLV output rate, measured as the daemon
// reads ffmpeg's stdout (source "output"); before any output it falls back
// to the ingest rate (source "ingest") [SL-P3-2, SL-P3-3].
func TestStatusBitrateFromIngestAndOutput(t *testing.T) {
	var logs lockedBuffer
	l := newFakeLauncher()
	clk := &fakeClock{t: time.Unix(1000, 0)}
	m, _ := newTestManager(t, l, &logs)
	m.opts.Now = clk.now
	if _, err := m.Start("b1", testRequest()); err != nil {
		t.Fatal(err)
	}
	src := newFrameSource()
	done := make(chan error, 1)
	go func() { done <- m.Ingest("b1", src.next, src.abort) }()
	f := <-l.ready

	// 1 s of ingest at 1.6 Mbit/s: 8 slices of 25_000 bytes, 125 ms apart.
	chunk := bytes.Repeat([]byte{1}, 25_000)
	sent := 0
	for i := 0; i < 8; i++ {
		clk.add(125 * time.Millisecond)
		src.ch <- chunk
		sent += len(chunk)
		waitFor(t, "chunk", func() bool { return len(f.received()) == sent })
	}
	_, _ = io.WriteString(f.progress, "frame=30\nfps=30.0\nbitrate=N/A\ntotal_size=N/A\nout_time_us=1000000\nspeed=1x\nprogress=continue\n")
	waitFor(t, "live", func() bool { s, _ := m.Status("b1"); return s.State == StateLive })
	s, _ := m.Status("b1")
	if s.OutputKbps != 0 {
		t.Fatalf("no encoder output yet: %+v", s)
	}
	if !near(s.IngestKbps, 1600) || !near(s.BitrateKbps, 1600) || s.BitrateSource != "ingest" {
		t.Fatalf("ingest bitrate: ingest=%v bitrate=%v source=%q", s.IngestKbps, s.BitrateKbps, s.BitrateSource)
	}

	// Encoder output: a tag, then 1 s later 312_500 bytes on the wire
	// (11-byte header + body + 4-byte size) = 2500 kbps.
	_, _ = f.flv.Write(flvHeader())
	_, _ = f.flv.Write(flvTagBytes(tagScript, 0, fxMeta))
	waitFor(t, "first tag", func() bool { return m.flvBytes("b1") > 0 })
	clk.add(time.Second)
	_, _ = f.flv.Write(flvTagBytes(tagVideo, 1000, bytes.Repeat([]byte{0x27}, 312_500-15)))
	waitFor(t, "second tag", func() bool { s, _ := m.Status("b1"); return s.OutputKbps > 0 })
	s, _ = m.Status("b1")
	if !near(s.OutputKbps, 2500) || !near(s.BitrateKbps, 2500) || s.BitrateSource != "output" {
		t.Fatalf("output bitrate: output=%v bitrate=%v source=%q", s.OutputKbps, s.BitrateKbps, s.BitrateSource)
	}

	// Ingest stalls: both windows drain and the bitrate falls to 0.
	clk.add(6 * time.Second)
	s, _ = m.Status("b1")
	if s.IngestKbps != 0 || s.OutputKbps != 0 || s.BitrateKbps != 0 || s.BitrateSource != "" {
		t.Fatalf("stalled stream still reports a rate: %+v", s)
	}

	if _, err := m.Stop("b1"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s, _ = m.Status("b1")
	if s.BitrateKbps != 0 || s.IngestKbps != 0 {
		t.Fatalf("stopped stream reports a rate: %+v", s)
	}
}

func (m *Manager) flvBytes(sess string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[sess].flvBytes
}
