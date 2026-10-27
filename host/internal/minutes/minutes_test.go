package minutes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTranscribeFileArgsGolden(t *testing.T) {
	got := TranscribeFileArgs("/pkg/audio/Ana_mic_t1.wav", "Ana Lopez", "en-US", "/tmp/x/transcript.json")
	want := []string{"transcribe-file", "/pkg/audio/Ana_mic_t1.wav", "--speaker", "Ana Lopez", "--locale", "en-US", "--output", "/tmp/x/transcript.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	// A hostile or empty name never becomes a flag and never adds arguments.
	got = TranscribeFileArgs("/a.wav", "--json; rm -rf ~\n--output /etc/x", "", "/o.json")
	want = []string{"transcribe-file", "/a.wav", "--speaker", "json; rm -rf ~ --output /etc/x", "--locale", "en-US", "--output", "/o.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hostile argv = %q", got)
	}
	got = TranscribeFileArgs("/a.wav", " -- ", "fr-FR", "/o.json")
	want = []string{"transcribe-file", "/a.wav", "--locale", "fr-FR", "--output", "/o.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty-name argv = %q", got)
	}
	if n := len([]rune(SafeSpeaker(strings.Repeat("é", 300)))); n != 120 {
		t.Fatalf("speaker not bounded: %d", n)
	}
}

func TestDiscoverOrder(t *testing.T) {
	present := map[string]bool{}
	f := Finder{
		Home:     "/Users/u",
		IsExec:   func(p string) bool { return present[p] },
		LookPath: func(string) (string, error) { return "", errors.New("no") },
	}
	if _, err := Discover("", f); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing installed: %v", err)
	}
	present["/Users/u/.local/bin/minutes-cli"] = true
	if p, _ := Discover("", f); p != "/Users/u/.local/bin/minutes-cli" {
		t.Fatalf("home candidate: %s", p)
	}
	present["/Applications/Minutes.app/Contents/MacOS/minutes-cli"] = true
	if p, _ := Discover("", f); p != "/Applications/Minutes.app/Contents/MacOS/minutes-cli" {
		t.Fatalf("app bundle wins over home: %s", p)
	}
	f.LookPath = func(string) (string, error) { return "/opt/bin/minutes-cli", nil }
	if p, _ := Discover("", f); p != "/opt/bin/minutes-cli" {
		t.Fatalf("PATH wins over candidates: %s", p)
	}
	// A configured path is authoritative: missing means an error, never a fallback.
	if _, err := Discover("/custom/minutes-cli", f); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing configured path must fail: %v", err)
	}
	present["/custom/minutes-cli"] = true
	if p, _ := Discover("/custom/minutes-cli", f); p != "/custom/minutes-cli" {
		t.Fatalf("configured: %s", p)
	}
	if _, err := Discover("relative/minutes-cli", f); err == nil {
		t.Fatal("relative configured path must be rejected")
	}
}

type fakeRunner struct {
	calls  [][]string
	write  string // JSON written to --output
	stdout string
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, program string, args []string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{program}, args...))
	if _, ok := ctx.Deadline(); !ok {
		return nil, nil, errors.New("no deadline")
	}
	for i, a := range args {
		if a == "--output" && f.write != "" {
			if err := os.WriteFile(args[i+1], []byte(f.write), 0o600); err != nil {
				return nil, nil, err
			}
		}
	}
	return []byte(f.stdout), nil, f.err
}

const sample = `{"schema":"minutes.file-transcript/1","audio":"a.wav","duration":9.5,"speechDuration":4,"uncoveredSpeechSeconds":0,
 "speaker":"Ana","clusters":1,"engine":{"asr":"apple-speech-on-device","diarization":"fluidaudio","locale":"en-US"},
 "segments":[{"start":0.5,"end":2,"speaker":"Ana","cluster":"c1","text":"hello there","confidence":0.8,
   "words":[{"word":"hello","start":0.5,"end":1,"confidence":0.9},{"word":"there","start":1.1,"end":2,"confidence":0.7}]}]}`

func TestTranscribeFileUsesPrivateOutputAndCleansUp(t *testing.T) {
	tmp := t.TempDir()
	r := &fakeRunner{write: sample}
	c := &Client{Path: "/bin/minutes-cli", Runner: r, Locale: "en-US", TempDir: tmp, Base: time.Minute, PerAudioSecond: time.Second}
	ft, err := c.TranscribeFile(context.Background(), "/pkg/audio/a.wav", "Ana", 9.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ft.Segments) != 1 || ft.Segments[0].Text != "hello there" || *ft.Segments[0].Confidence != 0.8 || len(ft.Segments[0].Words) != 2 {
		t.Fatalf("decoded: %+v", ft)
	}
	call := r.calls[0]
	if call[0] != "/bin/minutes-cli" || call[1] != "transcribe-file" || call[2] != "/pkg/audio/a.wav" {
		t.Fatalf("argv: %q", call)
	}
	out := call[len(call)-1]
	if filepath.Dir(filepath.Dir(out)) != tmp || !strings.HasPrefix(filepath.Base(filepath.Dir(out)), "studiolink-minutes-") {
		t.Fatalf("output must be in a private run folder under TempDir: %s", out)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("run folder not removed: %v", entries)
	}
	if _, err := c.TranscribeFile(context.Background(), "relative.wav", "Ana", 1); err == nil {
		t.Fatal("relative audio path must be refused")
	}
}

func TestTranscribeFileReportsCLIErrorLine(t *testing.T) {
	r := &fakeRunner{stdout: "Error: Speech recognition permission denied by user or system policy\n", err: errors.New("exit status 1")}
	c := &Client{Path: "/bin/minutes-cli", Runner: r, TempDir: t.TempDir(), Base: time.Minute}
	_, err := c.TranscribeFile(context.Background(), "/a.wav", "Ana", 1)
	if err == nil || !strings.Contains(err.Error(), "Speech recognition permission denied") || strings.Contains(err.Error(), "Error:") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseRejectsUnknownSchemaAndBadTimes(t *testing.T) {
	if _, err := ParseFileTranscript([]byte(`{"schema":"minutes.file-transcript/2","segments":[]}`)); err == nil {
		t.Fatal("unknown schema accepted")
	}
	if _, err := ParseFileTranscript([]byte(`{"schema":"minutes.file-transcript/1","segments":[{"start":3,"end":1,"text":"x"}]}`)); err == nil {
		t.Fatal("end before start accepted")
	}
	if _, err := ParseFileTranscript([]byte(`not json`)); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestSupportedChecksHelpText(t *testing.T) {
	r := &fakeRunner{stdout: "Usage:\n  minutes-cli import <audio-path>\n"}
	c := &Client{Path: "/bin/minutes-cli", Runner: r}
	if err := c.Supported(withDeadline(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("old CLI: %v", err)
	}
	r.stdout += "  minutes-cli transcribe-file <audio-path> ...\n"
	if err := c.Supported(withDeadline(t)); err != nil {
		t.Fatalf("new CLI: %v", err)
	}
	if !reflect.DeepEqual(r.calls[0][1:], HelpArgs) {
		t.Fatalf("help argv: %q", r.calls[0])
	}
}

func withDeadline(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// The shared contract fixture (also produced by Minutes' FileTranscriptDocument).
func TestContractFixtureParses(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/integrations/minutes.file-transcript.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	ft, err := ParseFileTranscript(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(ft.Segments) != 2 || ft.Segments[1].Confidence != nil || len(ft.Segments[0].Words) != 4 || *ft.Speaker != "Ana" || ft.Engine.ASR != "apple-speech-on-device" {
		t.Fatalf("fixture decoded as %+v", ft)
	}
}

// End to end through os/exec with a stand-in CLI: the real argv reaches the
// program and the JSON it writes to --output is read back.
func TestExecRunnerWithStandInCLI(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.txt")
	script := filepath.Join(dir, "minutes-cli")
	fixture, err := filepath.Abs("../../../testdata/integrations/minutes.file-transcript.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsLog + "'\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = --output ]; then cp '" + fixture + "' \"$2\"; fi; shift; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewClient(script)
	c.TempDir = dir
	ft, err := c.TranscribeFile(context.Background(), "/pkg/audio/Ana mic.wav", "Ana; $(whoami)", 9.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ft.Segments) != 2 {
		t.Fatalf("segments %d", len(ft.Segments))
	}
	logged, _ := os.ReadFile(argsLog)
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if lines[0] != "transcribe-file" || lines[1] != "/pkg/audio/Ana mic.wav" || lines[2] != "--speaker" || lines[3] != "Ana; $(whoami)" {
		t.Fatalf("argv not passed verbatim: %q", lines)
	}
}

// Opt-in run against a real minutes-cli (needs Speech Recognition permission
// for the calling terminal and FluidAudio models):
//
//	STUDIOLINK_MINUTES_CLI=/path/minutes-cli STUDIOLINK_MINUTES_AUDIO=/abs/voice.wav go test ./internal/minutes -run Real -v
func TestRealMinutesCLI(t *testing.T) {
	cli, audio := os.Getenv("STUDIOLINK_MINUTES_CLI"), os.Getenv("STUDIOLINK_MINUTES_AUDIO")
	if cli == "" || audio == "" {
		t.Skip("set STUDIOLINK_MINUTES_CLI and STUDIOLINK_MINUTES_AUDIO to run")
	}
	c := NewClient(cli)
	if err := c.Supported(context.Background()); err != nil {
		t.Fatalf("supported: %v", err)
	}
	ft, err := c.TranscribeFile(context.Background(), audio, "Real Speaker", 60)
	if err != nil {
		if strings.Contains(err.Error(), "permission") {
			t.Skipf("minutes-cli ran but Speech Recognition is not authorized for this process: %v", err)
		}
		t.Fatal(err)
	}
	for _, s := range ft.Segments {
		t.Logf("%.2f-%.2f %s: %s", s.Start, s.End, s.Speaker, s.Text)
	}
	if len(ft.Segments) == 0 {
		t.Fatal("no segments recognized")
	}
}

func TestMinimalEnvDropsSecrets(t *testing.T) {
	t.Setenv("STUDIOLINK_SECRET_KEY", "super-secret-value-0123456789abcdef")
	for _, kv := range minimalEnv() {
		if strings.Contains(kv, "super-secret") {
			t.Fatal("secret leaked into child environment")
		}
	}
}
