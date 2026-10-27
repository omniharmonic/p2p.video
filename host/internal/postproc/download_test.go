package postproc

import (
	"archive/zip"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadPublishedOutputsOnlyAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateSession(&store.Session{ID: "s1", Title: "Synthetic", CreatedAt: time.Now(), PresetJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	take := &store.Take{ID: "t1", SessionID: "s1", Number: 1, Status: "recording", WallClockStart: time.Now()}
	if err := st.CreateTake(take); err != nil {
		t.Fatal(err)
	}
	p := NewPipeline(st, &config.Config{DataDir: root}, nil)
	if _, err := p.PrepareDownload("t1"); err == nil {
		t.Fatal("incomplete take downloadable")
	}
	take.Status = "complete"
	if err := st.UpdateTake(take); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "exports", "Synthetic_s1", "take_01_t1")
	os.MkdirAll(filepath.Join(dir, "audio"), 0700)
	for _, name := range []string{"manifest.json", "take_01_multicam.fcpxml", "audio/mic.wav", ".secret_key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	name, err := p.PrepareDownload("t1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(name)
	info, _ := os.Stat(name)
	if info.Mode().Perm() != 0600 {
		t.Fatal("temporary download must be private")
	}
	z, err := zip.OpenReader(name)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if len(z.File) != 3 {
		t.Fatalf("unexpected files %d", len(z.File))
	}
	for _, f := range z.File {
		if !filepath.IsLocal(f.Name) || f.Name == ".secret_key" {
			t.Fatal("unsafe ZIP member")
		}
	}
	os.Symlink(filepath.Join(root, "test.db"), filepath.Join(dir, "audio", "linked.wav"))
	if _, err := p.PrepareDownload("t1"); err == nil {
		t.Fatal("symlink accepted")
	}
}

// Transcript and clip-editor handoff files added after export travel in the
// download; staging folders and unrelated folders never do [SL-P4-1].
func TestDownloadIncludesTranscriptAndHandoff(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.CreateSession(&store.Session{ID: "s1", Title: "Synthetic", CreatedAt: time.Now(), PresetJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTake(&store.Take{ID: "t1", SessionID: "s1", Number: 1, Status: "complete", WallClockStart: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p := NewPipeline(st, &config.Config{DataDir: root}, nil)
	dir := filepath.Join(root, "exports", "Synthetic_s1", "take_01_t1")
	for _, sub := range []string{"transcript", "handoff/ave", ".transcript-123", "raw"} {
		os.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
	for _, name := range []string{"manifest.json", "transcript/transcript.json", "transcript/transcript.srt", "handoff/clip-editor.json",
		"handoff/ave/transcript.json", ".transcript-123/transcript.json", "raw/upload.webm", "transcript/.hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.WithPublishedPackage("t1", func(d string) error {
		if d != dir {
			t.Fatalf("package dir %s", d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	name, err := p.PrepareDownload("t1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(name)
	z, err := zip.OpenReader(name)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	got := map[string]bool{}
	for _, f := range z.File {
		got[f.Name] = true
	}
	for _, want := range []string{"manifest.json", "transcript/transcript.json", "transcript/transcript.srt", "handoff/clip-editor.json", "handoff/ave/transcript.json"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if len(got) != 5 {
		t.Errorf("unexpected members: %v", got)
	}
}
