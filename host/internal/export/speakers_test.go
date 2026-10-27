package export

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSpeakerAudioPrefersMicrophonesAndSkipsScreens(t *testing.T) {
	m := Manifest{Sources: []ExportSource{
		{ID: "t-ben-cam", Participant: "Ben", ParticipantID: "p2", Kind: "video"},
		{ID: "t-ben-cam-audio", Participant: "Ben", ParticipantID: "p2", Kind: "audio", FileRelPath: "audio/ben_cam.wav"},
		{ID: "t-ana-mic", Participant: "Ana", ParticipantID: "p1", Kind: "audio", FileRelPath: "audio/ana.wav"},
		{ID: "t-ben-mic", Participant: "Ben", ParticipantID: "p2", Kind: "audio", FileRelPath: "audio/ben.wav"},
		{ID: "t-ana-screen-audio", Participant: "Ana", ParticipantID: "p1", Kind: "audio", Source: SourceScreen, FileRelPath: "audio/screen.wav"},
		{ID: "t-cy-cam-audio", Participant: "Cy", Kind: "audio", FileRelPath: "audio/cy_cam.wav"},
	}}
	got := m.SpeakerAudio()
	want := []string{"audio/ben.wav", "audio/ana.wav", "audio/cy_cam.wav"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].FileRelPath != w {
			t.Errorf("speaker %d = %s, want %s", i, got[i].FileRelPath, w)
		}
	}
}

func TestReadManifestRoundTripAndRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	src := []ExportSource{{ID: "a", Participant: "Ana", ParticipantID: "p1", Kind: "audio", FileRelPath: "audio/a.wav", OffsetSec: 0.5, DurationSec: 3}}
	if err := WriteManifest(dir, "s", "T", 1, time.Unix(0, 0), 30, src); err != nil {
		t.Fatal(err)
	}
	m, raw, err := ReadManifest(dir)
	if err != nil || len(raw) == 0 || m.Sources[0].ParticipantID != "p1" || m.Sources[0].OffsetSec != 0.5 {
		t.Fatalf("%+v %v", m, err)
	}
	src[0].FileRelPath = "../../etc/passwd"
	if err := WriteManifest(dir, "s", "T", 1, time.Unix(0, 0), 30, src); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadManifest(dir); err == nil {
		t.Fatal("escaping path accepted")
	}
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"schema":"other/1"}`), 0o644)
	if _, _, err := ReadManifest(dir); err == nil {
		t.Fatal("foreign schema accepted")
	}
}
