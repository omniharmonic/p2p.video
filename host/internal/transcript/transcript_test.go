package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func conf(v float64) *float64 { return &v }

// Two isolated tracks with different manifest offsets. File-relative times
// interleave only after each is shifted onto the take timeline.
func syntheticTracks() []Track {
	return []Track{
		{Speaker: "Ana", ParticipantID: "p-ana", SourceID: "t-ana", File: "audio/Ana.wav", OffsetSec: 0.25, Segments: []Segment{
			{Start: 0.5, End: 2.0, Text: "Welcome to the show.", Confidence: conf(0.9),
				Words: []Word{{Word: "Welcome", Start: 0.5, End: 0.9, Confidence: 0.9}, {Word: "to", Start: 0.9, End: 1.0, Confidence: 0.9}, {Word: "the", Start: 1.0, End: 1.2, Confidence: 0.9}, {Word: "show.", Start: 1.2, End: 2.0, Confidence: 0.9}}},
			{Start: 6.0, End: 7.0, Text: "  Tell us   more. "},
		}},
		{Speaker: "Ben", ParticipantID: "p-ben", SourceID: "t-ben", File: "audio/Ben.wav", OffsetSec: 1.5, Segments: []Segment{
			{Start: 1.5, End: 3.5, Text: "Thanks for having me."}, // take 3.0..5.0
			{Start: 6.0, End: 6.5, Text: ""},                      // dropped
			{Start: 4.75, End: 5.75, Text: "Sure."},               // take 6.25..7.25 (== Ana's 6.25 start)
		}},
	}
}

func TestMergeAppliesOffsetsAndOrdersByTakeTime(t *testing.T) {
	segs := Merge(syntheticTracks())
	type row struct {
		start, end float64
		speaker    string
		text       string
	}
	want := []row{
		{0.75, 2.25, "Ana", "Welcome to the show."},
		{3.0, 5.0, "Ben", "Thanks for having me."},
		{6.25, 7.25, "Ana", "Tell us more."}, // same start and end as Ben's: track order breaks the tie
		{6.25, 7.25, "Ben", "Sure."},
	}
	if len(segs) != len(want) {
		t.Fatalf("got %d segments: %+v", len(segs), segs)
	}
	for i, w := range want {
		g := segs[i]
		if g.Start != w.start || g.End != w.end || g.Speaker != w.speaker || g.Text != w.text {
			t.Errorf("segment %d = %+v, want %+v", i, g, w)
		}
	}
	if segs[0].ParticipantID != "p-ana" || segs[0].SourceID != "t-ana" || segs[0].Confidence == nil || *segs[0].Confidence != 0.9 {
		t.Errorf("identity/confidence not carried: %+v", segs[0])
	}
	if segs[0].Words[0].Start != 0.75 || segs[0].Words[3].End != 2.25 {
		t.Errorf("words not shifted: %+v", segs[0].Words)
	}
	if segs[1].Confidence != nil {
		t.Error("absent confidence must stay absent")
	}
}

func TestMergeIsDeterministicAndClampsBeforeTakeZero(t *testing.T) {
	tracks := []Track{{Speaker: "Early", SourceID: "a", OffsetSec: -0.4, Segments: []Segment{{Start: 0.1, End: 0.3, Text: "hi"}, {Start: 0.2, End: 1.0, Text: "there"}}}}
	segs := Merge(tracks)
	if segs[0].Start != 0 || segs[0].End != 0 || segs[1].Start != 0 || segs[1].End != 0.6 {
		t.Fatalf("clamp: %+v", segs)
	}
	a, _ := json.Marshal(Merge(syntheticTracks()))
	b, _ := json.Marshal(Merge(syntheticTracks()))
	if string(a) != string(b) {
		t.Fatal("merge must be deterministic")
	}
}

func TestTimestampsAndTimecodes(t *testing.T) {
	cases := map[float64][2]string{
		0:          {"00:00:00,000", "0:00"},
		1.2345:     {"00:00:01,235", "0:01"},
		61.5:       {"00:01:01,500", "1:01"},
		3725.0004:  {"01:02:05,000", "1:02:05"},
		-3:         {"00:00:00,000", "0:00"},
		35999.9995: {"10:00:00,000", "9:59:59"},
	}
	for in, want := range cases {
		if got := Timestamp(in, ","); got != want[0] {
			t.Errorf("Timestamp(%v) = %s, want %s", in, got, want[0])
		}
		if got := Timecode(in); got != want[1] {
			t.Errorf("Timecode(%v) = %s, want %s", in, got, want[1])
		}
	}
	if Timestamp(1.5, ".") != "00:00:01.500" {
		t.Error("VTT separator")
	}
}

func TestSRTGolden(t *testing.T) {
	got := SRT(Merge(syntheticTracks()))
	want := "1\n00:00:00,750 --> 00:00:02,250\nAna: Welcome to the show.\n\n" +
		"2\n00:00:03,000 --> 00:00:05,000\nBen: Thanks for having me.\n\n" +
		"3\n00:00:06,250 --> 00:00:07,250\nAna: Tell us more.\n\n" +
		"4\n00:00:06,250 --> 00:00:07,250\nBen: Sure.\n\n"
	if got != want {
		t.Fatalf("SRT mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func TestVTTGoldenEscapesAndUsesVoiceSpans(t *testing.T) {
	segs := []Segment{{Start: 1, End: 2.5, Speaker: "Ana <host>", Text: "a --> b & c"}}
	got := VTT(segs)
	want := "WEBVTT\n\n00:00:01.000 --> 00:00:02.500\n<v Ana &lt;host&gt;>a --&gt; b &amp; c\n\n"
	if got != want {
		t.Fatalf("VTT mismatch:\n%q\nwant\n%q", got, want)
	}
}

func TestCuesRespectCharacterAndDurationLimits(t *testing.T) {
	// 30 words over 24 s without word timings: split by length and by the 7 s limit.
	words := strings.Fields(strings.Repeat("podcast ", 30))
	seg := Segment{Start: 10, End: 34, Speaker: "Ben", Text: strings.Join(words, " ")}
	cues := Cues([]Segment{seg})
	if len(cues) < 4 {
		t.Fatalf("expected at least 4 cues for 24 s, got %d", len(cues))
	}
	if cues[0].Start != 10 || cues[len(cues)-1].End != 34 {
		t.Fatalf("cues must cover the segment: %+v", cues)
	}
	var rebuilt []string
	for i, c := range cues {
		if len(c.Text) > MaxCueChars {
			t.Errorf("cue %d too long: %d", i, len(c.Text))
		}
		if c.End-c.Start > MaxCueSec+1e-9 {
			t.Errorf("cue %d too long on screen: %.3f", i, c.End-c.Start)
		}
		if i > 0 && c.Start != cues[i-1].End {
			t.Errorf("cue %d not contiguous", i)
		}
		rebuilt = append(rebuilt, c.Text)
	}
	if strings.Join(rebuilt, " ") != seg.Text {
		t.Error("cues must keep every word in order")
	}
	// With word timings, boundaries follow words and the duration limit.
	var timed []Word
	for i := 0; i < 20; i++ {
		timed = append(timed, Word{Word: "word", Start: float64(i), End: float64(i) + 0.8})
	}
	wc := Cues([]Segment{{Start: 0, End: 19.8, Speaker: "Ana", Text: "x", Words: timed}})
	for i, c := range wc {
		if c.End-c.Start > MaxCueSec {
			t.Errorf("word cue %d spans %.2f s", i, c.End-c.Start)
		}
	}
	if wc[0].Start != 0 || wc[len(wc)-1].End != 19.8 {
		t.Errorf("word cues must cover all words: %+v", wc)
	}
	// Very short utterances stay readable.
	short := Cues([]Segment{{Start: 5, End: 5.1, Speaker: "A", Text: "Yes."}})
	if short[0].End != 5.5 {
		t.Errorf("min duration: %+v", short)
	}
}

func TestMarkdownFollowsMinutesConvention(t *testing.T) {
	doc := Document{Session: SessionMeta{Title: "Episode *42*"}, Take: 2,
		Speakers: []Speaker{{Name: "Ana"}, {Name: "Ben"}}, Segments: Merge(syntheticTracks())}
	md := Markdown(doc)
	for _, want := range []string{"# Episode *42* — Take 2\n", "Speakers: Ana, Ben\n", "## Transcript\n", "**Ana** (0:00): Welcome to the show.\n", "**Ben** (0:03): Thanks for having me.\n"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	empty := Markdown(Document{Take: 1})
	if !strings.Contains(empty, "No speech was recognized") || !strings.Contains(empty, "p2p.video session") {
		t.Errorf("empty markdown: %s", empty)
	}
}

func TestWriteReplacesAtomicallyAndReadsBack(t *testing.T) {
	dir := t.TempDir()
	doc := Document{Session: SessionMeta{ID: "s", Title: "T"}, Take: 1, Segments: Merge(syntheticTracks())}
	if err := Write(dir, doc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"transcript.json", "transcript.md", "transcript.srt", "transcript.vtt"} {
		if info, err := os.Stat(filepath.Join(dir, Dir, name)); err != nil || info.Size() == 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got, err := Read(dir)
	if err != nil || got.Schema != Schema || len(got.Segments) != 4 || got.Timeline != TimelineNote {
		t.Fatalf("read back: %+v %v", got, err)
	}
	doc.Segments = nil
	if err := Write(dir, doc); err != nil {
		t.Fatal(err)
	}
	got, _ = Read(dir)
	if len(got.Segments) != 0 {
		t.Fatal("rewrite must replace the previous transcript")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("staging left behind: %v", entries)
	}
}
