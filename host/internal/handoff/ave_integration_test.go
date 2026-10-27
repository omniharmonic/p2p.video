package handoff

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

// aveCheck loads a p2p.video package with ave's own code. With an ave that
// has `ave.studiolink` (branch feat/studiolink-import) it runs the importer;
// with a stock ave it follows the manual recipe in CLIP_EDITOR.md. Either way
// it then probes the footage (ave ingest + audio), plans the cut list with
// ave's planner, builds the assembly EditDoc and validates it.
const aveCheck = `
import json, shutil, sys
from pathlib import Path
from ave import assemble, cutplan, editdoc
from ave.episode import Episode, read_json, write_json

pkg, epdir = Path(sys.argv[1]), Path(sys.argv[2])
ep = Episode(epdir)
try:
    from ave import studiolink
except ImportError:
    studiolink = None
if studiolink:
    mode = "import-studiolink"
    r = studiolink.import_package(ep, pkg)
    problems = r["problems"]
else:
    mode = "manual-recipe"
    from ave import audio, media
    ep.create()
    idx = read_json(pkg / "handoff/clip-editor.json")
    for dest, src in idx["ave"]["raw"].items():
        (ep.root / dest).parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(pkg / src, ep.root / dest)
    shutil.copy2(pkg / idx["ave"]["transcript"], ep.artifact("transcript.json"))
    shutil.copy2(pkg / idx["ave"]["transcriptMarkdown"], ep.artifact("transcript.md"))
    shutil.copy2(pkg / idx["ave"]["sync"], ep.artifact("sync.json"))
    meta = read_json(ep.meta_path)
    meta.update({k: v for k, v in read_json(pkg / idx["ave"]["episode"]).items() if k in ("cameras", "speakers", "audio_source")})
    write_json(ep.meta_path, meta)
    media.ingest(ep)
    audio.extract_all(ep)
    problems = []
probed = {m["file"]: m for m in read_json(ep.artifact("media.json"))["files"]}
sync, tr = read_json(ep.artifact("sync.json")), read_json(ep.artifact("transcript.json"))
problems += [f"sync names unprobed {f}" for f in sync["files"] if f not in probed]
if tr["source"] not in probed:
    problems.append("transcript source not probed")
meta = read_json(ep.meta_path)
problems += [f"camera {k} has no fps" for k, c in meta["cameras"].items() if not (probed.get(c["file"]) or {}).get("video", {}).get("fps")]
# A stock ave plans a take whose cameras all start after take zero only as a named session.
session = None if hasattr(cutplan, "late_start_s") else "s1"
cut = cutplan.build_cutlist(ep, session=session)
doc = assemble.build(ep, (640, 360), session=session)
print(json.dumps({"mode": mode, "session": session, "problems": problems + editdoc.validate(doc),
                  "shots": [[s["camera"], s["speaker"]] for s in cut["shots"]], "regions": cut["regions"],
                  "speakers": {k: v["name"] for k, v in tr["speakers"].items()}, "words": len(tr["words"]),
                  "dialogue_src": doc["sources"][doc["tracks"]["dialogue"][0]["src"]]["file"]}))
`

// avePython finds ave's interpreter: $STUDIOLINK_AVE_DIR (an ave checkout)
// or ~/dev/agentic-video-editor, with its own .venv (scripts/setup.sh).
func avePython(t *testing.T) string {
	dir := os.Getenv("STUDIOLINK_AVE_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "dev", "agentic-video-editor")
	}
	py := filepath.Join(dir, ".venv", "bin", "python")
	if _, err := os.Stat(py); err != nil {
		t.Skipf("ave not found (%s); set STUDIOLINK_AVE_DIR to an ave checkout with its .venv", py)
	}
	return py
}

// TestHandoffLoadsInAve writes a real package (three picture-only cameras
// that start 20-40 ms after take zero, three isolated mics, the program mix
// from the production builder, a transcript) and has ave load and plan it.
func TestHandoffLoadsInAve(t *testing.T) {
	if testing.Short() {
		t.Skip("runs ave")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	py := avePython(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "take_01_t1")
	for _, d := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(pkg, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ff, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	const take = 9.0
	people := []struct {
		name, id, role string
		camOff, micOff float64
		from, to       float64
		text           string
	}{
		{"Ana", "p1", "guest", 0.02, 0.01, 3.2, 5.8, "thanks for having me today"},
		{"Ben", "p2", "host", 0.04, 0.03, 0.4, 2.8, "welcome to the show everyone"},
		{"Cy", "p3", "guest", 0.03, 0.02, 6.2, 8.6, "I record everything locally"},
	}
	m := &export.Manifest{Schema: export.ManifestSchema, Session: export.SessionMeta{ID: "s1", Title: "Pilot"}, Take: 1, FrameRate: "30/1", SampleRate: 48000}
	var tracks []transcript.Track
	var mix []ffmpeg.MixInput
	for i, p := range people {
		cam, mic := fmt.Sprintf("video/%s_cam_v%d.mov", p.name, i), fmt.Sprintf("audio/%s_mic_a%d.wav", p.name, i)
		run("-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=320x180:rate=30:duration=%.3f", take-p.camOff), "-an", "-c:v", "libx264", "-pix_fmt", "yuv420p", filepath.Join(pkg, cam))
		// Speech in the mic's own clock (take time minus its offset), like a real isolated track.
		run("-f", "lavfi", "-i", fmt.Sprintf("aevalsrc=0.4*sin(2*PI*%d*t)*between(t\\,%.3f\\,%.3f):s=48000:d=%.3f", 220+110*i, p.from-p.micOff, p.to-p.micOff, take-p.micOff),
			"-ac", "2", "-c:a", "pcm_f32le", filepath.Join(pkg, mic))
		m.Participants = append(m.Participants, export.ManifestParticipant{ID: p.id, Name: p.name, Role: p.role})
		m.Sources = append(m.Sources,
			export.ExportSource{ID: "v" + p.id, Participant: p.name, ParticipantID: p.id, Kind: "video", FileRelPath: cam, OffsetSec: p.camOff, DurationSec: take - p.camOff, Method: "sidecar-coarse"},
			export.ExportSource{ID: "a" + p.id, Participant: p.name, ParticipantID: p.id, Kind: "audio", FileRelPath: mic, OffsetSec: p.micOff, DurationSec: take - p.micOff, Method: "sidecar-coarse"})
		words := strings.Fields(p.text)
		seg := transcript.Segment{Start: p.from - p.micOff, End: p.to - p.micOff, Text: p.text}
		step := (p.to - p.from) / float64(len(words))
		for k, w := range words {
			seg.Words = append(seg.Words, transcript.Word{Word: w, Start: seg.Start + step*float64(k), End: seg.Start + step*float64(k+1), Confidence: 0.9})
		}
		tracks = append(tracks, transcript.Track{Speaker: p.name, ParticipantID: p.id, SourceID: "a" + p.id, File: mic, OffsetSec: p.micOff, Segments: []transcript.Segment{seg}})
		mix = append(mix, ffmpeg.MixInput{Path: filepath.Join(pkg, mic), OffsetSec: p.micOff})
	}
	if err := os.MkdirAll(filepath.Join(pkg, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	args, err := ffmpeg.BuildProgramMixArgs(mix, filepath.Join(pkg, filepath.FromSlash(ProgramFile)))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("program mix: %v\n%s", err, out)
	}
	doc := &transcript.Document{Schema: transcript.Schema, Engine: transcript.Engine{Name: "minutes-cli", ASR: "test"}, Segments: transcript.Merge(tracks)}
	if _, err := Write(pkg, Input{Manifest: m, Transcript: doc, ProgramDurationSec: take}); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(dir, "ave_check.py")
	if err := os.WriteFile(script, []byte(aveCheck), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-I", script, pkg, filepath.Join(dir, "episode"))
	if src := os.Getenv("STUDIOLINK_AVE_SRC"); src != "" { // test an ave worktree with that checkout's venv
		cmd = exec.Command(py, script, pkg, filepath.Join(dir, "episode"))
		cmd.Env = append(os.Environ(), "PYTHONPATH="+src)
	}
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("ave could not load the package: %v\n%s\n%s", err, out, stderr)
	}
	var got struct {
		Mode, Session string
		Problems      []string
		Shots         [][2]any
		Regions       [][2]float64
		Speakers      map[string]string
		Words         int
		DialogueSrc   string `json:"dialogue_src"`
	}
	if err := json.Unmarshal(out[strings.LastIndexByte(strings.TrimSpace(string(out)), '\n')+1:], &got); err != nil {
		t.Fatalf("ave output %s: %v", out, err)
	}
	t.Logf("ave %s (session %q): shots %v regions %v", got.Mode, got.Session, got.Shots, got.Regions)
	if len(got.Problems) != 0 {
		t.Fatalf("ave problems: %v", got.Problems)
	}
	// The host is S1 on C1; the cut follows the conversation: Ben, Ana, Cy.
	// A stock ave plans a named session, whose opening shot falls back to a
	// camera that rolled before the host's (20 ms earlier) and absorbs Ana's.
	want := [][2]any{{"C1", "S1"}, {"C2", "S2"}, {"C3", "S3"}}
	if got.Mode == "manual-recipe" {
		want = want[2:]
		got.Shots = got.Shots[len(got.Shots)-1:]
	}
	if fmt.Sprint(got.Shots) != fmt.Sprint(want) {
		t.Fatalf("shots %v, want %v", got.Shots, want)
	}
	if len(got.Regions) != 1 || got.Regions[0][0] <= 0 {
		t.Fatalf("regions %v: the cut list starts once the cameras roll", got.Regions)
	}
	if got.Speakers["S1"] != "Ben" || got.Speakers["S2"] != "Ana" || got.Speakers["S3"] != "Cy" || got.Words != 14 || got.DialogueSrc != AveRawProgram {
		t.Fatalf("speakers %v words %d dialogue %s", got.Speakers, got.Words, got.DialogueSrc)
	}
}
