package postproc

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// OriginalsSchema identifies the quick-delivery package manifest.
const OriginalsSchema = "studiolink.originals/1"

// OriginalFile is one assembled upload, as recorded on its device.
type OriginalFile struct {
	TrackID       string `json:"trackId"`
	Participant   string `json:"participant"`
	ParticipantID string `json:"participantId,omitempty"`
	Label         string `json:"label"`
	Kind          string `json:"kind"`
	Source        string `json:"source,omitempty"`
	File          string `json:"file"`
	Mime          string `json:"mime"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256,omitempty"`
	// OffsetSec places the file's container time zero on the take timeline
	// (host clock, take start = 0). Drift is not corrected in originals.
	OffsetSec  float64 `json:"offsetSec"`
	DriftPpm   float64 `json:"driftPpm"`
	Method     string  `json:"alignmentMethod"`
	ResidualMs float64 `json:"residualMs"`
	// ReplacesAudioOf names the camera track whose embedded microphone this
	// uncompressed PCM recording supersedes.
	ReplacesAudioOf string `json:"replacesAudioOfTrackId,omitempty"`
	// MicProcessing: raw or clean recording microphone [SL-P1-10].
	MicProcessing *export.MicProcessing `json:"micProcessing,omitempty"`
}

// OriginalsManifest describes the quick-delivery package.
type OriginalsManifest struct {
	Schema  string `json:"schema"`
	Session struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"session"`
	Take           int            `json:"take"`
	StartWallClock time.Time      `json:"startWallClock"`
	Files          []OriginalFile `json:"files"`
}

// OriginalsPackage is a planned originals download.
type OriginalsPackage struct {
	Manifest OriginalsManifest
	paths    []string
}

// PlanOriginals lists a take's assembled per-participant originals with
// their clock-based timeline offsets. It is available as soon as every
// registered track is assembled, independently of (and concurrently with)
// the conformed export: raw uploads never change after assembly, so it does
// not take the export lock.
func (p *Pipeline) PlanOriginals(takeID string) (*OriginalsPackage, error) {
	tk, err := p.Store.GetTake(takeID)
	if err != nil {
		return nil, fmt.Errorf("take unavailable")
	}
	if tk.Status == "recording" {
		return nil, fmt.Errorf("take is still recording")
	}
	missing, err := p.Store.ListMissingTakeParticipants(takeID)
	if err != nil {
		return nil, fmt.Errorf("check expected recorders: %w", err)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%d expected participants have not finished uploading", len(missing))
	}
	sess, err := p.Store.GetSession(tk.SessionID)
	if err != nil {
		return nil, fmt.Errorf("session unavailable")
	}
	settings := store.DefaultSessionSettings()
	if sess.PresetJSON != "" && sess.PresetJSON != "{}" {
		if err := json.Unmarshal([]byte(sess.PresetJSON), &settings); err != nil {
			return nil, fmt.Errorf("decode session settings: %w", err)
		}
	}
	tracks, err := p.Store.ListTracksByTake(takeID)
	if err != nil {
		return nil, fmt.Errorf("list tracks: %w", err)
	}
	if len(tracks) == 0 {
		return nil, fmt.Errorf("take has no tracks")
	}
	rawDir := filepath.Join(p.Config.DataDir, "sessions", tk.SessionID, "takes", takeID, "raw")
	pkg := &OriginalsPackage{}
	m := &pkg.Manifest
	m.Schema, m.Take, m.StartWallClock = OriginalsSchema, tk.Number, tk.WallClockStart
	m.Session.ID, m.Session.Title = sess.ID, sess.Title
	for _, tr := range tracks {
		if tr.Status != "assembled" && tr.Status != "processed" {
			return nil, fmt.Errorf("track %s is %s, awaiting assembly", tr.ID, tr.Status)
		}
		ext := rawExt(tr.Mime)
		rawPath := filepath.Join(rawDir, tr.ID+ext)
		info, err := os.Lstat(rawPath)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return nil, fmt.Errorf("assembled track %s is missing or empty", tr.ID)
		}
		f := OriginalFile{TrackID: tr.ID, Participant: "Unknown", Label: tr.SourceID, Kind: tr.Kind, Mime: tr.Mime, Bytes: info.Size(), SHA256: tr.SHA256}
		if src, err := p.Store.GetSource(tr.SourceID); err == nil && src != nil {
			f.Label = src.Label
			f.Source = exportSourceKind(src.Kind == "screen", false)
			if part, _ := p.Store.GetParticipant(src.ParticipantID); part != nil {
				var phone bool
				f.Participant, f.ParticipantID, phone = p.sourceOwner(part)
				if phone {
					// Same name as the editor package ("Phone Camera"), so the
					// phone isn't a second "<Name> Camera" file [SL-QA-1].
					f.Source, f.Label = exportSourceKind(false, true), "Phone "+f.Label
				}
			}
		}
		sidecarPath := filepath.Join(rawDir, tr.ID+".sidecar.json")
		result, err := align.AlignTrackWithReferenceConfig(sidecarPath, tk.StartAtHostUs, nil, nil, settings.AudioSampleRate, settings.XcorrThreshold, settings.XcorrWindowMs)
		if err != nil {
			return nil, fmt.Errorf("align track %s: %w", tr.ID, err)
		}
		f.OffsetSec, f.DriftPpm, f.Method, f.ResidualMs = result.OffsetSec, result.DriftPpm, result.Method, result.ResidualMs
		if f.Source != "screen" {
			f.MicProcessing = readMicProcessing(sidecarPath)
		}
		if data, err := os.ReadFile(sidecarPath); err == nil {
			var side pcmSidecar
			if json.Unmarshal(data, &side) == nil && side.PCM != nil {
				f.ReplacesAudioOf = side.ReplacesAudioOfTrackID
			}
		}
		f.File = filepath.ToSlash(filepath.Join("originals", fmt.Sprintf("%s_%s_%s%s", sanitize(f.Participant), sanitize(f.Label), sanitize(tr.ID), ext)))
		m.Files = append(m.Files, f)
		pkg.paths = append(pkg.paths, rawPath)
	}
	// The same paired PCM correction as the editor package [SL-QA-1].
	if replacements, err := p.isolatedPCMReplacements(tracks, rawDir); err == nil && p.FFmpeg != nil {
		paired := pairedCameras(tracks, replacements)
		for i, tr := range tracks {
			camera, ok := paired[tr.ID]
			if !ok {
				continue
			}
			info, err := p.FFmpeg.ProbeMedia(pkg.paths[i])
			if err != nil {
				continue
			}
			f := &m.Files[i]
			clock := &align.AlignmentResult{Method: f.Method, OffsetSec: f.OffsetSec, DriftPpm: f.DriftPpm, ResidualMs: f.ResidualMs}
			r := p.refinePairedPCM(tk, rawDir, &settings, tr, camera, clock, info.DurationSec)
			f.OffsetSec, f.Method, f.ResidualMs = r.OffsetSec, r.Method, r.ResidualMs
		}
	}
	return pkg, nil
}

const originalsReadme = `p2p.video originals (quick delivery)

These are the per-participant recordings exactly as each device captured and
uploaded them, available as soon as the take finished uploading. The conformed
editor package (constant frame rate video, 48 kHz WAV, editor timelines) is
prepared separately; prefer it for editing when it is ready.

- originals/      one file per recorded source (camera with embedded microphone,
                  uncompressed PCM microphone, screen share)
- manifest.json   studiolink.originals/1: participant, source, MIME type, size,
                  SHA-256 and timeline placement of every file
- offsets.json    file -> offsetSec, for quick scripting

To line the files up, place each one so that its start sits offsetSec seconds
after the take start (0). Offsets are clock-based (sidecar), not acoustic sync
between people; an uncompressed microphone is measured against its own
camera's audio (method paired-xcorr) when possible.
Browser recordings are variable frame rate and drift is not corrected here;
some editors need them conformed to a constant frame rate before multicam.
A PCM microphone listed with replacesAudioOfTrackId supersedes that camera's
embedded microphone.
`

// OpenFiles opens every original, so a missing file fails before a response
// starts. The caller closes them (CloseFiles).
func (pkg *OriginalsPackage) OpenFiles() ([]*os.File, error) {
	files := make([]*os.File, 0, len(pkg.paths))
	for _, path := range pkg.paths {
		f, err := os.Open(path)
		if err != nil {
			CloseFiles(files)
			return nil, fmt.Errorf("open original: %w", err)
		}
		files = append(files, f)
	}
	return files, nil
}

// CloseFiles closes files opened by OpenFiles.
func CloseFiles(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// WriteZip streams the package as a ZIP: media stored, documents deflated.
func (pkg *OriginalsPackage) WriteZip(w io.Writer, files []*os.File) error {
	if len(files) != len(pkg.Manifest.Files) {
		return errors.New("originals were not opened")
	}
	archive := zip.NewWriter(w)
	manifest, err := json.MarshalIndent(pkg.Manifest, "", "  ")
	if err != nil {
		return err
	}
	offsets := make(map[string]float64, len(pkg.Manifest.Files))
	for _, f := range pkg.Manifest.Files {
		offsets[f.File] = f.OffsetSec
	}
	offsetsJSON, err := json.MarshalIndent(offsets, "", "  ")
	if err != nil {
		return err
	}
	modified := pkg.Manifest.StartWallClock
	if modified.IsZero() {
		modified = time.Now()
	}
	for _, doc := range []struct {
		name string
		data []byte
	}{{"README.txt", []byte(originalsReadme)}, {"manifest.json", manifest}, {"offsets.json", offsetsJSON}} {
		dest, err := archive.CreateHeader(&zip.FileHeader{Name: doc.name, Method: zip.Deflate, Modified: modified})
		if err != nil {
			return err
		}
		if _, err := dest.Write(doc.data); err != nil {
			return err
		}
	}
	for i, f := range files {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() != pkg.Manifest.Files[i].Bytes {
			return errors.New("original changed while packaging")
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = pkg.Manifest.Files[i].File
		// Media is already compressed (or PCM): store, never deflate.
		header.Method = zip.Store
		dest, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := io.Copy(dest, f); err != nil {
			return err
		}
	}
	return archive.Close()
}
