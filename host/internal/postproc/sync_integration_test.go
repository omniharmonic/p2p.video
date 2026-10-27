package postproc

import (
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Decode the actual deliverables and place their observed flash/click landmarks
// using the exported timeline, independently of manifest offsets. The two-minute
// fixture makes missing or reversed drift correction exceed several video frames.
func TestPostprocExportPreservesFlashClickSyncAcrossClockDrift(t *testing.T) {
	flashClickSync(t, "")
}

// The ProRes Proxy export codec [SL-P0-9] must place media identically.
func TestProResExportPreservesFlashClickSyncAcrossClockDrift(t *testing.T) {
	flashClickSync(t, `{"export_codec":"prores_proxy"}`)
}

func flashClickSync(t *testing.T, preset string) {
	if testing.Short() {
		t.Skip("two-minute synthetic media integration")
	}
	runner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	runner.HasVideoToolbox = os.Getenv("STUDIOLINK_TEST_NATIVE_ENCODER") == "1" && runner.HasVideoToolbox
	fixtureEncoder := []string{"-c:v", "libx264", "-preset", "ultrafast"}
	if runner.HasVideoToolbox {
		fixtureEncoder = []string{"-c:v", "h264_videotoolbox", "-b:v", "1M"}
	}
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	session := &store.Session{ID: "sync", Title: "Synthetic sync", CreatedAt: now, PresetJSON: preset}
	take := &store.Take{ID: "take", SessionID: session.ID, Number: 1, StartAtHostUs: 1000000, Status: "stopped", WallClockStart: now}
	for _, err = range []error{
		st.CreateSession(session), st.CreateTake(take),
		st.CreateParticipant(&store.Participant{ID: "phone", SessionID: session.ID, Name: "Synthetic phone", Role: "guest", Status: "admitted", JoinedAt: now}),
		st.CreateSource(&store.Source{ID: "camera", ParticipantID: "phone", Kind: "camera", Label: "Portrait camera"}),
		st.CreateSource(&store.Source{ID: "microphone", ParticipantID: "phone", Kind: "mic", Label: "Independent microphone"}),
		st.CreateTrack(&store.Track{ID: "video", TakeID: take.ID, SourceID: "camera", Kind: "video", Mime: "video/mp4", Status: "assembled"}),
		st.CreateTrack(&store.Track{ID: "mic", TakeID: take.ID, SourceID: "microphone", Kind: "audio", Mime: "audio/wav", Status: "assembled"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "raw")
	if err = os.MkdirAll(raw, 0700); err != nil {
		t.Fatal(err)
	}
	const videoBeta = 1.001
	const micBeta = .999
	const videoStart = .3
	const leading = .2
	const micStart = .1
	flashLocal := []float64{2, 116}
	hostCues := []float64{videoStart + (leading+flashLocal[0])*videoBeta, videoStart + (leading+flashLocal[1])*videoBeta}
	fixtureArgs := []string{"-y", "-filter_complex", "color=black:size=90x160:rate=30:duration=119.8,drawbox=color=white:t=fill:enable='between(n,60,62)+between(n,3480,3482)',setpts=PTS+0.2/TB[v];anullsrc=r=48000:cl=mono[a]", "-map", "[v]", "-map", "[a]", "-fps_mode", "passthrough"}
	fixtureArgs = append(fixtureArgs, fixtureEncoder...)
	fixtureArgs = append(fixtureArgs, "-c:a", "aac", "-t", "120", filepath.Join(raw, "video.mp4"))
	if err = runner.Execute(fixtureArgs); err != nil {
		t.Fatal(err)
	}
	clickLocal := []float64{(hostCues[0] - micStart) / micBeta, (hostCues[1] - micStart) / micBeta}
	writeSyntheticClickWAV(t, filepath.Join(raw, "mic.wav"), 120, clickLocal)
	for id, fit := range map[string]struct{ start, beta float64 }{"video": {videoStart, videoBeta}, "mic": {micStart, micBeta}} {
		side := align.Sidecar{TrackID: id, ClockFit: align.ClockFitData{A: 1000000 + fit.start*1e6, B: fit.beta, R2: 1, RttP10Ms: 1}, Events: []align.SyncEvent{{Type: "recorder-start", PerfTime: 0}}}
		data, _ := json.Marshal(side)
		if err = os.WriteFile(filepath.Join(raw, id+".sidecar.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = NewPipeline(st, &config.Config{DataDir: root}, runner).ProcessTake(take.ID); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "out")
	data, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest export.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var video, audio export.ExportSource
	for _, src := range manifest.Sources {
		if src.ID == "video" {
			video = src
		}
		if src.ID == "mic" {
			audio = src
		}
	}
	if video.Width != 90 || video.Height != 160 || audio.FileRelPath == "" {
		t.Fatal("missing portrait video or independent mic")
	}
	xmlData, err := os.ReadFile(filepath.Join(out, "take_01_multicam.fcpxml"))
	if err != nil {
		t.Fatal(err)
	}
	// Match asset URI -> actual camera angle or connected timeline microphone,
	// rather than assuming resource/order IDs.
	type positionedClip struct {
		Ref    string `xml:"ref,attr"`
		Offset string `xml:"offset,attr"`
	}
	var doc struct {
		Assets []struct {
			ID  string `xml:"id,attr"`
			Rep struct {
				Src string `xml:"src,attr"`
			} `xml:"media-rep"`
		} `xml:"resources>asset"`
		VideoClips []positionedClip `xml:"resources>media>multicam>mc-angle>asset-clip"`
		AudioClips []positionedClip `xml:"library>event>project>sequence>spine>mc-clip>asset-clip"`
	}
	if err = xml.Unmarshal(xmlData, &doc); err != nil {
		t.Fatal(err)
	}
	offsets := map[string]float64{}
	for _, a := range doc.Assets {
		for _, c := range append(doc.VideoClips, doc.AudioClips...) {
			if a.ID == c.Ref {
				offsets[strings.TrimPrefix(a.Rep.Src, "./")] = parseSyntheticXMLTime(t, c.Offset)
			}
		}
	}
	videoOffset, vok := offsets[filepath.ToSlash(video.FileRelPath)]
	audioOffset, aok := offsets[filepath.ToSlash(audio.FileRelPath)]
	if !vok || !aok {
		t.Fatal("export lost actual media references")
	}
	// Both editor formats must place the same decoded media on the same frame.
	type legacyTrack struct {
		Clips []struct {
			Start int `xml:"start"`
			File  struct {
				ID string `xml:"id,attr"`
			} `xml:"file"`
		} `xml:"clipitem"`
	}
	var legacy struct {
		Video []legacyTrack `xml:"sequence>media>video>track"`
		Audio []legacyTrack `xml:"sequence>media>audio>track"`
	}
	legacyData, err := os.ReadFile(filepath.Join(out, "take_01_premiere.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if err = xml.Unmarshal(legacyData, &legacy); err != nil {
		t.Fatal(err)
	}
	legacyPositions := map[string]float64{}
	for _, track := range append(legacy.Video, legacy.Audio...) {
		for _, clip := range track.Clips {
			legacyPositions[clip.File.ID] = float64(clip.Start) / 30
		}
	}
	if legacyPositions["file-video"] != videoOffset || legacyPositions["file-audio-mic"] != audioOffset {
		t.Fatal("FCP7 and FCPXML exports disagree on decoded media placement")
	}
	frames, err := exec.Command(runner.FFmpegPath, "-v", "error", "-i", filepath.Join(out, video.FileRelPath), "-vf", "scale=1:1", "-pix_fmt", "gray", "-f", "rawvideo", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	var flashes []float64
	for i, v := range frames {
		if v > 128 && (i == 0 || frames[i-1] <= 128) {
			flashes = append(flashes, float64(i)/30)
		}
	}
	samples, err := runner.ExtractAudioSamples(filepath.Join(out, audio.FileRelPath), 48000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(flashes) != 2 {
		t.Fatalf("decoded %d flash landmarks; want 2", len(flashes))
	}
	for i, expected := range hostCues {
		// Locate the first audible sample within a bounded expected cue window.
		wantSample := int((expected - audioOffset) * 48000)
		click := -1
		for n := wantSample - 4800; n < wantSample+4800 && n < len(samples); n++ {
			if n >= 0 && math.Abs(samples[n]) > .1 {
				click = n
				break
			}
		}
		if click < 0 {
			t.Fatalf("missing decoded audio cue %d", i)
		}
		videoTime := videoOffset + flashes[i]
		audioTime := audioOffset + float64(click)/48000
		vr, ar, av := (videoTime-expected)*1000, (audioTime-expected)*1000, (videoTime-audioTime)*1000
		t.Logf("cue %d at %.3fs: video residual %.3fms; audio residual %.3fms; A/V residual %.3fms", i+1, expected, vr, ar, av)
		if math.Abs(vr) > 34 || math.Abs(ar) > 25 || math.Abs(av) > 50 {
			t.Fatalf("cue %d exceeds frame/audio processing tolerance", i+1)
		}
	}
}

func parseSyntheticXMLTime(t *testing.T, s string) float64 {
	t.Helper()
	parts := strings.Split(strings.TrimSuffix(s, "s"), "/")
	n, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) == 2 {
		d, e := strconv.ParseFloat(parts[1], 64)
		if e != nil {
			t.Fatal(e)
		}
		n /= d
	}
	return n
}

func writeSyntheticClickWAV(t *testing.T, path string, duration float64, cues []float64) {
	t.Helper()
	const rate = 48000
	n := int(duration * rate)
	data := make([]byte, 44+n*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], rate)
	binary.LittleEndian.PutUint32(data[28:], rate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(n*2))
	for _, cue := range cues {
		start := int(math.Round(cue * rate))
		for j := 0; j < 480; j++ {
			i := start + j
			if i >= n {
				t.Fatal(fmt.Errorf("cue beyond fixture"))
			}
			value := int16(20000 * math.Sin(2*math.Pi*1000*float64(j)/rate))
			binary.LittleEndian.PutUint16(data[44+i*2:], uint16(value))
		}
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
