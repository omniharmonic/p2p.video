package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManifestSchema identifies manifest.json.
const ManifestSchema = "studiolink.take/1"

// ReadManifest loads <pkgDir>/manifest.json and rejects file paths that would
// leave the package.
func ReadManifest(pkgDir string) (*Manifest, []byte, error) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "manifest.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Schema != ManifestSchema {
		return nil, nil, fmt.Errorf("unsupported manifest schema %q", m.Schema)
	}
	for _, s := range m.Sources {
		if !filepath.IsLocal(filepath.FromSlash(s.FileRelPath)) {
			return nil, nil, fmt.Errorf("manifest source %s has a non-local path", s.ID)
		}
	}
	return &m, data, nil
}

// SpeakerAudio picks one isolated voice track per participant, in manifest
// order: a dedicated microphone WAV when present, otherwise the audio
// extracted from that participant's camera recording. Screen-share audio is
// never a speaker (it carries other people's voices and system sound).
func (m *Manifest) SpeakerAudio() []ExportSource {
	type pick struct {
		src   ExportSource
		isMic bool
	}
	order := []string{}
	chosen := map[string]pick{}
	for _, s := range m.Sources {
		// Shared screen audio and a phone's room mic aren't a participant's voice track.
		if s.Kind != "audio" || s.Source == SourceScreen || s.Source == SourcePhone {
			continue
		}
		key := s.ParticipantID
		if key == "" {
			key = "name:" + s.Participant
		}
		isMic := !strings.HasSuffix(s.ID, "-audio")
		cur, seen := chosen[key]
		if !seen {
			order = append(order, key)
			chosen[key] = pick{s, isMic}
		} else if isMic && !cur.isMic {
			chosen[key] = pick{s, isMic}
		}
	}
	out := make([]ExportSource, 0, len(order))
	for _, k := range order {
		out = append(out, chosen[k].src)
	}
	return out
}
