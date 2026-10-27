package postproc

import (
	"encoding/binary"
	"encoding/json"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsolatedPCMReplacesOnlyOwnedVideoAudio(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, err := range []error{
		st.CreateSession(&store.Session{ID: "s", Title: "PCM", CreatedAt: time.Now()}),
		st.CreateParticipant(&store.Participant{ID: "p1", SessionID: "s", Name: "Same name", Status: "admitted", JoinedAt: time.Now()}),
		st.CreateParticipant(&store.Participant{ID: "p2", SessionID: "s", Name: "Same name", Status: "admitted", JoinedAt: time.Now()}),
		st.CreateSource(&store.Source{ID: "v", ParticipantID: "p1", Kind: "camera"}),
		st.CreateSource(&store.Source{ID: "a", ParticipantID: "p1", Kind: "mic"}),
		st.CreateSource(&store.Source{ID: "foreign", ParticipantID: "p2", Kind: "mic"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	pipeline := NewPipeline(st, &config.Config{DataDir: root}, nil)
	tracks := []store.Track{{ID: "video", TakeID: "take", SourceID: "v", Kind: "video", Mime: "video/webm", Status: "assembled"}, {ID: "audio", TakeID: "take", SourceID: "a", Kind: "audio", Mime: "audio/wav", Status: "assembled"}}
	writeSidecar := func(complete bool) {
		body, _ := json.Marshal(map[string]any{"replacesAudioOfTrackId": "video", "captureComplete": complete, "pcm": map[string]any{"format": "float32le", "sampleRate": 48000, "channels": 1, "frames": 2}})
		if err := os.WriteFile(filepath.Join(root, "audio.sidecar.json"), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Canonical Float32 WAVE, with two synthetic zero samples.
	wav := make([]byte, 66)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], 58)
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 18)
	binary.LittleEndian.PutUint16(wav[20:], 3)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 48000)
	binary.LittleEndian.PutUint32(wav[28:], 192000)
	binary.LittleEndian.PutUint16(wav[32:], 4)
	binary.LittleEndian.PutUint16(wav[34:], 32)
	copy(wav[38:], "fact")
	binary.LittleEndian.PutUint32(wav[42:], 4)
	binary.LittleEndian.PutUint32(wav[46:], 2)
	copy(wav[50:], "data")
	binary.LittleEndian.PutUint32(wav[54:], 8)
	if err := os.WriteFile(filepath.Join(root, "audio.wav"), wav, 0600); err != nil {
		t.Fatal(err)
	}
	writeSidecar(true)
	replacements, err := pipeline.isolatedPCMReplacements(tracks, root)
	if err != nil || replacements["video"] != "audio" {
		t.Fatalf("own PCM pairing failed: %v %v", replacements, err)
	}
	tracks[1].SourceID = "foreign"
	if _, err := pipeline.isolatedPCMReplacements(tracks, root); err == nil || !strings.Contains(err.Error(), "participant") {
		t.Fatalf("cross-participant audio accepted: %v", err)
	}
	tracks[1].SourceID = "a"
	writeSidecar(false)
	if _, err := pipeline.isolatedPCMReplacements(tracks, root); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial capture allowed: %v", err)
	}
	writeSidecar(true)
	second := tracks[1]
	second.ID = "second-audio"
	for _, suffix := range []string{".wav", ".sidecar.json"} {
		body, err := os.ReadFile(filepath.Join(root, "audio"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, second.ID+suffix), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pipeline.isolatedPCMReplacements(append(tracks, second), root); err == nil || !strings.Contains(err.Error(), "multiple PCM") {
		t.Fatalf("duplicate audio replacement accepted: %v", err)
	}
	wav[54] = 4
	if err := os.WriteFile(filepath.Join(root, "audio.wav"), wav, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.isolatedPCMReplacements(tracks, root); err == nil {
		t.Fatal("WAV geometry mismatch accepted")
	}
}
