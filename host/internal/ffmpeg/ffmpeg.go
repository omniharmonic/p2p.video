package ffmpeg

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// Runner manages FFmpeg/FFprobe operations.
type Runner struct {
	FFmpegPath      string
	FFprobePath     string
	HasVideoToolbox bool
}

// NewRunner discovers ffmpeg and probes capabilities.
func NewRunner() (*Runner, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		// Attempt to guess ffprobe alongside ffmpeg
		ffprobePath = strings.Replace(ffmpegPath, "ffmpeg", "ffprobe", 1)
	}

	r := &Runner{
		FFmpegPath:  ffmpegPath,
		FFprobePath: ffprobePath,
	}
	r.HasVideoToolbox = r.probeEncoder("h264_videotoolbox")

	return r, nil
}

func (r *Runner) probeEncoder(name string) bool {
	cmd := exec.Command(r.FFmpegPath, "-encoders")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), name)
}

// BuildCFRVideoArgs returns the argv slice for conforming video to Constant Frame Rate (CFR).
func (r *Runner) BuildCFRVideoArgs(inPath, outPath string, fps float64, timecode string) []string {
	return r.BuildCFRVideoArgsWithDrift(inPath, outPath, fps, timecode, 1)
}

// BuildCFRVideoArgsWithDrift maps client media time onto the host clock.
func (r *Runner) BuildCFRVideoArgsWithDrift(inPath, outPath string, fps float64, timecode string, driftBeta float64) []string {
	// Conformed media starts at zero; the pipeline retains the original video
	// stream's leading offset separately in the editing timeline.
	return r.BuildConformArgs(ConformSpec{In: inPath, Out: outPath, FPS: fps, Timecode: timecode, DriftBeta: driftBeta, Codec: CodecH264})
}

// BuildScreenVideoArgs conforms a screen-share recording like a camera
// (BuildCFRVideoArgsWithDrift) but onto a fixed, even-sized canvas. Shared
// windows can have odd dimensions (rejected by 4:2:0 H.264) and can be
// resized mid-recording; scaling with preserved aspect ratio and padding to
// width×height keeps the encoder input constant. width/height are the
// recording's initial geometry; they are rounded down to even numbers.
// -reinit_filter 0 keeps one filter graph across a mid-stream resize: a
// rebuilt graph would restart setpts' STARTPTS at the resize and the CFR
// filter would drop every later frame as a duplicate.
func (r *Runner) BuildScreenVideoArgs(inPath, outPath string, fps float64, timecode string, driftBeta float64, width, height int) []string {
	return r.BuildConformArgs(ConformSpec{In: inPath, Out: outPath, FPS: fps, Timecode: timecode, DriftBeta: driftBeta, Codec: CodecH264, Screen: true, Width: width, Height: height})
}

func evenAtLeast2(v int) int {
	v -= v % 2
	if v < 2 {
		return 2
	}
	return v
}

// ExtractAudioSamples decodes audio from a media file into 32-bit floating point PCM samples.
func (r *Runner) ExtractAudioSamples(mediaPath string, sampleRate int, maxSeconds float64) ([]float64, error) {
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	args := []string{
		"-v", "error",
		"-i", mediaPath,
		"-vn",
		"-ac", "1",
		"-ar", strconv.Itoa(sampleRate),
	}
	if maxSeconds > 0 {
		args = append(args, "-t", fmt.Sprintf("%f", maxSeconds))
	}
	args = append(args, "-f", "f32le", "pipe:1")

	cmd := exec.Command(r.FFmpegPath, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg extract audio samples from %s: %w", mediaPath, err)
	}

	if len(out) < 4 {
		return nil, fmt.Errorf("empty audio samples decoded from %s", mediaPath)
	}

	numSamples := len(out) / 4
	samples := make([]float64, numSamples)
	for i := 0; i < numSamples; i++ {
		bits := binary.LittleEndian.Uint32(out[i*4 : (i+1)*4])
		samples[i] = float64(math.Float32frombits(bits))
	}
	return samples, nil
}

// BuildAudioWindowArgs returns argv that decodes durSec seconds of a file's
// audio starting at startSec (input timestamps, accurate seek) to mono
// float32 PCM on stdout, for acoustic analysis.
func BuildAudioWindowArgs(mediaPath string, startSec, durSec float64, sampleRate int) []string {
	return []string{
		"-v", "error", "-nostdin",
		"-ss", strconv.FormatFloat(startSec, 'f', 6, 64),
		"-i", mediaPath,
		"-t", strconv.FormatFloat(durSec, 'f', 6, 64),
		"-vn", "-ac", "1", "-ar", strconv.Itoa(sampleRate),
		"-f", "f32le", "pipe:1",
	}
}

// ExtractAudioWindow runs BuildAudioWindowArgs and returns the samples.
func (r *Runner) ExtractAudioWindow(mediaPath string, startSec, durSec float64, sampleRate int) ([]float64, error) {
	out, err := exec.Command(r.FFmpegPath, BuildAudioWindowArgs(mediaPath, startSec, durSec, sampleRate)...).Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg decode audio window: %w", err)
	}
	samples := make([]float64, len(out)/4)
	for i := range samples {
		samples[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(out[i*4:])))
	}
	return samples, nil
}

// BuildAudioDecodeArgs returns argv that decodes a whole file's audio to
// mono float32 PCM at sampleRate on stdout (the refrec reference stream,
// read once in order) [SL-P0-10].
func BuildAudioDecodeArgs(mediaPath string, sampleRate int) []string {
	return []string{
		"-v", "error", "-nostdin",
		"-i", mediaPath,
		"-vn", "-ac", "1", "-ar", strconv.Itoa(sampleRate),
		"-f", "f32le", "pipe:1",
	}
}

// StreamAudio runs BuildAudioDecodeArgs and passes successive blocks of
// samples to fn, without holding the whole decode in memory.
func (r *Runner) StreamAudio(mediaPath string, sampleRate int, fn func([]float32)) error {
	cmd := exec.Command(r.FFmpegPath, BuildAudioDecodeArgs(mediaPath, sampleRate)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg decode pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg decode: %w", err)
	}
	buf := make([]byte, 64<<10)
	block := make([]float32, 0, len(buf)/4)
	var carry []byte
	for {
		n, rerr := out.Read(buf)
		data := append(carry, buf[:n]...)
		whole := len(data) / 4 * 4
		block = block[:0]
		for i := 0; i < whole; i += 4 {
			block = append(block, math.Float32frombits(binary.LittleEndian.Uint32(data[i:])))
		}
		carry = append(carry[:0:0], data[whole:]...)
		if len(block) > 0 {
			fn(block)
		}
		if rerr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg decode audio: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// driftRates returns integer sample rates p, q with p/q as close as possible
// to beta, q in [192000, 384000]. Each q contributes |round(beta*q)/q-beta|
// <= 0.5/q; scanning the range finds ratios accurate to well below 1e-9,
// i.e. under 4 ms of accumulated error over an hour.
func driftRates(beta float64) (p, q int) {
	bestErr := math.Inf(1)
	for cand := 192000; cand <= 384000; cand++ {
		num := int(math.Round(beta * float64(cand)))
		if e := math.Abs(float64(num)/float64(cand) - beta); e < bestErr {
			bestErr, p, q = e, num, cand
		}
	}
	return p, q
}

// TimestampGapFill keeps decoded audio on its container timeline [SL-QA-2]:
// a gap of more than 15 ms between consecutive audio timestamps becomes
// silence instead of being closed up. Chromium's MediaRecorder writes such a
// gap (one Opus packet whose duration exceeds its samples, e.g. 90 ms for
// 60 ms) when the capture clock jumps under load; concatenating the samples
// put the rest of the camera's audio ~30 ms ahead of its video. The 15 ms
// threshold matches the PCM recorder's gap fill (web/src/lib/pcm-recorder.ts);
// async=1 fills and trims only, it never stretches samples.
const TimestampGapFill = "aresample=async=1:min_hard_comp=0.015"

// BuildAudioNormalizeArgs returns argv for converting audio to 48kHz 24-bit PCM WAV.
func (r *Runner) BuildAudioNormalizeArgs(inPath, outPath string, driftBeta float64) []string {
	filter := TimestampGapFill + ",aresample=48000"
	if driftBeta > 0 && driftBeta != 1 {
		// hostUs = A+B*clientUs: the device's sample clock ran at 1/B of
		// nominal, so its samples really span B times longer. Relabel the
		// rate and resample back to 48 kHz: this undoes the clock error
		// (time and pitch together) and is transparent. atempo was used
		// before; its time-domain stretching rewrote the waveform (about
		// 10-25 dB SNR against the original) even for a few ppm of drift.
		// asetrate only accepts integer rates, so a ppm-scale ratio is
		// expressed as P/Q with large integers: upsample to P, relabel as Q
		// (duration scales by P/Q = B), then resample to 48 kHz.
		p, q := driftRates(driftBeta)
		filter = fmt.Sprintf("%s,aresample=%d:filter_size=64:phase_shift=10:cutoff=0.97,asetrate=%d,aresample=48000:filter_size=64:phase_shift=10:cutoff=0.97", TimestampGapFill, p, q)
	}

	return []string{
		"-y",
		"-i", inPath,
		"-vn",
		"-af", filter,
		"-c:a", "pcm_s24le",
		outPath,
	}
}

// ProbeDurationSec extracts media duration in seconds via ffprobe.
func (r *Runner) ProbeDurationSec(mediaPath string) (float64, error) {
	cmd := exec.Command(r.FFprobePath,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		mediaPath,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0.0, fmt.Errorf("ffprobe duration failed: %w", err)
	}

	str := strings.TrimSpace(out.String())
	val, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0.0, fmt.Errorf("invalid duration format %q: %w", str, err)
	}
	return val, nil
}

// Execute runs ffmpeg with the specified arguments.
func (r *Runner) Execute(args []string) error {
	cmd := exec.Command(r.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg failed: %w, stderr: %s", err, stderr.String())
	}
	return nil
}

// HasAudioStream checks if the media file contains at least one audio stream.
func (r *Runner) HasAudioStream(mediaPath string) (bool, error) {
	cmd := exec.Command(r.FFprobePath,
		"-v", "error",
		"-select_streams", "a",
		"-show_entries", "stream=codec_type",
		"-of", "csv=p=0",
		mediaPath,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("ffprobe streams failed: %w", err)
	}
	return strings.Contains(out.String(), "audio"), nil
}

// VideoInfo describes source geometry and the video start relative to its container.
type VideoInfo struct {
	Width, Height    int
	LeadingOffsetSec float64
}

func (r *Runner) ProbeVideoInfo(mediaPath string) (VideoInfo, error) {
	var probe struct {
		Streams []struct {
			Width  int    `json:"width"`
			Height int    `json:"height"`
			Start  string `json:"start_time"`
		} `json:"streams"`
		Format struct {
			Start string `json:"start_time"`
		} `json:"format"`
	}
	out, err := exec.Command(r.FFprobePath, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height,start_time:format=start_time", "-of", "json", mediaPath).Output()
	if err != nil {
		return VideoInfo{}, fmt.Errorf("probe video geometry: %w", err)
	}
	if err = json.Unmarshal(out, &probe); err != nil {
		return VideoInfo{}, fmt.Errorf("decode video geometry: %w", err)
	}
	if len(probe.Streams) != 1 || probe.Streams[0].Width <= 0 || probe.Streams[0].Height <= 0 {
		return VideoInfo{}, fmt.Errorf("missing video geometry")
	}
	parse := func(s string) (float64, error) {
		if s == "" || s == "N/A" {
			return 0, nil
		}
		v, e := strconv.ParseFloat(s, 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("invalid source timestamp")
		}
		return v, nil
	}
	videoStart, err := parse(probe.Streams[0].Start)
	if err != nil {
		return VideoInfo{}, err
	}
	containerStart, err := parse(probe.Format.Start)
	if err != nil {
		return VideoInfo{}, err
	}
	return VideoInfo{Width: probe.Streams[0].Width, Height: probe.Streams[0].Height, LeadingOffsetSec: math.Max(0, videoStart-containerStart)}, nil
}
