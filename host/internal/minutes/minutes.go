// Package minutes invokes the Minutes command line tool (a separate local
// app: on-device Apple Speech + FluidAudio) to transcribe isolated tracks.
// Commands are argv-only through Runner; nothing is ever built as a shell
// string. The CLI contract is documented in docs/integrations/MINUTES.md.
package minutes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FileTranscriptSchema is the JSON contract emitted by
// `minutes-cli transcribe-file`.
const FileTranscriptSchema = "minutes.file-transcript/1"

// ErrNotFound means no minutes-cli executable was found.
var ErrNotFound = errors.New("minutes-cli not found")

// ErrUnsupported means the installed minutes-cli predates transcribe-file.
var ErrUnsupported = errors.New("minutes-cli does not support transcribe-file (see docs/integrations/MINUTES.md)")

// Runner executes one program with arguments and returns its output. It is
// the only process boundary and is replaced in tests.
type Runner interface {
	Run(ctx context.Context, program string, args []string) (stdout, stderr []byte, err error)
}

// ExecRunner runs programs with os/exec (argv only, no shell).
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, program string, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = minimalEnv()
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// minimalEnv passes only what a local CLI needs; p2p.video secrets in the
// daemon environment (STUDIOLINK_SECRET_KEY) never reach child processes.
func minimalEnv() []string {
	var env []string
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// Candidates lists where minutes-cli is looked for, in order, after an
// explicitly configured path and $PATH.
func Candidates(home string) []string {
	list := []string{
		"/Applications/Minutes.app/Contents/MacOS/minutes-cli",
		"/Applications/Minutes.app/Contents/Helpers/minutes-cli",
		"/opt/homebrew/bin/minutes-cli",
		"/usr/local/bin/minutes-cli",
	}
	if home != "" {
		list = append(list,
			filepath.Join(home, "Applications/Minutes.app/Contents/MacOS/minutes-cli"),
			filepath.Join(home, "Applications/Minutes.app/Contents/Helpers/minutes-cli"),
			filepath.Join(home, ".local/bin/minutes-cli"),
			filepath.Join(home, "bin/minutes-cli"),
		)
	}
	return list
}

// Finder abstracts the filesystem for Discover.
type Finder struct {
	LookPath func(string) (string, error)
	IsExec   func(string) bool
	Home     string
}

// DefaultFinder uses the real filesystem.
func DefaultFinder() Finder {
	home, _ := os.UserHomeDir()
	return Finder{LookPath: exec.LookPath, IsExec: isExecutable, Home: home}
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

// Discover returns the minutes-cli to use: the configured path (which must
// exist; no silent fallback when the user named one), then $PATH, then
// Candidates.
func Discover(configured string, f Finder) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("minutes-cli path must be absolute: %q", configured)
		}
		if !f.IsExec(configured) {
			return "", fmt.Errorf("%w at configured path %s", ErrNotFound, configured)
		}
		return configured, nil
	}
	if f.LookPath != nil {
		if p, err := f.LookPath("minutes-cli"); err == nil && filepath.IsAbs(p) {
			return p, nil
		}
	}
	for _, c := range Candidates(f.Home) {
		if f.IsExec(c) {
			return c, nil
		}
	}
	return "", ErrNotFound
}

// TranscribeFileArgs is the exact argv (after the program) for one isolated
// track. The speaker name is only a label for standalone readers; p2p.video
// names segments itself from the manifest.
func TranscribeFileArgs(audioPath, speaker, locale, outPath string) []string {
	args := []string{"transcribe-file", audioPath}
	if name := SafeSpeaker(speaker); name != "" {
		args = append(args, "--speaker", name)
	}
	if locale == "" {
		locale = "en-US"
	}
	return append(args, "--locale", locale, "--output", outPath)
}

// SafeSpeaker makes a participant name safe as a flag value: one line, no
// leading dashes (which the CLI would read as a flag), at most 120 runes.
func SafeSpeaker(name string) string {
	name = strings.TrimLeft(strings.Join(strings.Fields(name), " "), "-")
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	return name
}

// FileTranscript is the decoded minutes.file-transcript/1 document.
type FileTranscript struct {
	Schema                 string    `json:"schema"`
	Audio                  string    `json:"audio"`
	Duration               float64   `json:"duration"`
	SpeechDuration         float64   `json:"speechDuration"`
	UncoveredSpeechSeconds float64   `json:"uncoveredSpeechSeconds"`
	Speaker                *string   `json:"speaker"`
	Clusters               int       `json:"clusters"`
	Engine                 Engine    `json:"engine"`
	Segments               []Segment `json:"segments"`
}

// Engine describes the recognizer.
type Engine struct {
	ASR         string `json:"asr"`
	Diarization string `json:"diarization"`
	Locale      string `json:"locale"`
}

// Segment is one utterance relative to the start of the file.
type Segment struct {
	Start      float64  `json:"start"`
	End        float64  `json:"end"`
	Speaker    string   `json:"speaker"`
	Cluster    string   `json:"cluster"`
	Text       string   `json:"text"`
	Confidence *float64 `json:"confidence"`
	Words      []Word   `json:"words"`
}

// Word is one recognized word relative to the start of the file.
type Word struct {
	Word       string  `json:"word"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"`
}

// Client transcribes files with a discovered minutes-cli.
type Client struct {
	Path   string
	Runner Runner
	Locale string
	// TempDir is where private per-run output folders are created
	// (default os.TempDir()).
	TempDir string
	// Timeout bounds one file: Base + PerAudioSecond × duration.
	Base           time.Duration
	PerAudioSecond time.Duration
}

// NewClient returns a client with production defaults.
func NewClient(path string) *Client {
	return &Client{Path: path, Runner: ExecRunner{}, Locale: "en-US", Base: 5 * time.Minute, PerAudioSecond: 2 * time.Second}
}

// HelpArgs asks the CLI for its usage text.
var HelpArgs = []string{"help"}

// Supported reports whether the CLI offers transcribe-file.
func (c *Client) Supported(ctx context.Context) error {
	out, _, err := c.Runner.Run(ctx, c.Path, HelpArgs)
	if err != nil && len(out) == 0 {
		return fmt.Errorf("run minutes-cli help: %w", err)
	}
	if !bytes.Contains(out, []byte("transcribe-file")) {
		return ErrUnsupported
	}
	return nil
}

// TranscribeFile transcribes one isolated track. durationSec scales the
// timeout. The CLI writes into a private 0700 directory removed afterwards.
func (c *Client) TranscribeFile(ctx context.Context, audioPath, speaker string, durationSec float64) (*FileTranscript, error) {
	if !filepath.IsAbs(audioPath) {
		return nil, fmt.Errorf("audio path must be absolute")
	}
	dir, err := os.MkdirTemp(c.TempDir, "studiolink-minutes-")
	if err != nil {
		return nil, fmt.Errorf("create private output directory: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect output directory: %w", err)
	}
	outPath := filepath.Join(dir, "transcript.json")
	timeout := c.Base + time.Duration(durationSec*float64(c.PerAudioSecond))
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err := c.Runner.Run(runCtx, c.Path, TranscribeFileArgs(audioPath, speaker, c.Locale, outPath))
	if err != nil {
		return nil, fmt.Errorf("minutes-cli transcribe-file: %w: %s", err, failureDetail(stdout, stderr))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("minutes-cli produced no transcript: %w", err)
	}
	return ParseFileTranscript(data)
}

// failureDetail keeps the CLI's own error line (its "Error: …" output) and
// bounds it. minutes-cli prints errors on stdout.
func failureDetail(stdout, stderr []byte) string {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		text = strings.TrimSpace(string(stdout))
	}
	if i := strings.LastIndex(text, "Error:"); i >= 0 {
		text = strings.TrimSpace(text[i+len("Error:"):])
	}
	if r := []rune(text); len(r) > 300 {
		text = string(r[:300]) + "…"
	}
	if text == "" {
		return "no output"
	}
	return text
}

// ParseFileTranscript validates and decodes the CLI's JSON.
func ParseFileTranscript(data []byte) (*FileTranscript, error) {
	var ft FileTranscript
	if err := json.Unmarshal(data, &ft); err != nil {
		return nil, fmt.Errorf("decode minutes transcript: %w", err)
	}
	if ft.Schema != FileTranscriptSchema {
		return nil, fmt.Errorf("unsupported minutes transcript schema %q", ft.Schema)
	}
	for i, s := range ft.Segments {
		if s.End < s.Start || s.Start < 0 {
			return nil, fmt.Errorf("minutes transcript segment %d has invalid times", i)
		}
	}
	return &ft, nil
}
