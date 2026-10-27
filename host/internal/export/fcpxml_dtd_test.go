package export

import (
	"encoding/xml"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Structural validation of the FCPXML we emit against Apple's FCPXML 1.10
// DTD [SL-P0-10].
//
// Apple's DTD ("Copyright (c) 2011-2021 Apple Inc. All rights reserved.")
// ships inside Final Cut Pro and is not openly licensed, so it is not
// vendored, and tests may not fetch it. The rules below transcribe, for
// every element p2p.video writes, its content model (as a regular
// expression over the child element names), its declared attributes, which
// are #REQUIRED, and their enumerations, from FCPXMLv1_10.dtd. The full DTD
// was also run once with `xmllint --dtdvalid` on 2026-10-26 (STATUS log).
// Beyond the DTD, it checks what the DTD leaves as CDATA but Final Cut
// requires: rational time syntax, edit times on frame boundaries,
// contiguous storylines, IDREF targets of the right kind, clips within
// their media, roles on connected audio.

type dtdRule struct {
	content  *regexp.Regexp // over "name name ..." of child elements
	attrs    map[string]string
	required []string
}

const anyCDATA = ""

func enum(v ...string) string { return "|" + strings.Join(v, "|") + "|" }

var (
	clipAttrs  = map[string]string{"lane": anyCDATA, "offset": anyCDATA, "name": anyCDATA, "start": anyCDATA, "duration": anyCDATA, "enabled": enum("0", "1")}
	mediaAttrs = map[string]string{"format": anyCDATA, "duration": anyCDATA, "tcStart": anyCDATA, "tcFormat": enum("DF", "NDF")}
	anchorItem = `audio|video|clip|title|caption|mc-clip|ref-clip|sync-clip|asset-clip|audition|spine`
	clipItem   = `audio|video|clip|title|mc-clip|ref-clip|sync-clip|asset-clip|audition|gap`
	markerItem = `marker|chapter-marker|rating|keyword|analysis-marker`
	intrinsicV = `(object-tracker )?(adjust-crop )?(adjust-corners )?(adjust-conform )?(adjust-transform )?(adjust-blend )?(adjust-stabilization )?(adjust-rollingShutter )?(adjust-360-transform )?(adjust-reorient )?(adjust-orientation )?(adjust-cinematic )?`
	intrinsicA = `(adjust-volume )?(adjust-panner )?`
	timing     = `(conform-rate )?(timeMap )?`
)

func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(`^` + s + `$`) }

// fcpxml110 transcribes FCPXMLv1_10.dtd for the elements p2p.video writes.
var fcpxml110 = map[string]dtdRule{
	"fcpxml":    {re(`(import-options )?(resources )?((library )|(event )*|((clip|audition|mc-clip|ref-clip|sync-clip|asset-clip|collection-folder|keyword-collection|smart-collection|project) )*)`), map[string]string{"version": enum("1.10")}, nil},
	"resources": {re(`((asset|effect|format|media|locator) )*`), map[string]string{}, nil},
	"format": {re(``), map[string]string{"id": anyCDATA, "name": anyCDATA, "frameDuration": anyCDATA, "fieldOrder": anyCDATA, "width": anyCDATA, "height": anyCDATA,
		"paspH": anyCDATA, "paspV": anyCDATA, "colorSpace": anyCDATA, "projection": anyCDATA, "stereoscopic": anyCDATA}, []string{"id"}},
	"asset": {re(`(media-rep )+(metadata )?`), map[string]string{"id": anyCDATA, "name": anyCDATA, "uid": anyCDATA, "start": anyCDATA, "duration": anyCDATA, "hasVideo": anyCDATA,
		"format": anyCDATA, "hasAudio": anyCDATA, "videoSources": anyCDATA, "audioSources": anyCDATA, "audioChannels": anyCDATA, "audioRate": anyCDATA,
		"customLUTOverride": anyCDATA, "colorSpaceOverride": anyCDATA, "projectionOverride": anyCDATA, "stereoscopicOverride": anyCDATA, "auxVideoFlags": anyCDATA}, []string{"id"}},
	"media-rep": {re(`(bookmark )?`), map[string]string{"kind": enum("original-media", "proxy-media"), "sig": anyCDATA, "src": anyCDATA, "suggestedFilename": anyCDATA}, []string{"src"}},
	"media":     {re(`((multicam|sequence) )?`), map[string]string{"id": anyCDATA, "name": anyCDATA, "uid": anyCDATA, "projectRef": anyCDATA, "modDate": anyCDATA}, []string{"id"}},
	"multicam":  {re(`(mc-angle )*(metadata )?`), merge(mediaAttrs, map[string]string{"renderFormat": anyCDATA}), []string{"format"}},
	"mc-angle":  {re(`((` + clipItem + `|transition) )*`), map[string]string{"name": anyCDATA, "angleID": anyCDATA}, []string{"angleID"}},
	"library":   {re(`((event|smart-collection) )*`), map[string]string{"location": anyCDATA, "colorProcessing": enum("standard", "wide", "wide-hdr")}, nil},
	"event":     {re(`((clip|audition|mc-clip|ref-clip|sync-clip|asset-clip|collection-folder|keyword-collection|smart-collection|project) )*`), map[string]string{"name": anyCDATA, "uid": anyCDATA}, nil},
	"project":   {re(`sequence `), map[string]string{"name": anyCDATA, "uid": anyCDATA, "id": anyCDATA, "modDate": anyCDATA}, nil},
	"sequence": {re(`(note )?spine (metadata )?`), merge(mediaAttrs, map[string]string{"audioLayout": enum("mono", "stereo", "surround"),
		"audioRate": enum("32k", "44.1k", "48k", "88.2k", "96k", "176.4k", "192k"), "renderFormat": anyCDATA, "keywords": anyCDATA}), []string{"format"}},
	"spine": {re(`((` + clipItem + `|transition) )*`), map[string]string{"lane": anyCDATA, "offset": anyCDATA, "name": anyCDATA, "format": anyCDATA}, nil},
	"mc-clip": {re(`(note )?` + timing + intrinsicA + `(mc-source )*((` + anchorItem + `) )*((` + markerItem + `) )*(filter-audio )*(metadata )?`),
		merge(clipAttrs, map[string]string{"ref": anyCDATA, "srcEnable": enum("all", "audio", "video"), "audioStart": anyCDATA, "audioDuration": anyCDATA, "modDate": anyCDATA}), []string{"ref", "duration"}},
	"mc-source": {re(`(audio-role-source )*` + intrinsicV + `((filter-video|filter-video-mask) )*`), map[string]string{"angleID": anyCDATA, "srcEnable": enum("all", "audio", "video", "none")}, []string{"angleID"}},
	"asset-clip": {re(`(note )?` + timing + intrinsicV + intrinsicA + `((` + anchorItem + `) )*((` + markerItem + `) )*(audio-channel-source )*((filter-video|filter-video-mask) )*(filter-audio )*(metadata )?`),
		merge(clipAttrs, map[string]string{"ref": anyCDATA, "srcEnable": enum("all", "audio", "video"), "audioStart": anyCDATA, "audioDuration": anyCDATA, "format": anyCDATA,
			"tcStart": anyCDATA, "tcFormat": enum("DF", "NDF"), "modDate": anyCDATA, "audioRole": anyCDATA, "videoRole": anyCDATA}), []string{"ref"}},
	"gap": {re(`(note )?((` + anchorItem + `) )*((` + markerItem + `) )*(metadata )?`),
		map[string]string{"name": anyCDATA, "offset": anyCDATA, "start": anyCDATA, "duration": anyCDATA, "enabled": enum("0", "1")}, []string{"duration"}},
}

// tb is the part of testing.TB the validators use (a collector in the
// negative test).
type tb interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

type collector struct{ errs []string }

func (c *collector) Helper()                   {}
func (c *collector) Errorf(f string, a ...any) { c.errs = append(c.errs, fmt.Sprintf(f, a...)) }
func (c *collector) Fatalf(f string, a ...any) { c.Errorf(f, a...); panic(c) }

// node is a generic parsed element.
type node struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []node     `xml:",any"`
}

func (n node) attr(k string) (string, bool) {
	for _, a := range n.Attrs {
		if a.Name.Local == k {
			return a.Value, true
		}
	}
	return "", false
}

func (n node) children(name string) []node {
	var out []node
	for _, c := range n.Children {
		if c.XMLName.Local == name {
			out = append(out, c)
		}
	}
	return out
}

func validateDTD(t tb, n node, path string) {
	t.Helper()
	name := n.XMLName.Local
	rule, ok := fcpxml110[name]
	if !ok {
		t.Errorf("%s: element <%s> is not one p2p.video is expected to write", path, name)
		return
	}
	var seq strings.Builder
	for _, c := range n.Children {
		seq.WriteString(c.XMLName.Local + " ")
	}
	if !rule.content.MatchString(seq.String()) {
		t.Errorf("%s: children %q violate the DTD content model of <%s>", path, seq.String(), name)
	}
	for _, a := range n.Attrs {
		allowed, ok := rule.attrs[a.Name.Local]
		if !ok {
			t.Errorf("%s: attribute %s is not declared for <%s>", path, a.Name.Local, name)
			continue
		}
		if allowed != anyCDATA && !strings.Contains(allowed, "|"+a.Value+"|") {
			t.Errorf("%s: %s=%q not in %s", path, a.Name.Local, a.Value, allowed)
		}
	}
	for _, req := range rule.required {
		if _, ok := n.attr(req); !ok {
			t.Errorf("%s: <%s> lacks #REQUIRED %s", path, name, req)
		}
	}
	for i, c := range n.Children {
		validateDTD(t, c, fmt.Sprintf("%s/%s[%d]", path, c.XMLName.Local, i))
	}
}

var timeRE = regexp.MustCompile(`^(0|-?[1-9][0-9]*)(/[1-9][0-9]*)?s$`)

// parseTime returns an FCPXML time as an exact rational.
func parseTime(t tb, v string) *big.Rat {
	t.Helper()
	if !timeRE.MatchString(v) {
		t.Fatalf("time %q is not an FCPXML rational time", v)
	}
	r, ok := new(big.Rat).SetString(strings.TrimSuffix(v, "s"))
	if !ok {
		t.Fatalf("bad time %q", v)
	}
	return r
}

func isMultiple(r, unit *big.Rat) bool {
	q := new(big.Rat).Quo(r, unit)
	return q.IsInt()
}

// validateFCPXMLSemantics checks the rules Final Cut enforces beyond the DTD.
func validateFCPXMLSemantics(t tb, root node) {
	t.Helper()
	resources := root.children("resources")[0]
	ids := map[string]node{}
	var walk func(node)
	walk = func(n node) {
		if id, ok := n.attr("id"); ok && n.XMLName.Local != "sequence" {
			if _, dup := ids[id]; dup {
				t.Errorf("duplicate ID %s", id)
			}
			ids[id] = n
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	refKind := func(id, kind, from string) node {
		n, ok := ids[id]
		if !ok || n.XMLName.Local != kind {
			t.Errorf("%s references %q: want a <%s>", from, id, kind)
		}
		return n
	}
	seq := root.children("library")[0].children("event")[0].children("project")[0].children("sequence")[0]
	seqFormatID, _ := seq.attr("format")
	seqFormat := refKind(seqFormatID, "format", "sequence")
	fd, _ := seqFormat.attr("frameDuration")
	frame := parseTime(t, fd)
	for _, f := range resources.children("format") {
		if v, ok := f.attr("frameDuration"); ok && parseTime(t, v).Cmp(frame) != 0 {
			t.Errorf("format %v frameDuration %s differs from the sequence's %s", f.Attrs, v, fd)
		}
	}
	assetDur := map[string]*big.Rat{}
	for _, a := range resources.children("asset") {
		id, _ := a.attr("id")
		d, ok := a.attr("duration")
		if !ok {
			t.Errorf("asset %s without duration", id)
			continue
		}
		assetDur[id] = parseTime(t, d)
		hasV, _ := a.attr("hasVideo")
		hasA, _ := a.attr("hasAudio")
		if hasV == "1" {
			f, ok := a.attr("format")
			if !ok {
				t.Errorf("video asset %s without format", id)
			} else {
				refKind(f, "format", "asset "+id)
			}
		}
		if rate, ok := a.attr("audioRate"); hasA == "1" && (!ok || rate != "48000") {
			t.Errorf("audio asset %s audioRate %q", id, rate)
		} else if hasA != "1" && ok {
			t.Errorf("silent asset %s declares audioRate", id)
		}
		src, _ := a.children("media-rep")[0].attr("src")
		if strings.ContainsAny(src, " #?") {
			t.Errorf("media-rep src %q is not URL-encoded", src)
		}
	}
	// Edit times are on frame boundaries; a storyline's items are contiguous
	// from 0s; clips stay within their media.
	checkClip := func(c node, where string, connected bool) {
		ref, _ := c.attr("ref")
		offset, _ := c.attr("offset")
		dur, _ := c.attr("duration")
		start, _ := c.attr("start")
		o, d, s := parseTime(t, offset), parseTime(t, dur), parseTime(t, start)
		if !isMultiple(o, frame) || !isMultiple(d, frame) {
			t.Errorf("%s: offset %s / duration %s not on %s frame boundaries", where, offset, dur, fd)
		}
		if o.Sign() < 0 || d.Sign() <= 0 || s.Sign() < 0 {
			t.Errorf("%s: negative or empty timing %s %s %s", where, offset, dur, start)
		}
		if c.XMLName.Local != "asset-clip" {
			return
		}
		asset := refKind(ref, "asset", where)
		if end := new(big.Rat).Add(s, d); assetDur[ref] != nil && end.Cmp(assetDur[ref]) > 0 {
			t.Errorf("%s: start+duration %s beyond asset duration %s", where, end.FloatString(4), assetDur[ref].FloatString(4))
		}
		hasA, _ := asset.attr("hasAudio")
		hasV, _ := asset.attr("hasVideo")
		_, fmtOK := c.attr("format")
		if hasV == "1" {
			if !isMultiple(s, frame) || !fmtOK {
				t.Errorf("%s: video clip start %s must be on a frame and carry its format", where, start)
			}
		} else if fmtOK {
			t.Errorf("%s: audio-only clip must not reference a video format", where)
		}
		if connected {
			lane, _ := c.attr("lane")
			if lane == "" || lane == "0" {
				t.Errorf("%s: anchored item needs a non-zero lane", where)
			}
			if role, ok := c.attr("audioRole"); hasA == "1" && (!ok || (role != "dialogue" && role != "effects")) {
				t.Errorf("%s: connected audio needs a dialogue/effects role, got %q", where, role)
			}
		}
	}
	for _, m := range resources.children("media") {
		for _, mc := range m.children("multicam") {
			f, _ := mc.attr("format")
			refKind(f, "format", "multicam")
			for _, angle := range mc.children("mc-angle") {
				next := new(big.Rat)
				for i, item := range angle.Children {
					where := fmt.Sprintf("angle %v item %d", angle.Attrs, i)
					o, _ := item.attr("offset")
					if parseTime(t, o).Cmp(next) != 0 {
						t.Errorf("%s: storyline gap/overlap: offset %s, previous item ends at %s", where, o, next.FloatString(4))
					}
					checkClip(item, where, false)
					d, _ := item.attr("duration")
					next.Add(next, parseTime(t, d))
				}
			}
		}
	}
	seqDur, _ := seq.attr("duration")
	sd := parseTime(t, seqDur)
	for _, item := range seq.children("spine")[0].Children {
		checkClip(item, "spine "+item.XMLName.Local, false)
		if d, _ := item.attr("duration"); parseTime(t, d).Cmp(sd) != 0 {
			t.Errorf("primary item duration %s != sequence %s", d, seqDur)
		}
		if item.XMLName.Local == "mc-clip" {
			ref, _ := item.attr("ref")
			media := refKind(ref, "media", "mc-clip")
			angles := map[string]bool{}
			for _, mc := range media.children("multicam") {
				for _, a := range mc.children("mc-angle") {
					id, _ := a.attr("angleID")
					angles[id] = true
				}
			}
			for _, src := range item.children("mc-source") {
				id, _ := src.attr("angleID")
				if !angles[id] {
					t.Errorf("mc-source selects missing angle %s", id)
				}
			}
		}
		for _, c := range item.Children {
			if c.XMLName.Local == "asset-clip" {
				checkClip(c, "connected "+c.XMLName.Local, true)
				o, _ := c.attr("offset")
				d, _ := c.attr("duration")
				if end := new(big.Rat).Add(parseTime(t, o), parseTime(t, d)); end.Cmp(sd) > 0 {
					t.Errorf("connected clip ends after the sequence")
				}
			}
		}
	}
}

func dtdFixtures() map[string][]ExportSource {
	two := []ExportSource{
		{ID: "cam1", Participant: "Ana & Co", Label: "Cam <1>", Kind: "video", FileRelPath: "video/Ana cam#1.mov", OffsetSec: -0.021, DurationSec: 61.27, Width: 1280, Height: 720},
		{ID: "mic1", Participant: "Ana & Co", Label: "Mic", Kind: "audio", FileRelPath: "audio/ana.wav", OffsetSec: 0.0137, DurationSec: 61.3093, Channels: 1},
		{ID: "cam2", Participant: "Bo", Label: "Cam", Kind: "video", FileRelPath: "video/bo.mov", OffsetSec: 0.083, DurationSec: 60.9, Width: 1080, Height: 1920, Source: SourcePhone},
		{ID: "mic2", Participant: "Bo", Label: "Mic", Kind: "audio", FileRelPath: "audio/bo.wav", OffsetSec: 0.0911, DurationSec: 61.0, Channels: 2},
		{ID: "scr", Participant: "Bo", Label: "Screen", Kind: "video", Source: SourceScreen, FileRelPath: "video/scr.mov", OffsetSec: 12.345, DurationSec: 10.01},
		{ID: "scr-a", Participant: "Bo", Label: "Screen Audio", Kind: "audio", Source: SourceScreen, FileRelPath: "audio/scr.wav", OffsetSec: 12.3461, DurationSec: 10.0},
	}
	return map[string][]ExportSource{
		"call":       two,
		"audio-only": {two[1], two[3]},
		"one-camera": {two[0]},
	}
}

func TestFCPXMLConformsToDTD110AndFinalCutRules(t *testing.T) {
	for name, sources := range dtdFixtures() {
		for _, fps := range []float64{23.976, 24, 25, 29.97, 30, 50, 59.94, 60} {
			t.Run(fmt.Sprintf("%s/%v", name, fps), func(t *testing.T) {
				dir := t.TempDir()
				if err := GenerateFCPXML(dir, "Show & Tell", 3, fps, sources); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "take_03_multicam.fcpxml"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(string(data), "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE fcpxml>\n") {
					t.Fatal("missing XML declaration or DOCTYPE")
				}
				var root node
				if err := xml.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				validateDTD(t, root, "fcpxml")
				validateFCPXMLSemantics(t, root)
			})
		}
	}
}

// The microphone keeps its sub-frame position: clip offset (a frame) minus
// the head trim lands exactly on the source offset (± half a sample).
func TestFCPXMLMicrophoneIsSampleAccurate(t *testing.T) {
	dir := t.TempDir()
	src := []ExportSource{
		{ID: "cam", Kind: "video", OffsetSec: 0, DurationSec: 10},
		{ID: "mic", Kind: "audio", OffsetSec: 0.0137, DurationSec: 10},
	}
	if err := GenerateFCPXML(dir, "x", 1, 29.97, src); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
	var doc multicamPositionDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	mic := doc.Sequence.Edit.Clips[0]
	at := new(big.Rat).Sub(parseTime(t, mic.Offset), parseTime(t, mic.Start))
	got, _ := at.Float64()
	if d := got - 0.0137; d > 0.5/48000 || d < -0.5/48000 {
		t.Fatalf("microphone lands at %.6f s, want 0.013700 (offset %s start %s)", got, mic.Offset, mic.Start)
	}
}

// A source that started before the take's mark moves the whole timeline
// later: nothing is negative and relative offsets are kept.
func TestTimelinesShiftNegativeOffsets(t *testing.T) {
	dir := t.TempDir()
	src := []ExportSource{
		{ID: "cam", Kind: "video", FileAbsPath: "/m/cam.mov", OffsetSec: -0.1, DurationSec: 5},
		{ID: "cam2", Kind: "video", FileAbsPath: "/m/cam2.mov", OffsetSec: 0.2, DurationSec: 5},
	}
	if err := GenerateFCPXML(dir, "x", 1, 30, src); err != nil {
		t.Fatal(err)
	}
	if err := GenerateXMEML(dir, "x", 1, 30, src); err != nil {
		t.Fatal(err)
	}
	f, _ := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
	x, _ := os.ReadFile(filepath.Join(dir, "take_01_premiere.xml"))
	if strings.Contains(string(f), `="-`) || strings.Contains(string(x), "<start>-") {
		t.Fatal("negative timeline position")
	}
	if !strings.Contains(string(f), `offset="9/30s"`) || !strings.Contains(string(x), "<start>9</start>") || !strings.Contains(string(x), "<start>0</start>") {
		t.Fatalf("relative offsets not kept:\n%s\n%s", f, x)
	}
}

// The validator itself must catch what it claims to: run it on the shape of
// the exporter's pre-SL-P0-10 output for a camera that started before the
// take mark (the full DTD accepted that file), plus two DTD violations.
func TestFCPXMLValidatorRejectsKnownViolations(t *testing.T) {
	const old = `<fcpxml version="1.10"><resources>
<format id="r1" name="FFVideoFormat1080p30" frameDuration="1/30s" width="1920" height="1080"/>
<asset id="r2" name="Cam" start="0s" duration="1800/30s" hasVideo="1" hasAudio="0" format="r1" audioRate="48000"><media-rep kind="original-media" src="./video/a b.mov"/></asset>
<asset id="r3" name="Mic" start="0s" duration="1800/30s" hasVideo="0" hasAudio="1" audioRate="48000"><media-rep kind="original-media" src="./audio/a.wav"/></asset>
<media id="r4" name="M"><multicam format="r1" tcStart="0s" tcFormat="NDF" renderFormat="original">
<mc-angle name="A" angleID="angle-1"><asset-clip name="Cam" ref="r2" offset="-1/30s" duration="1800/30s" start="0s" format="r1"/></mc-angle>
</multicam></media></resources>
<library><event name="T"><project name="P"><sequence format="r1" duration="1801/30s" tcStart="0s" tcFormat="NDF" audioLayout="stereo" audioRate="48000">
<spine><mc-clip name="M" ref="r4" offset="0s" start="0s" duration="1801/30s" srcEnable="video"><mc-source angleID="angle-9" srcEnable="video"/>
<asset-clip name="Mic" ref="r3" offset="0/30s" duration="1801/30s" start="0s" lane="-1" format="r1"/></mc-clip></spine></sequence></project></event></library></fcpxml>`
	var root node
	if err := xml.Unmarshal([]byte(old), &root); err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	func() {
		defer func() { _ = recover() }()
		validateDTD(c, root, "fcpxml")
		validateFCPXMLSemantics(c, root)
	}()
	all := strings.Join(c.errs, "\n")
	for _, want := range []string{
		`audioRate="48000" not in`, // the sequence's audioRate is an enumeration (48k)
		"silent asset r2 declares audioRate",
		"not URL-encoded",
		"storyline gap/overlap",
		"negative or empty timing",
		"beyond asset duration",
		"audio-only clip must not reference a video format",
		"connected audio needs a dialogue/effects role",
		"mc-source selects missing angle",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("validator missed %q; got:\n%s", want, all)
		}
	}
}
