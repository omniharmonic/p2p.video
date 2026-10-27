package captions

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Guardrail (AGENTS.md): captions come from WebRTC-received call audio, so
// they must stay out of the recording pipeline in both directions. This
// package may not import recording, upload, storage or export packages, and
// none of those packages may import it. The same holds for the post-take
// transcript packages (filespeech, integrations, transcript) [SL-P4-3].
func TestCaptionsStayOutOfTheRecordingPipeline(t *testing.T) {
	const mod = "github.com/omniharmonic/studiolink/host/internal/"
	forbidden := []string{"store", "upload", "postproc", "export", "refrec", "align", "ffmpeg", "sfu", "filespeech", "integrations", "transcript"}
	for _, imp := range imports(t, ".") {
		for _, f := range forbidden {
			if imp == mod+f {
				t.Errorf("captions imports %s", imp)
			}
		}
	}
	for _, f := range forbidden {
		for _, imp := range imports(t, filepath.Join("..", f)) {
			if imp == mod+"captions" {
				t.Errorf("%s imports captions", f)
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
		if !strings.HasSuffix(e.Name(), ".go") {
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
