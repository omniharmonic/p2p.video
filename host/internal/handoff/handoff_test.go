package handoff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

func manifest() *export.Manifest {
	return &export.Manifest{Schema: export.ManifestSchema, Session: export.SessionMeta{ID: "s1", Title: "Ep"}, Take: 1, FrameRate: "30/1", SampleRate: 48000,
		Sources: []export.ExportSource{
			{ID: "v-ana", Participant: "Ana", ParticipantID: "p1", Kind: "video", FileRelPath: "video/Ana_cam.mov", OffsetSec: 0.1, DurationSec: 60, Method: "clock-only"},
			{ID: "a-ana", Participant: "Ana", ParticipantID: "p1", Kind: "audio", FileRelPath: "audio/Ana_mic.wav", OffsetSec: 0.05, DurationSec: 60, Method: "clock-only", ResidualMs: 1.5},
			{ID: "a-ben", Participant: "Ben", ParticipantID: "p2", Kind: "audio", FileRelPath: "audio/Ben_mic.wav", OffsetSec: 1.25, DurationSec: 58, Method: "clock-only"},
			{ID: "v-scr", Participant: "Ana", ParticipantID: "p1", Kind: "video", Source: export.SourceScreen, FileRelPath: "video/Ana_screen.mov", OffsetSec: 10, DurationSec: 20},
		}}
}

func doc() *transcript.Document {
	segs := transcript.Merge([]transcript.Track{
		{Speaker: "Ana", ParticipantID: "p1", SourceID: "a-ana", OffsetSec: 0.05, Segments: []transcript.Segment{
			{Start: 1, End: 2, Text: "hello there", Words: []transcript.Word{{Word: "hello", Start: 1, End: 1.4, Confidence: 0.9}, {Word: "there", Start: 1.5, End: 2, Confidence: 0.8}}},
			{Start: 2.5, End: 3, Text: "again", Words: []transcript.Word{{Word: "again", Start: 2.5, End: 3, Confidence: 0.7}}},
		}},
		{Speaker: "Ben", ParticipantID: "p2", SourceID: "a-ben", OffsetSec: 1.25, Segments: []transcript.Segment{
			{Start: 0.5, End: 1.5, Text: "hi you"}, // no word timings: 1.75..2.75
		}},
	})
	return &transcript.Document{Schema: transcript.Schema, Segments: segs}
}

func TestBuildDescribesSpeakersSourcesAndAveLayout(t *testing.T) {
	d := Build(Input{Manifest: manifest(), Transcript: doc(), ProgramDurationSec: 59.25, Files: []string{"take_01_multicam.fcpxml", "take_01_premiere.xml"}})
	if d.Schema != Schema || d.Program == nil || d.Program.File != ProgramFile || d.Transcript == nil || d.Transcript.Segments != 3 {
		t.Fatalf("doc %+v", d)
	}
	if len(d.Speakers) != 2 || d.Speakers[0].ID != "S1" || d.Speakers[0].Name != "Ana" || d.Speakers[0].Audio != "audio/Ana_mic.wav" ||
		len(d.Speakers[0].Video) != 1 || d.Speakers[0].Video[0] != "video/Ana_cam.mov" || d.Speakers[1].ID != "S2" || len(d.Speakers[1].Video) != 0 {
		t.Fatalf("speakers %+v", d.Speakers)
	}
	if d.Timeline["durationSec"] != 60.1 {
		t.Fatalf("timeline %+v", d.Timeline)
	}
	if d.Editor["fcpxml"] != "take_01_multicam.fcpxml" || d.Editor["xmeml"] != "take_01_premiere.xml" {
		t.Fatalf("editor %+v", d.Editor)
	}
	if d.Ave.Raw[AveRawProgram] != ProgramFile || d.Ave.Raw["raw/video/Ana_cam.mov"] != "video/Ana_cam.mov" || d.Ave.Transcript == "" || d.Ave.Sync == "" {
		t.Fatalf("ave %+v", d.Ave)
	}
	noProgram := Build(Input{Manifest: manifest()})
	if noProgram.Program != nil || noProgram.Transcript != nil || noProgram.Ave.Sync != "" || noProgram.Ave.Transcript != "" {
		t.Fatalf("without program/transcript: %+v", noProgram)
	}
}

func TestAveTranscriptMatchesAveShapes(t *testing.T) {
	out := AveTranscript(manifest(), doc(), 59.25)
	data, _ := json.Marshal(out)
	var got struct {
		Source   string
		Duration float64
		Speakers map[string]struct {
			Name        string  `json:"name"`
			TalkSeconds float64 `json:"talk_seconds"`
		}
		Words []struct {
			W       string  `json:"w"`
			Start   float64 `json:"start"`
			End     float64 `json:"end"`
			Speaker string  `json:"speaker"`
			Timing  string  `json:"timing"`
		}
		Utterances []struct {
			Speaker   string  `json:"speaker"`
			Start     float64 `json:"start"`
			End       float64 `json:"end"`
			FirstWord int     `json:"first_word"`
			LastWord  int     `json:"last_word"`
			Text      string  `json:"text"`
		}
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "raw/program.wav" || got.Speakers["S1"].Name != "Ana" || got.Speakers["S2"].Name != "Ben" {
		t.Fatalf("header %+v", got)
	}
	// Words interleave by take time: Ana hello(1.05) there(1.55) | Ben hi(1.75) you(2.25) | Ana again(2.55).
	wantWords := []string{"hello", "there", "hi", "you", "again"}
	for i, w := range wantWords {
		if got.Words[i].W != w {
			t.Fatalf("word order %+v", got.Words)
		}
	}
	if got.Words[2].Timing != "interpolated" || got.Words[2].Start != 1.75 || got.Words[3].End != 2.75 || got.Words[0].Timing != "" {
		t.Fatalf("interpolated words %+v", got.Words)
	}
	// ave's rule splits on speaker change, so Ana's "again" is its own utterance.
	if len(got.Utterances) != 3 || got.Utterances[0].Text != "hello there" || got.Utterances[1].Speaker != "S2" ||
		got.Utterances[1].FirstWord != 2 || got.Utterances[1].LastWord != 3 || got.Utterances[2].Text != "again" {
		t.Fatalf("utterances %+v", got.Utterances)
	}
	if got.Speakers["S1"].TalkSeconds != 1.5 {
		t.Fatalf("talk seconds %+v", got.Speakers)
	}
}

func TestAveSyncUsesManifestOffsets(t *testing.T) {
	s := AveSync(manifest(), 59.25)
	files := s["files"].(map[string]any)
	if s["reference"] != "raw/program.wav" || files["raw/program.wav"].(map[string]any)["start_offset"] != 0.0 {
		t.Fatalf("reference %+v", s)
	}
	ben := files["raw/audio/Ben_mic.wav"].(map[string]any)
	if ben["start_offset"] != 1.25 || ben["drift_ppm"] != 0.0 {
		t.Fatalf("ben %+v", ben)
	}
}

func TestWriteCreatesFilesAndDropsStaleAveTranscript(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(dir, Input{Manifest: manifest(), Transcript: doc(), ProgramDurationSec: 59}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"handoff/clip-editor.json", "handoff/ave/sync.json", "handoff/ave/transcript.json", "handoff/ave/transcript.md", "handoff/ave/episode.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if _, err := Write(dir, Input{Manifest: manifest(), ProgramDurationSec: 59}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"handoff/ave/transcript.json", "handoff/ave/transcript.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Fatalf("stale %s left behind", f)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "handoff/ave/episode.json")); err != nil {
		t.Fatal("the camera mapping does not need a transcript")
	}
}

// hostLast is manifest() with roles: Ben is the host although his voice is
// listed second, plus a phone camera for Ana.
func hostLast() *export.Manifest {
	m := manifest()
	m.Participants = []export.ManifestParticipant{{ID: "p1", Name: "Ana", Role: "guest"}, {ID: "p2", Name: "Ben", Role: "host"}}
	m.Sources = append(m.Sources,
		export.ExportSource{ID: "v-ben", Participant: "Ben", ParticipantID: "p2", Kind: "video", FileRelPath: "video/Ben_cam.mov", OffsetSec: 1.3, DurationSec: 58},
		export.ExportSource{ID: "v-ana-phone", Participant: "Ana", ParticipantID: "p1", Kind: "video", Source: export.SourcePhone, FileRelPath: "video/Ana_phone.mov", OffsetSec: 20, DurationSec: 30})
	return m
}

func TestSpeakersPutTheHostFirstAndPhonesAfterCameras(t *testing.T) {
	sp := Speakers(hostLast())
	if len(sp) != 2 || sp[0].ID != "S1" || sp[0].Name != "Ben" || sp[0].Role != "host" || sp[1].Name != "Ana" || sp[1].Role != "guest" {
		t.Fatalf("speakers %+v", sp)
	}
	if len(sp[1].Video) != 2 || sp[1].Video[0] != "video/Ana_cam.mov" || sp[1].Video[1] != "video/Ana_phone.mov" {
		t.Fatalf("Ana's cameras %+v (screen share excluded, webcam before phone)", sp[1].Video)
	}
	if d := Build(Input{Manifest: hostLast()}); len(d.Participants) != 2 || d.Speakers[0].Name != "Ben" || d.Ave.Episode != AveEpisodeFile || d.Ave.Import == "" {
		t.Fatalf("clip-editor.json participants/speakers/ave %+v %+v %+v", d.Participants, d.Speakers, d.Ave)
	}
}

// handoff/ave/episode.json is exactly what ave reads at the camera-mapping
// checkpoint: cameras{file, role, session}, speakers{name, camera}, audio_source.
func TestAveEpisodeMapsEachSpeakerToTheirOwnCamera(t *testing.T) {
	data, _ := json.Marshal(AveEpisode(hostLast(), true))
	var got struct {
		Cameras map[string]struct {
			File, Role, Session, Participant string
		}
		Speakers map[string]struct {
			Name, Camera, ParticipantID string
		}
		AudioSource string                                       `json:"audio_source"`
		ExtraAngles []struct{ File, Participant, Source string } `json:"extra_angles"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if c := got.Cameras["C1"]; c.File != "raw/video/Ben_cam.mov" || c.Role != "host" || c.Session != AveSession || got.Speakers["S1"].Camera != "C1" || got.Speakers["S1"].Name != "Ben" {
		t.Fatalf("host mapping %+v %+v", got.Cameras, got.Speakers)
	}
	if c := got.Cameras["C2"]; c.File != "raw/video/Ana_cam.mov" || c.Role != "guest" || got.Speakers["S2"].Camera != "C2" || len(got.Cameras) != 2 {
		t.Fatalf("guest mapping %+v", got.Cameras)
	}
	if got.AudioSource != AveRawProgram || len(got.ExtraAngles) != 2 || got.ExtraAngles[0].Source != export.SourceScreen || got.ExtraAngles[1].Source != export.SourcePhone {
		t.Fatalf("audio source / extra angles %+v %+v", got.AudioSource, got.ExtraAngles)
	}
	if AveEpisode(hostLast(), false).AudioSource != "" {
		t.Fatal("audio_source names raw/program.wav without a program mix")
	}
	// A participant without a camera still gets a speaker entry, with no camera.
	if sp := AveEpisode(manifest(), true).Speakers["S2"]; sp.Name != "Ben" || sp.Camera != "" {
		t.Fatalf("camera-less speaker %+v", sp)
	}
}

// work/transcript.md matches `ave transcribe` (transcribe.fmt_ts, "[ts] S1: text").
func TestAveMarkdownAndEngineMatchAve(t *testing.T) {
	d := doc()
	d.Engine = transcript.Engine{Name: "minutes-cli", ASR: "apple-speech-on-device"}
	tr := AveTranscript(manifest(), d, 59.25)
	want := "[00:00:01.05] S1: hello there\n[00:00:01.75] S2: hi you\n[00:00:02.55] S1: again\n"
	if got := AveMarkdown(tr); got != want {
		t.Fatalf("markdown\n%s\nwant\n%s", got, want)
	}
	if tr.Engine["asr"] != "minutes-cli: apple-speech-on-device" || tr.Engine["diarization"] != "studiolink-isolated-tracks" {
		t.Fatalf("engine %+v", tr.Engine)
	}
	if AveTranscript(manifest(), doc(), 1).Engine["asr"] != "unknown" {
		t.Fatal("an unnamed engine must say unknown, not claim Apple Speech")
	}
	for in, want := range map[float64]string{0: "00:00:00.00", 61.5: "00:01:01.50", 3725.25: "01:02:05.25"} {
		if got := aveTimestamp(in); got != want {
			t.Fatalf("aveTimestamp(%v) = %s, want %s", in, got, want)
		}
	}
}
