package postproc

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/pion/rtp"
)

// speechWAV writes durSec of speech-like noise bursts (mono 48 kHz 16-bit).
func speechWAV(t *testing.T, path string, durSec float64, seed int64) {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	n := int(durSec * 48000)
	data := make([]byte, 44+2*n)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+2*n))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], 48000)
	binary.LittleEndian.PutUint32(data[28:], 96000)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(2*n))
	var lp1, lp2, hp, env, target float64
	next := 0
	for i := 0; i < n; i++ {
		if i >= next {
			target = 0
			if r.Float64() < 0.65 {
				target = 0.3 + 0.7*r.Float64()
			}
			next = i + int((0.08+0.22*r.Float64())*48000)
		}
		env += (target - env) * 0.002
		lp1 += (r.NormFloat64() - lp1) * 0.35
		lp2 += (lp1 - lp2) * 0.35
		hp += (lp2 - hp) * 0.01
		v := math.Max(-1, math.Min(1, 1.5*(lp2-hp)*env))
		binary.LittleEndian.PutUint16(data[44+2*i:], uint16(int16(v*32000)))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// End to end through ProcessTake [SL-P0-10]: a participant's microphone whose
// clock placement is 60 ms early (e.g. a recorder anchor that misses input
// latency) is corrected from the host's received copy, and the manifest and
// README say so. A second person with an accurate clock is verified and
// left alone; a third with no reference stays clock-based.
func TestProcessTakeVerifiesAndCorrectsFromReference(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic media integration")
	}
	runner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	const takeStart = 10_000_000
	session := &store.Session{ID: "ac", Title: "Acoustic", CreatedAt: now}
	take := &store.Take{ID: "take", SessionID: session.ID, Number: 1, StartAtHostUs: takeStart, Status: "stopped", WallClockStart: now}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateSession(session))
	must(st.CreateTake(take))
	takeDir := filepath.Join(root, "sessions", session.ID, "takes", take.ID)
	raw := filepath.Join(takeDir, "raw")
	must(os.MkdirAll(raw, 0o700))

	people := []struct {
		id      string
		trueOff float64 // seconds after the take mark the recording really starts
		errMs   float64 // clock placement error (content later than placed)
		ref     bool
	}{{"ana", 0.30, 60, true}, {"bo", 0.42, 0, true}, {"cy", 0.10, 0, false}}
	const dur = 45.0
	for i, p := range people {
		must(st.CreateParticipant(&store.Participant{ID: p.id, SessionID: session.ID, Name: strings.ToUpper(p.id[:1]) + p.id[1:], Role: "guest", Status: "admitted", JoinedAt: now}))
		must(st.CreateSource(&store.Source{ID: "mic-" + p.id, ParticipantID: p.id, Kind: "mic", Label: "Mic"}))
		must(st.CreateTrack(&store.Track{ID: "t-" + p.id, TakeID: take.ID, SourceID: "mic-" + p.id, Kind: "audio", Mime: "audio/wav", Status: "assembled"}))
		wav := filepath.Join(raw, "t-"+p.id+".wav")
		speechWAV(t, wav, dur, int64(i+1))
		placed := p.trueOff - p.errMs/1000
		side, _ := json.Marshal(map[string]any{"trackId": "t-" + p.id, "kind": "audio",
			"clockFit": map[string]any{"a": float64(takeStart), "b": 1.0, "r2": 1.0, "rttP10Ms": 2.0},
			"events":   []map[string]any{{"type": "first-sample", "perfTime": placed * 1000, "mediaTimeUs": 0}}})
		must(os.WriteFile(filepath.Join(raw, "t-"+p.id+".sidecar.json"), side, 0o600))
		if !p.ref {
			continue
		}
		// The host's received copy: libopus 20 ms packets through refrec,
		// arriving with the modelled send path (minus the codec's 6.5 ms
		// lookahead, which the reference adds itself) plus jitter.
		enc := filepath.Join(root, p.id+".ogg")
		if out, err := exec.Command(runner.FFmpegPath, "-v", "error", "-y", "-i", wav, "-c:a", "libopus", "-b:a", "48k", "-frame_duration", "20", "-application", "voip", enc).CombinedOutput(); err != nil {
			t.Skipf("no libopus encoder: %v %s", err, out)
		}
		b, _ := os.ReadFile(enc)
		packets := oggPacketsForTest(b)[2:]
		m := refrec.NewManager()
		m.Lossless = true
		m.Lossless = true
		must(m.StartTake(session.ID, filepath.Join(takeDir, "refs")))
		var arrival int64
		m.SetClock(func() int64 { return arrival })
		sendUs := (align.DefaultSendPathMs + 1 - 6.5) * 1000 // + rtt/2 = 1 ms
		for k, pk := range packets {
			arrival = takeStart + int64(p.trueOff*1e6) + int64(k)*20_000 + int64(sendUs) + int64((k*7919)%3000)
			must(m.WriteRTP(session.ID, p.id, &rtp.Packet{Header: rtp.Header{SSRC: 5, Timestamp: uint32(k * 960)}, Payload: pk}))
		}
		m.StopTake(session.ID)
	}

	p := NewPipeline(st, &config.Config{DataDir: root}, runner)
	if err := p.ProcessTake(take.ID); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(takeDir, "out")
	b, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest export.Manifest
	must(json.Unmarshal(b, &manifest))
	by := map[string]export.ExportSource{}
	for _, s := range manifest.Sources {
		by[s.ParticipantID] = s
	}
	ana, bo, cy := by["ana"], by["bo"], by["cy"]
	t.Logf("ana %+v %+v", ana.Acoustic, ana.OffsetSec)
	t.Logf("bo %+v %+v", bo.Acoustic, bo.OffsetSec)
	if ana.Acoustic == nil || ana.Acoustic.Status != align.AcousticCorrected || ana.Method != align.MethodAcoustic || math.Abs(ana.OffsetSec-0.30) > 0.0015 {
		t.Fatalf("ana not corrected to 0.300 s: offset %.4f method %s %+v", ana.OffsetSec, ana.Method, ana.Acoustic)
	}
	if bo.Acoustic == nil || bo.Acoustic.Status != align.AcousticVerified || bo.Method != "sidecar-coarse" || math.Abs(bo.OffsetSec-0.42) > 0.0005 || math.Abs(bo.Acoustic.ErrorMs) > 1.5 {
		t.Fatalf("bo not verified at 0.420 s: offset %.4f %+v", bo.OffsetSec, bo.Acoustic)
	}
	if cy.Acoustic == nil || cy.Acoustic.Status != "no-reference" || cy.Method != "sidecar-coarse" {
		t.Fatalf("cy: %+v", cy.Acoustic)
	}
	if ana.Channels != 1 {
		t.Fatalf("channels %d", ana.Channels)
	}
	readme, _ := os.ReadFile(filepath.Join(outDir, "README.txt"))
	for _, want := range []string{"Ana: corrected acoustically by +6", "Bo: verified acoustically", "Cy: clock-based only (no host copy"} {
		if !strings.Contains(string(readme), want) {
			t.Fatalf("README lacks %q:\n%s", want, readme)
		}
	}
	if strings.Contains(string(readme), "not independently verified") {
		t.Fatal("stale README sync claim")
	}
}

// oggPacketsForTest reassembles the packets of an Ogg stream.
func oggPacketsForTest(b []byte) [][]byte {
	var out [][]byte
	var cur []byte
	for len(b) >= 27 && string(b[:4]) == "OggS" {
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
