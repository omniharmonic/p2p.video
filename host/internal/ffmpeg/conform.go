package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Editor-package video codecs (session setting export_codec).
const (
	// CodecH264 is compact long-GOP H.264 at 15 Mbit/s. VideoToolbox's H.264
	// encoder is one hardware session at a time, so N cameras take N times
	// as long (docs/DECISIONS.md, SL-P0-9).
	CodecH264 = "h264"
	// CodecProResProxy is Apple ProRes 422 Proxy: editor-native intra frames,
	// encoded on the separate ProRes engines in parallel; measured 1.7× (busy
	// test pattern) to 9× (static webcam) the size of the H.264 deliverable.
	CodecProResProxy = "prores_proxy"
)

// NormalizeCodec maps a session setting to a supported export codec.
func NormalizeCodec(codec string) string {
	if codec == CodecProResProxy {
		return CodecProResProxy
	}
	return CodecH264
}

// ConformSpec describes one CFR conform of a camera or screen recording.
type ConformSpec struct {
	In, Out   string
	FPS       float64
	Timecode  string
	DriftBeta float64
	Codec     string
	// Screen shares go onto a fixed even Width×Height canvas.
	Screen        bool
	Width, Height int
}

// FPSFilterRate renders fps as the exact rational the fps filter uses.
func FPSFilterRate(fps float64) string {
	switch {
	case math.Abs(fps-29.97) < 0.015:
		return "30000/1001"
	case math.Abs(fps-23.976) < 0.015 || math.Abs(fps-23.98) < 0.015:
		return "24000/1001"
	case math.Abs(fps-59.94) < 0.015:
		return "60000/1001"
	}
	return strconv.Itoa(int(math.Round(fps)))
}

// encoderArgs selects the encoder for a codec. Hardware first; the software
// fallbacks are libx264 (system ffmpeg only, as before) and FFmpeg's native
// LGPL prores_ks.
func (r *Runner) encoderArgs(codec string) []string {
	if NormalizeCodec(codec) == CodecProResProxy {
		if r.HasVideoToolbox {
			return []string{"-c:v", "prores_videotoolbox", "-profile:v", "proxy"}
		}
		return []string{"-c:v", "prores_ks", "-profile:v", "proxy", "-vendor", "apl0", "-pix_fmt", "yuv422p10le"}
	}
	encoder := "libx264"
	if r.HasVideoToolbox {
		encoder = "h264_videotoolbox"
	}
	return []string{"-c:v", encoder, "-b:v", "15M", "-pix_fmt", "yuv420p"}
}

// BuildConformArgs returns the argv that conforms a recording to CFR on the
// host clock: PTS restart at zero scaled by the drift factor, then the fps
// filter. Screen shares add -reinit_filter 0 and a fixed padded canvas (see
// BuildScreenVideoArgs).
func (r *Runner) BuildConformArgs(s ConformSpec) []string {
	beta := s.DriftBeta
	if beta <= 0 {
		beta = 1
	}
	filter := fmt.Sprintf("setpts=(PTS-STARTPTS)*%.12f,fps=%s", beta, FPSFilterRate(s.FPS))
	args := []string{"-y"}
	if s.Screen {
		w, h := evenAtLeast2(s.Width), evenAtLeast2(s.Height)
		filter += fmt.Sprintf(",scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1", w, h, w, h)
		args = append(args, "-reinit_filter", "0")
	}
	args = append(args, "-i", s.In, "-vf", filter, "-fps_mode", "cfr")
	args = append(args, r.encoderArgs(s.Codec)...)
	if s.Timecode != "" {
		args = append(args, "-timecode", s.Timecode)
	}
	return append(args, "-an", s.Out)
}

// BuildRemuxVideoArgs stream-copies an already-CFR H.264 camera into the
// editor package without decoding (see CFRCopyEligible).
func (r *Runner) BuildRemuxVideoArgs(inPath, outPath, timecode string) []string {
	args := []string{"-y", "-i", inPath, "-map", "0:v:0", "-c:v", "copy"}
	if timecode != "" {
		args = append(args, "-timecode", timecode)
	}
	return append(args, "-an", outPath)
}

// WithProgress makes ffmpeg report machine-readable progress on stdout.
func WithProgress(args []string) []string {
	return append([]string{"-progress", "pipe:1", "-nostats"}, args...)
}

// ExecuteContext runs ffmpeg; cancelling ctx kills it. With onProgress, ffmpeg
// reports its output position, delivered as a fraction of durationSec.
func (r *Runner) ExecuteContext(ctx context.Context, args []string, durationSec float64, onProgress func(float64)) error {
	if onProgress != nil {
		args = WithProgress(args)
	}
	cmd := exec.CommandContext(ctx, r.FFmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var stdout io.ReadCloser
	if onProgress != nil {
		var err error
		if stdout, err = cmd.StdoutPipe(); err != nil {
			return fmt.Errorf("ffmpeg progress pipe: %w", err)
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg failed to start: %w", err)
	}
	if stdout != nil {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if frac, ok := progressFraction(scanner.Text(), durationSec); ok {
				onProgress(frac)
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg cancelled: %w", ctx.Err())
		}
		return fmt.Errorf("ffmpeg failed: %w, stderr: %s", err, stderr.String())
	}
	return nil
}

// progressFraction parses one -progress line ("out_time_us=…"; FFmpeg's
// out_time_ms is also microseconds).
func progressFraction(line string, durationSec float64) (float64, bool) {
	key, value, ok := strings.Cut(line, "=")
	if !ok || durationSec <= 0 || (key != "out_time_us" && key != "out_time_ms") {
		return 0, false
	}
	us, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || us < 0 {
		return 0, false
	}
	return math.Min(1, float64(us)/1e6/durationSec), true
}

// MediaInfo is everything the export needs from one ffprobe of an input.
type MediaInfo struct {
	DurationSec float64
	HasAudio    bool
	HasVideo    bool
	// AudioChannels is the first audio stream's channel count (0: unknown).
	AudioChannels int
	Video         VideoInfo
	VideoCodec    string
	PixFmt        string
	// FrameRate is the stream's r_frame_rate (e.g. "30/1").
	FrameRate string
	TimeBase  string
}

// ProbeMediaArgs returns the single ffprobe argv for ProbeMedia.
func ProbeMediaArgs(mediaPath string) []string {
	return []string{"-v", "error",
		"-show_entries", "format=duration,start_time:stream=codec_type,codec_name,width,height,start_time,r_frame_rate,time_base,pix_fmt,channels",
		"-of", "json", mediaPath}
}

// ErrNoContainerDuration: the container declares no duration. Firefox's
// MediaRecorder writes live WebM without a Segment Duration, and ffprobe
// does not estimate one for it [SL-P1-12].
var ErrNoContainerDuration = errors.New("container declares no duration")

// PacketEndArgs returns the ffprobe argv that lists every packet's
// presentation time and duration (no decoding): the fallback duration for a
// container without one is the latest packet end.
func PacketEndArgs(mediaPath string) []string {
	return []string{"-v", "error", "-show_entries", "packet=pts_time,duration_time", "-of", "csv=p=0", mediaPath}
}

// parsePacketEnd returns the latest pts+duration among "pts,duration" lines;
// N/A fields count as 0 duration or are skipped (no pts).
func parsePacketEnd(out []byte) (float64, error) {
	end, seen := 0.0, false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), ",")
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		pts, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) {
			continue
		}
		dur := 0.0
		if len(fields) > 1 {
			if d, err := strconv.ParseFloat(fields[1], 64); err == nil && d > 0 && !math.IsInf(d, 0) {
				dur = d
			}
		}
		if !seen || pts+dur > end {
			end, seen = pts+dur, true
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read packet list: %w", err)
	}
	if !seen || end <= 0 {
		return 0, fmt.Errorf("no timed packets")
	}
	return end, nil
}

// ProbeMedia replaces separate duration, audio-stream and geometry probes
// with one ffprobe call. The first video stream supplies geometry and its
// start relative to the container. A container without a duration (Firefox
// WebM) is measured from its packets instead.
func (r *Runner) ProbeMedia(mediaPath string) (MediaInfo, error) {
	out, err := exec.Command(r.FFprobePath, ProbeMediaArgs(mediaPath)...).Output()
	if err != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe %s: %w", mediaPath, err)
	}
	info, err := parseProbeMedia(out)
	if !errors.Is(err, ErrNoContainerDuration) {
		return info, err
	}
	packets, perr := exec.Command(r.FFprobePath, PacketEndArgs(mediaPath)...).Output()
	if perr != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe packets %s: %w", mediaPath, perr)
	}
	end, perr := parsePacketEnd(packets)
	if perr != nil {
		return MediaInfo{}, fmt.Errorf("%w; packet scan of %s: %v", err, mediaPath, perr)
	}
	info.DurationSec = end
	return info, nil
}

// parseProbeMedia decodes ProbeMediaArgs output. A missing ("" or "N/A")
// container duration returns the rest of the info with ErrNoContainerDuration.
func parseProbeMedia(out []byte) (MediaInfo, error) {
	var probe struct {
		Streams []struct {
			Type      string `json:"codec_type"`
			Codec     string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Start     string `json:"start_time"`
			FrameRate string `json:"r_frame_rate"`
			TimeBase  string `json:"time_base"`
			PixFmt    string `json:"pix_fmt"`
			Channels  int    `json:"channels"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			Start    string `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return MediaInfo{}, fmt.Errorf("decode ffprobe output: %w", err)
	}
	var info MediaInfo
	var missing error
	if d := strings.TrimSpace(probe.Format.Duration); d == "" || d == "N/A" {
		missing = ErrNoContainerDuration
	} else {
		duration, err := strconv.ParseFloat(d, 64)
		if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) {
			return MediaInfo{}, fmt.Errorf("invalid duration format %q", probe.Format.Duration)
		}
		info.DurationSec = duration
	}
	containerStart, err := parseTimestamp(probe.Format.Start)
	if err != nil {
		return MediaInfo{}, err
	}
	for _, s := range probe.Streams {
		switch s.Type {
		case "audio":
			if !info.HasAudio {
				info.AudioChannels = s.Channels
			}
			info.HasAudio = true
		case "video":
			if info.HasVideo {
				continue
			}
			info.HasVideo = true
			start, err := parseTimestamp(s.Start)
			if err != nil {
				return MediaInfo{}, err
			}
			info.Video = VideoInfo{Width: s.Width, Height: s.Height, LeadingOffsetSec: math.Max(0, start-containerStart)}
			info.VideoCodec, info.PixFmt, info.FrameRate, info.TimeBase = s.Codec, s.PixFmt, s.FrameRate, s.TimeBase
		}
	}
	return info, missing
}

// FirstAudioFrameArgs returns the ffprobe argv that decodes a file's first
// audio frame and prints its presentation time and the container start.
func FirstAudioFrameArgs(mediaPath string) []string {
	return []string{"-v", "error", "-select_streams", "a:0",
		"-read_intervals", "%+#8", "-show_entries", "frame=pts_time:format=start_time",
		"-of", "json", mediaPath}
}

// AudioLeadingSec is where a file's first decoded audio sample sits after
// the container start. Chromium's MP4/Opus recordings declare 80 ms of
// encoder priming (initial_padding 3840) without an edit list: the decoder
// drops those samples and the first real one is presented at 0.08 s, while a
// plain decode to WAV starts it at 0 — 80 ms ahead of the video [SL-QA-1].
func (r *Runner) AudioLeadingSec(mediaPath string) (float64, error) {
	out, err := exec.Command(r.FFprobePath, FirstAudioFrameArgs(mediaPath)...).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe first audio frame: %w", err)
	}
	return parseAudioLeading(out)
}

func parseAudioLeading(out []byte) (float64, error) {
	var probe struct {
		Frames []struct {
			PTS string `json:"pts_time"`
		} `json:"frames"`
		Format struct {
			Start string `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, fmt.Errorf("decode ffprobe output: %w", err)
	}
	if len(probe.Frames) == 0 {
		return 0, fmt.Errorf("no decodable audio frame")
	}
	pts, err := parseTimestamp(probe.Frames[0].PTS)
	if err != nil {
		return 0, err
	}
	start, err := parseTimestamp(probe.Format.Start)
	if err != nil {
		return 0, err
	}
	return math.Max(0, pts-start), nil
}

func parseTimestamp(s string) (float64, error) {
	if s == "" || s == "N/A" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("invalid source timestamp")
	}
	return v, nil
}

func parseRational(s string) (num, den int64, ok bool) {
	a, b, found := strings.Cut(s, "/")
	if !found {
		b = "1"
	}
	n, err1 := strconv.ParseInt(a, 10, 64)
	d, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0, 0, false
	}
	return n, d, true
}

// PacketTimestampsArgs lists every video packet's PTS (demux only, no decode).
func PacketTimestampsArgs(mediaPath string) []string {
	return []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts", "-of", "csv=p=0", mediaPath}
}

// CFRCopyEligible reports whether a camera recording can be stream-copied
// into the editor package instead of re-encoded. Every condition the CFR
// conform would otherwise guarantee must already hold:
//   - export codec H.264 and source H.264 4:2:0 8-bit (yuv420p),
//   - the video starts with the container (the conform resets PTS to zero),
//   - nominal rate equals the target rate exactly, and every packet sits on
//     that frame grid (±1 time-base tick; checked by demuxing all packets),
//   - accumulated clock drift over the take stays under half a frame.
//
// MediaRecorder output is VFR, so browser recordings never qualify today.
func (r *Runner) CFRCopyEligible(info MediaInfo, mediaPath string, fps, driftBeta float64, codec string) (bool, string) {
	if NormalizeCodec(codec) != CodecH264 {
		return false, "export codec is not H.264"
	}
	if info.VideoCodec != "h264" || info.PixFmt != "yuv420p" {
		return false, "source is not H.264 4:2:0"
	}
	if info.Video.LeadingOffsetSec != 0 {
		return false, "video does not start with the container"
	}
	if math.Abs(driftBeta-1)*info.DurationSec > 0.5/fps {
		return false, "clock drift exceeds half a frame over the take"
	}
	rn, rd, ok := parseRational(info.FrameRate)
	tn, td, ok2 := parseRational(FPSFilterRate(fps))
	if !ok || !ok2 || rn*td != tn*rd {
		return false, "nominal frame rate differs from the target"
	}
	bn, bd, ok := parseRational(info.TimeBase)
	if !ok {
		return false, "unknown time base"
	}
	// One frame in time-base ticks: (rd/rn) / (bn/bd).
	ticks := float64(rd*bd) / float64(rn*bn)
	out, err := exec.Command(r.FFprobePath, PacketTimestampsArgs(mediaPath)...).Output()
	if err != nil {
		return false, "packet scan failed"
	}
	return packetsOnGrid(out, ticks, info.DurationSec*float64(rn)/float64(rd))
}

func packetsOnGrid(csv []byte, ticks, expectedFrames float64) (bool, string) {
	var pts []int64
	for _, line := range strings.Split(strings.TrimSpace(string(csv)), "\n") {
		v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(line, ",")), 10, 64)
		if err != nil {
			return false, "packet without PTS"
		}
		pts = append(pts, v)
	}
	if len(pts) < 2 || math.Abs(float64(len(pts))-expectedFrames) > 1.5 {
		return false, "frame count does not match a CFR stream"
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i] < pts[j] })
	for i, p := range pts {
		if math.Abs(float64(p-pts[0])-float64(i)*ticks) > 1 {
			return false, "frame timestamps are not on the CFR grid"
		}
	}
	return true, ""
}
