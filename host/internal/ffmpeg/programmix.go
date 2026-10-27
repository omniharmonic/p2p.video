package ffmpeg

import (
	"fmt"
	"strconv"
	"strings"
)

// MixInput is one isolated microphone placed on the take timeline.
type MixInput struct {
	Path      string
	OffsetSec float64 // manifest offsetSec: where the file's t=0 sits after take zero
}

// BuildProgramMixArgs returns argv for a mono 48 kHz reference mix of the
// isolated microphones on the take timeline (t=0 is take zero), for tools
// that want one file per take, such as the clip editor. Inputs with a
// positive offset are delayed; a negative offset trims the file's head. The
// mix is summed without normalization and soft-limited so quiet speakers stay
// at their recorded level. It is a reference, not a mastered mix.
func BuildProgramMixArgs(inputs []MixInput, outPath string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("program mix needs at least one input")
	}
	args := []string{"-y", "-nostdin"}
	for _, in := range inputs {
		if in.Path == "" || strings.HasPrefix(in.Path, "-") {
			return nil, fmt.Errorf("invalid mix input path %q", in.Path)
		}
		args = append(args, "-i", in.Path)
	}
	var graph []string
	var labels strings.Builder
	for i, in := range inputs {
		label := fmt.Sprintf("[m%d]", i)
		switch {
		case in.OffsetSec > 0:
			ms := strconv.FormatInt(int64(in.OffsetSec*1000+0.5), 10)
			graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,aformat=channel_layouts=mono,adelay=delays=%s:all=1%s", i, ms, label))
		case in.OffsetSec < 0:
			graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,aformat=channel_layouts=mono,atrim=start=%s,asetpts=PTS-STARTPTS%s", i, strconv.FormatFloat(-in.OffsetSec, 'f', 3, 64), label))
		default:
			graph = append(graph, fmt.Sprintf("[%d:a]aresample=48000,aformat=channel_layouts=mono%s", i, label))
		}
		labels.WriteString(label)
	}
	graph = append(graph, fmt.Sprintf("%samix=inputs=%d:duration=longest:normalize=0,alimiter=limit=0.95:latency=1[mix]", labels.String(), len(inputs)))
	args = append(args,
		"-filter_complex", strings.Join(graph, ";"),
		"-map", "[mix]",
		"-ac", "1", "-ar", "48000",
		"-c:a", "pcm_s16le",
		outPath,
	)
	return args, nil
}
