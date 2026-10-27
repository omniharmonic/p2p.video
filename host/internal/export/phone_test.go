package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A phone camera (PRD F3) is an extra angle of its participant [SL-P1-9]:
// "<Name> Phone" in the multicam, never that participant's voice track.
func TestPhoneIsAnExtraAngleOfItsParticipant(t *testing.T) {
	dir := t.TempDir()
	sources := []ExportSource{
		{ID: "cam", Participant: "Ada", ParticipantID: "p1", Label: "Camera", Kind: "video", FileRelPath: "video/ada.mov", DurationSec: 10},
		{ID: "cam-audio", Participant: "Ada", ParticipantID: "p1", Label: "Camera Audio", Kind: "audio", FileRelPath: "audio/ada.wav", DurationSec: 10},
		{ID: "phone", Participant: "Ada", ParticipantID: "p1", Label: "Phone Camera", Kind: "video", Source: SourcePhone, FileRelPath: "video/ada_phone.mov", OffsetSec: 2, DurationSec: 6},
		{ID: "phone-audio", Participant: "Ada", ParticipantID: "p1", Label: "Phone Camera Audio", Kind: "audio", Source: SourcePhone, FileRelPath: "audio/ada_phone.wav", OffsetSec: 2, DurationSec: 6},
	}
	if err := GenerateFCPXML(dir, "Phone", 1, 30, sources); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
	s := string(data)
	if !strings.Contains(s, `name="Ada Camera"`) || !strings.Contains(s, `name="Ada Phone"`) || strings.Count(s, "<mc-angle ") != 2 {
		t.Fatal("phone is not its own angle of Ada")
	}
	speakers := (&Manifest{Sources: sources}).SpeakerAudio()
	if len(speakers) != 1 || speakers[0].FileRelPath != "audio/ada.wav" {
		t.Fatalf("speaker audio %+v: the phone's mic is not Ada's voice track", speakers)
	}
}
