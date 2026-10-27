package postproc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"io"
	"os"
	"path/filepath"
)

// readMicProcessing returns the sanitized recording-mic processing a track's
// sidecar declares [SL-P1-10], or nil.
func readMicProcessing(sidecarPath string) *export.MicProcessing {
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		return nil
	}
	var side struct {
		MicProcessing *export.MicProcessing `json:"micProcessing"`
	}
	if json.Unmarshal(raw, &side) != nil {
		return nil
	}
	return export.SanitizeMicProcessing(side.MicProcessing)
}

type pcmSidecar struct {
	CaptureComplete        *bool  `json:"captureComplete"`
	ReplacesAudioOfTrackID string `json:"replacesAudioOfTrackId"`
	PCM                    *struct {
		Format     string `json:"format"`
		SampleRate uint32 `json:"sampleRate"`
		Channels   uint16 `json:"channels"`
		Frames     uint64 `json:"frames"`
	} `json:"pcm"`
}

// A dedicated uncompressed source can replace the embedded microphone only for
// its explicitly paired video track, in the same take and owned by the same ID.
func (p *Pipeline) isolatedPCMReplacements(tracks []store.Track, rawDir string) (map[string]string, error) {
	byID := make(map[string]store.Track, len(tracks))
	for _, track := range tracks {
		byID[track.ID] = track
	}
	replacements := map[string]string{}
	for _, track := range tracks {
		if track.Kind != "audio" || track.Mime != "audio/wav" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(rawDir, track.ID+".sidecar.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read PCM sidecar: %w", err)
		}
		var sidecar pcmSidecar
		if err := json.Unmarshal(raw, &sidecar); err != nil {
			return nil, fmt.Errorf("decode audio sidecar: %w", err)
		}
		if sidecar.PCM == nil {
			continue
		}
		if sidecar.CaptureComplete == nil || !*sidecar.CaptureComplete {
			return nil, fmt.Errorf("partial PCM capture %s requires review; export blocked", track.ID)
		}
		pcm := sidecar.PCM
		if pcm.Format != "float32le" || pcm.SampleRate < 8000 || pcm.SampleRate > 192000 || pcm.Channels < 1 || pcm.Channels > 2 || pcm.Frames == 0 || pcm.Frames > (uint64(0xffffffff)-50)/(uint64(pcm.Channels)*4) {
			return nil, fmt.Errorf("invalid PCM geometry for %s", track.ID)
		}
		file, err := os.Open(filepath.Join(rawDir, track.ID+".wav"))
		if err != nil {
			return nil, fmt.Errorf("open PCM original: %w", err)
		}
		header := make([]byte, 58)
		_, readErr := io.ReadFull(file, header)
		stat, statErr := file.Stat()
		_ = file.Close()
		if readErr != nil || statErr != nil {
			return nil, fmt.Errorf("cannot inspect PCM WAV %s", track.ID)
		}
		dataBytes := pcm.Frames * uint64(pcm.Channels) * 4
		u16 := func(offset int) uint16 { return binary.LittleEndian.Uint16(header[offset:]) }
		u32 := func(offset int) uint32 { return binary.LittleEndian.Uint32(header[offset:]) }
		if string(header[:4]) != "RIFF" || string(header[8:16]) != "WAVEfmt " || u32(16) != 18 || u16(20) != 3 || u16(22) != pcm.Channels || u32(24) != pcm.SampleRate || u32(28) != pcm.SampleRate*uint32(pcm.Channels)*4 || u16(32) != pcm.Channels*4 || u16(34) != 32 || u16(36) != 0 || string(header[38:42]) != "fact" || u32(42) != 4 || uint64(u32(46)) != pcm.Frames || string(header[50:54]) != "data" || uint64(u32(54)) != dataBytes || uint64(u32(4))+8 != dataBytes+58 || stat.Size() != int64(dataBytes+58) {
			return nil, fmt.Errorf("PCM WAV header or frame count disagrees with source sidecar: %s", track.ID)
		}
		if sidecar.ReplacesAudioOfTrackID == "" {
			continue
		}
		video, ok := byID[sidecar.ReplacesAudioOfTrackID]
		if !ok || video.Kind != "video" || video.TakeID != track.TakeID {
			return nil, fmt.Errorf("PCM source %s references an invalid video track", track.ID)
		}
		audioSource, audioErr := p.Store.GetSource(track.SourceID)
		videoSource, videoErr := p.Store.GetSource(video.SourceID)
		if audioErr != nil || videoErr != nil || audioSource.ParticipantID != videoSource.ParticipantID {
			return nil, fmt.Errorf("PCM source %s cannot replace another participant's audio", track.ID)
		}
		if _, exists := replacements[video.ID]; exists {
			return nil, fmt.Errorf("multiple PCM sources claim video %s; review required", video.ID)
		}
		replacements[video.ID] = track.ID
	}
	return replacements, nil
}
