package ffmpeg

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// LivestreamPreset is an output geometry/bitrate profile for RTMP publishing.
type LivestreamPreset struct {
	Name              string
	Width, Height     int
	FPS               int
	VideoBitrateKbps  int
	AudioBitrateKbps  int
	AudioSampleRateHz int
	KeyframeSec       int
}

var (
	// Livestream720p30 suits most uplinks and every major platform.
	Livestream720p30 = LivestreamPreset{Name: "720p30", Width: 1280, Height: 720, FPS: 30, VideoBitrateKbps: 3000, AudioBitrateKbps: 160, AudioSampleRateHz: 48000, KeyframeSec: 2}
	// Livestream1080p30 follows YouTube's/Twitch's recommended 1080p30 range.
	Livestream1080p30 = LivestreamPreset{Name: "1080p30", Width: 1920, Height: 1080, FPS: 30, VideoBitrateKbps: 4500, AudioBitrateKbps: 160, AudioSampleRateHz: 48000, KeyframeSec: 2}
)

// LivestreamPresetByName returns a known preset; unknown names are an error.
func LivestreamPresetByName(name string) (LivestreamPreset, error) {
	switch name {
	case "", Livestream720p30.Name:
		return Livestream720p30, nil
	case Livestream1080p30.Name:
		return Livestream1080p30, nil
	}
	return LivestreamPreset{}, fmt.Errorf("unknown livestream preset %q", name)
}

// HasOutputProtocol reports whether this ffmpeg build can write to the given
// protocol (e.g. "rtmp", "rtmps"). Builds configured with --disable-network
// have neither.
func (r *Runner) HasOutputProtocol(name string) bool {
	out, err := exec.Command(r.FFmpegPath, "-hide_banner", "-protocols").Output()
	if err != nil {
		return false
	}
	return outputProtocolListed(string(out), name)
}

func outputProtocolListed(listing, name string) bool {
	_, after, ok := strings.Cut(listing, "Output:")
	if !ok {
		return false
	}
	for _, f := range strings.Fields(after) {
		if f == name {
			return true
		}
	}
	return false
}

// BuildLivestreamEncodeArgs returns the ffmpeg argv that reads a browser
// MediaRecorder stream (WebM or fragmented MP4) on stdin, re-encodes it with
// VideoToolbox H.264 (never libx264: no GPL) and native AAC, and writes one
// FLV stream to stdout. The daemon parses that FLV and publishes it to every
// RTMP(S) destination itself (ADR-014, SL-P3-3), so the argv contains no
// destination URL and no stream key: it is safe to show in `ps`. Progress is
// written as key=value lines on file descriptor 3 (the launcher passes a
// pipe there), keeping stdout for media.
func BuildLivestreamEncodeArgs(p LivestreamPreset) ([]string, error) {
	if p.Width <= 0 || p.Height <= 0 || p.FPS <= 0 || p.VideoBitrateKbps <= 0 || p.AudioBitrateKbps <= 0 || p.AudioSampleRateHz <= 0 || p.KeyframeSec <= 0 {
		return nil, errors.New("invalid livestream preset")
	}
	gop := p.FPS * p.KeyframeSec
	vb := strconv.Itoa(p.VideoBitrateKbps) + "k"
	w, h := strconv.Itoa(p.Width), strconv.Itoa(p.Height)
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "warning", "-nostats",
		"-progress", "pipe:3", "-stats_period", "1",
		"-fflags", "+genpts+discardcorrupt",
		// MediaRecorder headers carry codec parameters; ffmpeg's default 5s
		// analysis would add 5s of latency and a burst on connect.
		"-probesize", "1000000", "-analyzeduration", "1000000",
		"-thread_queue_size", "1024",
		"-i", "pipe:0",
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", "scale=" + w + ":" + h + ":force_original_aspect_ratio=decrease,pad=" + w + ":" + h + ":(ow-iw)/2:(oh-ih)/2,format=yuv420p",
		"-fps_mode", "cfr", "-r", strconv.Itoa(p.FPS),
		"-c:v", "h264_videotoolbox", "-realtime", "1", "-allow_sw", "1",
		"-profile:v", "high", "-b:v", vb, "-maxrate", vb, "-bufsize", strconv.Itoa(p.VideoBitrateKbps*2) + "k",
		"-g", strconv.Itoa(gop), "-force_key_frames", "expr:gte(t,n_forced*" + strconv.Itoa(p.KeyframeSec) + ")",
		"-af", "aresample=async=1000:first_pts=0",
		"-c:a", "aac", "-b:a", strconv.Itoa(p.AudioBitrateKbps) + "k", "-ar", strconv.Itoa(p.AudioSampleRateHz), "-ac", "2",
		// stdout is a pipe: never seek back to patch duration/filesize, and
		// flush every packet so the daemon sees media as soon as it exists.
		"-flvflags", "no_duration_filesize", "-flush_packets", "1",
		"-f", "flv", "pipe:1",
	}, nil
}
