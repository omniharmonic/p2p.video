package postproc

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Export stages, in pipeline order. Stage wall time is summed over every job
// of that stage, so parallel jobs can add up to more than the elapsed time.
const (
	StageValidate  = "validate"
	StageProbe     = "probe"
	StageAudio     = "audio"
	StageVideo     = "video"
	StageVerify    = "verify"
	StageTimelines = "timelines"
	StagePublish   = "publish"
)

var stageOrder = []string{StageValidate, StageProbe, StageAudio, StageVideo, StageVerify, StageTimelines, StagePublish}

// StageTiming is the summed wall time of one export stage.
type StageTiming struct {
	Stage   string  `json:"stage"`
	Seconds float64 `json:"seconds"`
	Jobs    int     `json:"jobs"`
}

// Status is the host-visible state of a take's export. It is memory-only:
// a daemon restart forgets it, and the persisted take status stays canonical.
type Status struct {
	TakeID     string        `json:"takeId"`
	State      string        `json:"state"` // running | complete | failed
	Stage      string        `json:"stage"`
	Percent    float64       `json:"percent"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
	ElapsedSec float64       `json:"elapsedSec"`
	Error      string        `json:"error,omitempty"`
	Stages     []StageTiming `json:"stages,omitempty"`
	// VideoWorkers and VideoCodec describe how the media jobs ran.
	VideoWorkers int    `json:"videoWorkers,omitempty"`
	VideoCodec   string `json:"videoCodec,omitempty"`
	// StreamCopies counts cameras copied without re-encoding.
	StreamCopies int `json:"streamCopies,omitempty"`
}

// run tracks one export. Media jobs weigh by their media duration (video
// encodes far more than audio), and finalization keeps the last 5 percent.
type run struct {
	mu       sync.Mutex
	status   Status
	weights  map[string]float64
	fraction map[string]float64
	timings  map[string]*StageTiming
	active   map[string]int
	final    float64
}

func newRun(takeID string) *run {
	return &run{
		status:   Status{TakeID: takeID, State: "running", Stage: StageValidate, StartedAt: time.Now()},
		weights:  map[string]float64{},
		fraction: map[string]float64{},
		timings:  map[string]*StageTiming{},
		active:   map[string]int{},
	}
}

// configure records how the media jobs run.
func (r *run) configure(videoWorkers int, codec string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.VideoWorkers, r.status.VideoCodec = videoWorkers, codec
}

func (r *run) streamCopied() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.StreamCopies++
}

// addJob registers a media job's weight before any job starts.
func (r *run) addJob(key string, weight float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if weight <= 0 {
		weight = 1
	}
	r.weights[key] = weight
}

// jobProgress records a media job's completed fraction (monotonic).
func (r *run) jobProgress(key string, fraction float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fraction < 0 {
		fraction = 0
	} else if fraction > 1 {
		fraction = 1
	}
	if fraction > r.fraction[key] {
		r.fraction[key] = fraction
	}
}

// finalStep advances the finalization share (verify, timelines, publish).
func (r *run) finalStep(fraction float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fraction > r.final {
		r.final = fraction
	}
}

// timed runs fn as one job of stage and records its wall time. The reported
// stage is the earliest pipeline stage with a running job.
func (r *run) timed(stage string, fn func() error) error {
	r.mu.Lock()
	r.active[stage]++
	r.refreshStageLocked()
	r.mu.Unlock()
	start := time.Now()
	err := fn()
	elapsed := time.Since(start).Seconds()
	r.mu.Lock()
	t := r.timings[stage]
	if t == nil {
		t = &StageTiming{Stage: stage}
		r.timings[stage] = t
	}
	t.Seconds += elapsed
	t.Jobs++
	r.active[stage]--
	r.refreshStageLocked()
	r.mu.Unlock()
	return err
}

func (r *run) refreshStageLocked() {
	for _, stage := range stageOrder {
		if r.active[stage] > 0 {
			r.status.Stage = stage
			return
		}
	}
}

func (r *run) finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.status.FinishedAt = &now
	if err != nil {
		r.status.State = "failed"
		r.status.Error = shortError(err)
		return
	}
	r.status.State = "complete"
	r.status.Stage = "complete"
	for key := range r.weights {
		r.fraction[key] = 1
	}
	r.final = 1
}

func (r *run) snapshot() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	end := time.Now()
	if s.FinishedAt != nil {
		end = *s.FinishedAt
	}
	s.ElapsedSec = end.Sub(s.StartedAt).Seconds()
	var total, done float64
	for key, w := range r.weights {
		total += w
		done += w * r.fraction[key]
	}
	media := 0.0
	if total > 0 {
		media = done / total
	}
	s.Percent = float64(int((media*95+r.final*5)*10)) / 10
	s.Stages = make([]StageTiming, 0, len(r.timings))
	for _, t := range r.timings {
		s.Stages = append(s.Stages, *t)
	}
	rank := map[string]int{}
	for i, stage := range stageOrder {
		rank[stage] = i
	}
	sort.Slice(s.Stages, func(i, j int) bool { return rank[s.Stages[i].Stage] < rank[s.Stages[j].Stage] })
	return s
}

// shortError keeps the first line of an export error, bounded, so ffmpeg
// stderr dumps never reach the UI.
func shortError(err error) string {
	msg := err.Error()
	if i := strings.IndexAny(msg, "\r\n"); i >= 0 {
		msg = msg[:i]
	}
	if i := strings.Index(msg, ", stderr:"); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	return msg
}

// ExportStatus reports the latest export of a take since the daemon started.
func (p *Pipeline) ExportStatus(takeID string) (Status, bool) {
	p.progressMu.Lock()
	r := p.runs[takeID]
	p.progressMu.Unlock()
	if r == nil {
		return Status{}, false
	}
	return r.snapshot(), true
}

func (p *Pipeline) beginRun(takeID string) *run {
	r := newRun(takeID)
	p.progressMu.Lock()
	if p.runs == nil {
		p.runs = map[string]*run{}
	}
	p.runs[takeID] = r
	p.progressMu.Unlock()
	return r
}
