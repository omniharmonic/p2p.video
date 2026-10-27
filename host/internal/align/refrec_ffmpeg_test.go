package align

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/pion/rtp"
)

// End-to-end through the real codec path [SL-P0-10]: a speech-like WAV is
// encoded by libopus (20 ms frames, as WebRTC sends), each Opus packet is
// written through refrec with a known host arrival time, and the resulting
// Ogg + packet log are decoded by ffmpeg and verified against the WAV placed
// with injected clock errors. Covers the Ogg pre-skip, packet mapping and
// the codec's own delay, which belongs to the send path.

func writeWAV(t *testing.T, path string, x []float64) {
	t.Helper()
	data := make([]byte, 44+2*len(x))
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+2*len(x)))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 48000)
	binary.LittleEndian.PutUint32(data[28:], 96000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(2*len(x)))
	for i, v := range x {
		binary.LittleEndian.PutUint16(data[44+2*i:], uint16(int16(math.Max(-1, math.Min(1, v))*32000)))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// oggPackets returns the packets of an Ogg stream (lacing reassembled).
func oggPackets(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	var cur []byte
	for len(b) >= 27 {
		if string(b[:4]) != "OggS" {
			t.Fatal("bad ogg page")
		}
		nseg := int(b[26])
		segs := b[27 : 27+nseg]
		p := b[27+nseg:]
		off := 0
		for _, l := range segs {
			cur = append(cur, p[off:off+int(l)]...)
			off += int(l)
			if l < 255 {
				out = append(out, cur)
				cur = nil
			}
		}
		b = p[off:]
	}
	return out
}

func TestReferenceThroughOpusAndOgg(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	const dur = 70.0
	src := newSpeechSource(11, 0, dur+1)
	x := make([]float64, int(dur*48000))
	for i := range x {
		x[i] = 1.5 * src.at(float64(i)/48000*1e6)
	}
	wav := filepath.Join(dir, "mic.wav")
	writeWAV(t, wav, x)
	opus := filepath.Join(dir, "enc.ogg")
	if out, err := exec.Command(ff, "-v", "error", "-y", "-i", wav, "-c:a", "libopus", "-b:a", "48k", "-frame_duration", "20", "-application", "voip", opus).CombinedOutput(); err != nil {
		t.Skipf("no libopus encoder: %v %s", err, out)
	}
	raw, err := os.ReadFile(opus)
	if err != nil {
		t.Fatal(err)
	}
	packets := oggPackets(t, raw)[2:] // skip OpusHead, OpusTags

	const takeStart = 10_000_000 // host µs
	const trueOffset = 0.25      // the WAV's first sample is captured at takeStart+0.25 s
	const sendMs = 28.0
	m := refrec.NewManager()
	m.Lossless = true
	refs := filepath.Join(dir, "refs")
	if err := m.StartTake("s1", refs); err != nil {
		t.Fatal(err)
	}
	var arrival int64
	m.SetClock(func() int64 { return arrival })
	var ts uint32 = 4_000_000_000
	for i, p := range packets {
		arrival = takeStart + int64(trueOffset*1e6) + int64(i)*20_000 + int64(sendMs*1000) + int64((i*7919)%5000) // jitter 0-5 ms
		if err := m.WriteRTP("s1", "p1", &rtp.Packet{Header: rtp.Header{SSRC: 9, Timestamp: ts, SequenceNumber: uint16(i)}, Payload: p}); err != nil {
			t.Fatal(err)
		}
		ts += 960
	}
	m.StopTake("s1")

	pkts, err := refrec.ReadPacketLog(filepath.Join(refs, "p1.pkt"))
	if err != nil || len(pkts) != len(packets) {
		t.Fatalf("packet log: %d of %d, %v", len(pkts), len(packets), err)
	}
	am, err := FitArrival(pkts)
	if err != nil {
		t.Fatal(err)
	}
	runner := &ffmpeg.Runner{FFmpegPath: ff}
	cfg := DefaultAcousticConfig()
	cfg.SendPathMs = sendMs
	cfg.MaxWindows = 6
	first, last := pkts[0], pkts[len(pkts)-1]
	for _, injected := range []float64{0, 45} {
		res := VerifyAcoustic(AcousticInput{
			OffsetSec: trueOffset - injected/1000, DriftBeta: 1, TakeStartUs: takeStart, DurationSec: dur,
			DecodeLocal: func(start, d float64) ([]float64, error) {
				return runner.ExtractAudioWindow(wav, start, d, cfg.Rate)
			},
			Reference: func(ivs []HostInterval) ([][]float64, []float64, error) {
				var blocks [][]float32
				if err := runner.StreamAudio(filepath.Join(refs, "p1.ogg"), cfg.Rate, func(b []float32) { blocks = append(blocks, append([]float32(nil), b...)) }); err != nil {
					return nil, nil, err
				}
				i := 0
				out, cov := ReferenceWindows(func() ([]float32, bool) {
					if i >= len(blocks) {
						return nil, false
					}
					i++
					return blocks[i-1], true
				}, pkts, am, cfg.Rate, ivs)
				return out, cov, nil
			},
			RefStartUs: float64(first.ArrivalUs), RefEndUs: float64(last.ArrivalUs),
		}, cfg)
		t.Logf("injected %v ms: %s error %.2f ms delay %.2f spread %.2f used %d/%d", injected, res.Status, res.ErrorMs, res.DelayMs, res.SpreadMs, res.UsedWindows, res.Windows)
		// libopus' encoder delay (312 samples, 6.5 ms) is part of what the
		// reference adds on top of the arrival time.
		if res.UsedWindows < 4 || math.Abs(res.ErrorMs-injected-6.5) > 1 {
			t.Fatalf("injected %v ms: %+v", injected, res)
		}
	}
}
