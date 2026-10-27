package export

import (
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Structural validation of premiere.xml against the FCP7 XML interchange
// format (xmeml v4) as Premiere Pro reads and writes it [SL-P0-10]. Premiere
// is not installed on the build machine and Apple's xmeml DTD is not openly
// licensed, so the rules Premiere relies on are encoded here:
//
//   - <xmeml version="4"> with one <sequence id> whose children are, in
//     order, duration, rate, name, media, timecode;
//   - rate = timebase + ntsc (TRUE for 23.976/29.97/59.94, timebase 24/30/60);
//   - every clipitem: unique id, name, enabled, duration, rate, start, end,
//     in, out, file in that order; end-start = out-in; 0 <= in < out <=
//     file duration; inside the sequence duration;
//   - every file element is defined once with a unique id, name, a pathurl
//     that is file://localhost/ + the percent-encoded absolute path (decodes
//     back to exactly the original path), rate, duration, timecode and media;
//   - audio files declare depth, samplerate 48000 and channelcount; the
//     clipitem selects sourcetrack audio/1, one track per microphone (a
//     multichannel file plays as one clip; no separate per-channel links);
//   - video files declare their native width/height; the sequence format is
//     1920x1080 square pixels;
//   - no <link> elements: every camera and microphone is a separate file
//     from a separate device, and linking would merge them into one clip.

type xRate struct {
	Timebase int    `xml:"timebase"`
	NTSC     string `xml:"ntsc"`
}

type xTimecode struct {
	Rate          xRate  `xml:"rate"`
	String        string `xml:"string"`
	Frame         int    `xml:"frame"`
	DisplayFormat string `xml:"displayformat"`
}

type xFile struct {
	ID       string    `xml:"id,attr"`
	Name     string    `xml:"name"`
	PathURL  string    `xml:"pathurl"`
	Rate     xRate     `xml:"rate"`
	Duration int       `xml:"duration"`
	Timecode xTimecode `xml:"timecode"`
	Media    struct {
		Video *struct {
			Width  int `xml:"samplecharacteristics>width"`
			Height int `xml:"samplecharacteristics>height"`
		} `xml:"video"`
		Audio *struct {
			Depth        int `xml:"samplecharacteristics>depth"`
			SampleRate   int `xml:"samplecharacteristics>samplerate"`
			ChannelCount int `xml:"channelcount"`
		} `xml:"audio"`
	} `xml:"media"`
}

type xClip struct {
	ID          string `xml:"id,attr"`
	Name        string `xml:"name"`
	Enabled     string `xml:"enabled"`
	Duration    int    `xml:"duration"`
	Rate        xRate  `xml:"rate"`
	Start       int    `xml:"start"`
	End         int    `xml:"end"`
	In          int    `xml:"in"`
	Out         int    `xml:"out"`
	File        xFile  `xml:"file"`
	SourceTrack *struct {
		MediaType  string `xml:"mediatype"`
		TrackIndex int    `xml:"trackindex"`
	} `xml:"sourcetrack"`
}

type xTrack struct {
	Clips []xClip `xml:"clipitem"`
}

type xDoc struct {
	XMLName  xml.Name `xml:"xmeml"`
	Version  string   `xml:"version,attr"`
	Sequence struct {
		ID       string    `xml:"id,attr"`
		Duration int       `xml:"duration"`
		Rate     xRate     `xml:"rate"`
		Name     string    `xml:"name"`
		Timecode xTimecode `xml:"timecode"`
		Media    struct {
			Video struct {
				Format struct {
					Rate   xRate  `xml:"samplecharacteristics>rate"`
					Width  int    `xml:"samplecharacteristics>width"`
					Height int    `xml:"samplecharacteristics>height"`
					PAR    string `xml:"samplecharacteristics>pixelaspectratio"`
				} `xml:"format"`
				Tracks []xTrack `xml:"track"`
			} `xml:"video"`
			Audio struct {
				Tracks []xTrack `xml:"track"`
			} `xml:"audio"`
		} `xml:"media"`
	} `xml:"sequence"`
}

// childOrder returns the element names directly inside each element whose
// name is parent, in document order.
func childOrder(t *testing.T, data []byte, parent string) [][]string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	var out [][]string
	var stack []string
	var cur []int // indexes into out for open parents
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if len(stack) > 0 && stack[len(stack)-1] == parent && len(cur) > 0 {
				out[cur[len(cur)-1]] = append(out[cur[len(cur)-1]], e.Name.Local)
			}
			stack = append(stack, e.Name.Local)
			if e.Name.Local == parent {
				out = append(out, nil)
				cur = append(cur, len(out)-1)
			}
		case xml.EndElement:
			if stack[len(stack)-1] == parent {
				cur = cur[:len(cur)-1]
			}
			stack = stack[:len(stack)-1]
		}
	}
	return out
}

var tcRE = regexp.MustCompile(`^\d\d:\d\d:\d\d[:;]\d\d$`)

func validateXMEML(t *testing.T, data []byte, fps float64, sources []ExportSource) {
	t.Helper()
	var doc xDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE xmeml>\n") {
		t.Error("missing XML declaration or DOCTYPE xmeml")
	}
	if doc.Version != "4" && doc.Version != "5" {
		t.Errorf("xmeml version %q", doc.Version)
	}
	rfr := ResolveFrameRate(fps)
	wantNTSC := map[bool]string{true: "TRUE", false: "FALSE"}[rfr.IsNTSC]
	checkRate := func(where string, r xRate) {
		if r.Timebase != rfr.Timebase || r.NTSC != wantNTSC {
			t.Errorf("%s rate %+v, want timebase %d ntsc %s", where, r, rfr.Timebase, wantNTSC)
		}
	}
	seq := doc.Sequence
	if seq.ID == "" || seq.Name == "" || seq.Duration <= 0 {
		t.Errorf("sequence id/name/duration missing: %q %q %d", seq.ID, seq.Name, seq.Duration)
	}
	checkRate("sequence", seq.Rate)
	checkRate("sequence timecode", seq.Timecode.Rate)
	if !tcRE.MatchString(seq.Timecode.String) || seq.Timecode.DisplayFormat != "NDF" {
		t.Errorf("sequence timecode %+v", seq.Timecode)
	}
	if got := childOrder(t, data, "sequence")[0]; strings.Join(got, " ") != "duration rate name media timecode" {
		t.Errorf("sequence children %v", got)
	}
	f := seq.Media.Video.Format
	checkRate("sequence format", f.Rate)
	if f.Width != 1920 || f.Height != 1080 || f.PAR != "square" {
		t.Errorf("sequence format %+v", f)
	}
	for _, order := range childOrder(t, data, "clipitem") {
		want := "name enabled duration rate start end in out file"
		got := strings.Join(order, " ")
		if got != want && got != want+" sourcetrack" {
			t.Errorf("clipitem children %q", got)
		}
	}
	if strings.Contains(string(data), "<link>") {
		t.Error("separate device files must not be linked")
	}
	byPath := map[string]ExportSource{}
	for _, s := range sources {
		byPath[s.FileAbsPath] = s
	}
	ids, fileIDs := map[string]bool{}, map[string]bool{}
	check := func(kind string, tracks []xTrack) int {
		n := 0
		for ti, tr := range tracks {
			if len(tr.Clips) != 1 {
				t.Errorf("%s track %d has %d clips; one per source", kind, ti, len(tr.Clips))
			}
			for _, c := range tr.Clips {
				n++
				where := kind + " " + c.ID
				if ids[c.ID] || fileIDs[c.File.ID] {
					t.Errorf("%s: duplicate clip or file id", where)
				}
				ids[c.ID], fileIDs[c.File.ID] = true, true
				checkRate(where, c.Rate)
				checkRate(where+" file", c.File.Rate)
				checkRate(where+" file timecode", c.File.Timecode.Rate)
				if c.Enabled != "TRUE" || c.Name == "" {
					t.Errorf("%s: enabled %q name %q", where, c.Enabled, c.Name)
				}
				if c.Start < 0 || c.End-c.Start != c.Out-c.In || c.In < 0 || c.Out <= c.In || c.Out > c.File.Duration || c.Duration != c.File.Duration || c.End > seq.Duration {
					t.Errorf("%s: timing start %d end %d in %d out %d duration %d file %d sequence %d", where, c.Start, c.End, c.In, c.Out, c.Duration, c.File.Duration, seq.Duration)
				}
				u, err := url.Parse(c.File.PathURL)
				if err != nil || u.Scheme != "file" || u.Host != "localhost" || !strings.HasPrefix(c.File.PathURL, "file://localhost/") {
					t.Errorf("%s: pathurl %q is not file://localhost/…", where, c.File.PathURL)
					continue
				}
				if strings.ContainsAny(c.File.PathURL, " #?") {
					t.Errorf("%s: pathurl %q not percent-encoded", where, c.File.PathURL)
				}
				src, ok := byPath[u.Path]
				if !ok {
					t.Errorf("%s: pathurl %q decodes to %q, not an exported file", where, c.File.PathURL, u.Path)
					continue
				}
				if c.File.Name != filepath.Base(src.FileAbsPath) {
					t.Errorf("%s: file name %q", where, c.File.Name)
				}
				switch kind {
				case "video":
					v := c.File.Media.Video
					w, h := src.Width, src.Height
					if w <= 0 || h <= 0 {
						w, h = 1920, 1080
					}
					if v == nil || v.Width != w || v.Height != h || c.SourceTrack != nil {
						t.Errorf("%s: video media %+v want %dx%d", where, v, w, h)
					}
				case "audio":
					a := c.File.Media.Audio
					if a == nil || a.Depth != 24 || a.SampleRate != 48000 || a.ChannelCount < 1 || (src.Channels > 0 && a.ChannelCount != src.Channels) {
						t.Errorf("%s: audio media %+v (source channels %d)", where, a, src.Channels)
					}
					if c.SourceTrack == nil || c.SourceTrack.MediaType != "audio" || c.SourceTrack.TrackIndex != 1 {
						t.Errorf("%s: sourcetrack %+v", where, c.SourceTrack)
					}
				}
			}
		}
		return n
	}
	var nv, na int
	for _, s := range sources {
		if s.Kind == "video" {
			nv++
		} else {
			na++
		}
	}
	if got := check("video", seq.Media.Video.Tracks); got != nv {
		t.Errorf("%d video clips, want %d", got, nv)
	}
	if got := check("audio", seq.Media.Audio.Tracks); got != na {
		t.Errorf("%d audio clips, want %d", got, na)
	}
}

func TestXMEMLConformsToPremiereStructure(t *testing.T) {
	for name, sources := range dtdFixtures() {
		abs := make([]ExportSource, len(sources))
		for i, s := range sources {
			s.FileAbsPath = "/Users/host/Movies/p2p.video Exports/Show #1 & 50%/" + s.FileRelPath
			abs[i] = s
		}
		for _, fps := range []float64{23.976, 25, 29.97, 30, 59.94} {
			t.Run(fmt.Sprintf("%s/%v", name, fps), func(t *testing.T) {
				dir := t.TempDir()
				if err := GenerateXMEML(dir, "Show & Tell", 1, fps, abs); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "take_01_premiere.xml"))
				if err != nil {
					t.Fatal(err)
				}
				validateXMEML(t, data, fps, abs)
			})
		}
	}
}
