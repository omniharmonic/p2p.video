package filespeech

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Guardrail (AGENTS.md): the built-in transcription broker only relays a
// path and recognizer output. It may not import the recording pipeline or
// live captions, and none of those may import it. Post-take transcripts read
// only the published export package (host/internal/integrations does that).
func TestFilespeechStaysOutOfRecordingAndCaptions(t *testing.T) {
	const mod = "github.com/omniharmonic/studiolink/host/internal/"
	forbidden := []string{"store", "upload", "postproc", "export", "refrec", "align", "ffmpeg", "sfu", "captions", "signaling"}
	for _, imp := range imports(t, ".") {
		if strings.HasPrefix(imp, mod) {
			t.Errorf("filespeech imports %s (it must stay dependency-free inside the module)", imp)
		}
	}
	for _, f := range forbidden {
		for _, imp := range imports(t, filepath.Join("..", f)) {
			if imp == mod+"filespeech" {
				t.Errorf("%s imports filespeech", f)
			}
		}
	}
}

func imports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			out = append(out, strings.Trim(imp.Path.Value, `"`))
		}
	}
	return out
}
