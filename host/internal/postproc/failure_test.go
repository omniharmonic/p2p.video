package postproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func TestCopyDirPropagatesDestinationFailure(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "blocker")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "media.wav"), []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyDir(src, filepath.Join(dst, "export")); err == nil {
		t.Fatal("copy failure swallowed")
	}
}

func TestProcessTakeRejectsIncompleteInputs(t *testing.T) {
	for _, status := range []string{"uploading", "assembled"} {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			st, err := store.Open(filepath.Join(root, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			session := &store.Session{ID: "session", Title: "test", CreatedAt: time.Now()}
			if err = st.CreateSession(session); err != nil {
				t.Fatal(err)
			}
			take := &store.Take{ID: "take", SessionID: session.ID, Number: 1, Status: "ready", WallClockStart: time.Now()}
			if err = st.CreateTake(take); err != nil {
				t.Fatal(err)
			}
			participant := &store.Participant{ID: "participant", SessionID: session.ID, Name: "Tester", Role: "host", Status: "admitted", JoinedAt: time.Now()}
			if err = st.CreateParticipant(participant); err != nil {
				t.Fatal(err)
			}
			source := &store.Source{ID: "source", ParticipantID: participant.ID, Kind: "mic", Label: "mic"}
			if err = st.CreateSource(source); err != nil {
				t.Fatal(err)
			}
			track := &store.Track{ID: "track", TakeID: take.ID, SourceID: source.ID, Kind: "audio", Mime: "audio/wav", Status: status}
			if err = st.CreateTrack(track); err != nil {
				t.Fatal(err)
			}
			pipeline := NewPipeline(st, &config.Config{DataDir: root}, &ffmpeg.Runner{FFmpegPath: "/missing/ffmpeg", FFprobePath: "/missing/ffprobe"})
			if err = pipeline.ProcessTake(take.ID); err == nil {
				t.Fatal("incomplete input completed")
			} else if status == "uploading" && !strings.Contains(err.Error(), "awaiting assembly") {
				t.Fatalf("wrong failure: %v", err)
			} else if status == "assembled" && !strings.Contains(err.Error(), "missing assembled track") {
				t.Fatalf("wrong failure: %v", err)
			}
			updated, err := st.GetTake(take.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Status != "failed" {
				t.Fatalf("status=%s", updated.Status)
			}
		})
	}
}

func TestPublishRegenerationPreservesPreviousPackage(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "new")
	dst := filepath.Join(root, "published")
	for _, dir := range []string{src, dst} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "manifest.json"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "manifest.json"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "editor-notes.txt"), []byte("keep user edits"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishDir(src, dst); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "manifest.json"))
	if err != nil || string(data) != "new" {
		t.Fatal("new package not published")
	}
	backups, err := filepath.Glob(dst + ".previous-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("prior package must be retained")
	}
	data, err = os.ReadFile(filepath.Join(backups[0], "editor-notes.txt"))
	if err != nil || string(data) != "keep user edits" {
		t.Fatal("prior user edits were not preserved")
	}
}

func TestFailedRegenerationLeavesPreviousPackageUntouched(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "published")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "manifest.json"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishDir(filepath.Join(root, "missing-source"), dst); err == nil {
		t.Fatal("expected copy failure")
	}
	data, err := os.ReadFile(filepath.Join(dst, "manifest.json"))
	if err != nil || string(data) != "old" {
		t.Fatal("failed copy changed old package")
	}
}
