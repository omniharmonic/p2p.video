package parachute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

const testToken = "eyJhbGciOiJFZERTQSJ9.eyJzY29wZSI6InZhdWx0OnRlc3Q6d3JpdGUifQ.c2lnbmF0dXJlLXNlY3JldA"

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

type recorded struct {
	Method, Path, Auth, ContentType string
	Body                            map[string]any
}

// fakeVault mimics the Parachute notes API closely enough for the client:
// POST creates (409 on a taken path), PATCH honours if_updated_at.
type fakeVault struct {
	mu       sync.Mutex
	requests []recorded
	notes    map[string]map[string]any // id -> note
	paths    map[string]string
	next     int
	fail     int // status to return for every request when non-zero
	echoAuth bool
}

func newFakeVault() *fakeVault {
	return &fakeVault{notes: map[string]map[string]any{}, paths: map[string]string{}}
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(data, &body)
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		msg := "boom"
		if f.echoAuth {
			msg = "bad header " + r.Header.Get("Authorization")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/vault/test/api/notes":
		path, _ := body["path"].(string)
		if _, taken := f.paths[path]; taken {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "path exists"})
			return
		}
		f.next++
		id := "n" + string(rune('0'+f.next))
		note := map[string]any{"id": id, "path": path, "updatedAt": "2026-10-25T10:00:0" + string(rune('0'+f.next)) + "Z"}
		f.notes[id], f.paths[path] = note, id
		_ = json.NewEncoder(w).Encode(map[string]any{"note": note})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/vault/test/api/notes/"):
		id := strings.TrimPrefix(r.URL.Path, "/vault/test/api/notes/")
		note, ok := f.notes[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if stamp, ok := body["if_updated_at"].(string); ok && stamp != note["updatedAt"] {
			w.WriteHeader(409)
			return
		}
		note["updatedAt"] = "2026-10-25T11:00:00Z"
		_ = json.NewEncoder(w).Encode(note) // bare object form
	default:
		w.WriteHeader(404)
	}
}

func client(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return &Client{Origin: srv.URL, Vault: "test", Tokens: staticToken(testToken), HTTP: srv.Client()}
}

func sampleSession() Session {
	return Session{SessionID: "sess-1", TakeID: "take-abcdef123", Title: "Episode 42: Isolation/Exactness", Take: 1,
		Started: time.Date(2026, 10, 25, 17, 40, 2, 0, time.Local), DurationSec: 754.4, Participants: []string{"Ana", "Ben"},
		PackageDir: "/Users/host/Movies/StudioLinkData/exports/Episode_42/take_01_x", Files: []string{"manifest.json", "transcript/transcript.srt"}}
}

func sampleDoc() *transcript.Document {
	return &transcript.Document{Session: transcript.SessionMeta{Title: "Episode 42"}, Take: 1,
		Speakers: []transcript.Speaker{{Name: "Ana"}, {Name: "Ben"}},
		Segments: []transcript.Segment{{Start: 0.75, End: 2, Speaker: "Ana", Text: "Welcome."}, {Start: 65, End: 66, Speaker: "Ben", Text: "Thanks."}}}
}

func TestSaveNoteRequestShapeAndAuth(t *testing.T) {
	fv := newFakeVault()
	srv := httptest.NewServer(fv)
	defer srv.Close()
	note := SessionNote(DefaultFolder, sampleSession(), sampleDoc())
	r, err := client(t, srv).SaveNote(context.Background(), note, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "n1" || r.UpdatedAt == "" || r.Path != note.Path {
		t.Fatalf("receipt %+v", r)
	}
	req := fv.requests[0]
	if req.Method != "POST" || req.Path != "/vault/test/api/notes" || req.Auth != "Bearer "+testToken || req.ContentType != "application/json" {
		t.Fatalf("request %+v", req)
	}
	if req.Body["path"] != "Meetings/2026-10-25 1740 Episode 42- Isolation-Exactness" {
		t.Fatalf("path %v", req.Body["path"])
	}
	tags, _ := json.Marshal(req.Body["tags"])
	if string(tags) != `["meeting","podcast","studiolink"]` {
		t.Fatalf("tags %s", tags)
	}
	meta := req.Body["metadata"].(map[string]any)
	for k, v := range map[string]string{"source": "studiolink", "studiolink_session_id": "sess-1", "studiolink_take_id": "take-abcdef123", "take": "1",
		"attendees": "Ana, Ben", "duration_seconds": "754.4", "transcript": "minutes", "export_path": sampleSession().PackageDir} {
		if meta[k] != v {
			t.Errorf("metadata %s = %v, want %s", k, meta[k], v)
		}
	}
	content := req.Body["content"].(string)
	for _, want := range []string{"# Episode 42: Isolation/Exactness — Take 1\n", "**Participants:** [[Ana]], [[Ben]]", "**Duration:** 12:34",
		"file:///Users/host/Movies/StudioLinkData/exports/Episode_42/take_01_x", "## Transcript", "**Ana** (0:00): Welcome.", "**Ben** (1:05): Thanks."} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q:\n%s", want, content)
		}
	}
	if strings.HasPrefix(content, "---") {
		t.Error("vault notes carry no front matter (Minutes convention)")
	}
}

func TestResendUpdatesInPlaceAndNeverOverwritesVaultEdits(t *testing.T) {
	fv := newFakeVault()
	srv := httptest.NewServer(fv)
	defer srv.Close()
	c := client(t, srv)
	note := SessionNote(DefaultFolder, sampleSession(), nil)
	first, err := c.SaveNote(context.Background(), note, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.SaveNote(context.Background(), SessionNote(DefaultFolder, sampleSession(), sampleDoc()), &first)
	if err != nil || second.ID != first.ID {
		t.Fatalf("update: %+v %v", second, err)
	}
	patch := fv.requests[1]
	if patch.Method != "PATCH" || patch.Path != "/vault/test/api/notes/n1" || patch.Body["if_updated_at"] != first.UpdatedAt {
		t.Fatalf("patch %+v", patch)
	}
	// The stale receipt (first) no longer matches: the vault copy was changed.
	if _, err := c.SaveNote(context.Background(), note, &first); !errors.Is(err, ErrEditedInVault) {
		t.Fatalf("stale update must refuse: %v", err)
	}
	// Deleted in the vault: created again.
	delete(fv.notes, "n1")
	delete(fv.paths, note.Path)
	again, err := c.SaveNote(context.Background(), note, &second)
	if err != nil || again.ID == "n1" {
		t.Fatalf("recreate: %+v %v", again, err)
	}
}

func TestPathCollisionKeepsBothNotes(t *testing.T) {
	fv := newFakeVault()
	srv := httptest.NewServer(fv)
	defer srv.Close()
	c := client(t, srv)
	note := SessionNote(DefaultFolder, sampleSession(), nil)
	if _, err := c.SaveNote(context.Background(), note, nil); err != nil {
		t.Fatal(err)
	}
	r, err := c.SaveNote(context.Background(), note, nil)
	if err != nil || r.Path != note.Path+" (take-a)" {
		t.Fatalf("collision: %+v %v", r, err)
	}
}

func TestTokenNeverLoggedOrReturned(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	fv := newFakeVault()
	fv.fail, fv.echoAuth = 500, true // a hostile server echoes the Authorization header
	srv := httptest.NewServer(fv)
	defer srv.Close()
	_, err := client(t, srv).SaveNote(context.Background(), SessionNote(DefaultFolder, sampleSession(), nil), nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	slog.Error("parachute send failed", "err", err)
	for _, s := range []string{err.Error(), logs.String()} {
		if strings.Contains(s, testToken) || strings.Contains(s, "c2lnbmF0dXJl") {
			t.Fatalf("token leaked: %s", s)
		}
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("expected scrubbed server message: %v", err)
	}
	fv.fail, fv.echoAuth = 401, false
	_, err = client(t, srv).SaveNote(context.Background(), SessionNote(DefaultFolder, sampleSession(), nil), nil)
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "refused the credential") {
		t.Fatalf("401: %v", err)
	}
}

func TestOriginAndVaultValidation(t *testing.T) {
	for _, ok := range []string{"https://hub.example.com", "https://hub.example.com/", "http://127.0.0.1:1940", "http://localhost:1940"} {
		if _, err := ValidateOrigin(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://hub.example.com", "https://u:p@hub.example.com", "https://hub.example.com/vault/x", "ftp://h", "", "hub.example.com"} {
		if _, err := ValidateOrigin(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if ValidVault("../x") || ValidVault("") || !ValidVault("default") || !ValidVault("my_vault-2") {
		t.Error("vault name validation")
	}
	c := &Client{Origin: "https://hub.example.com", Vault: "a/b", Tokens: staticToken(testToken)}
	if _, err := c.SaveNote(context.Background(), Note{}, nil); err == nil {
		t.Error("invalid vault must be refused before any request")
	}
}

type fakeRunner struct {
	program string
	args    []string
	env     []string
	out     string
	err     error
}

func (f *fakeRunner) Run(_ context.Context, program string, args []string, env []string) ([]byte, []byte, error) {
	f.program, f.args, f.env = program, args, env
	return []byte(f.out), []byte("stderr may echo " + testToken), f.err
}

func TestCLITokenSourceArgvAndErrors(t *testing.T) {
	t.Setenv("STUDIOLINK_SECRET_KEY", "daemon-secret-0123456789abcdef0123")
	r := &fakeRunner{out: "Minted.\n" + testToken + "\n"}
	src := CLITokenSource{Path: "/Users/u/.bun/bin/parachute", Vault: "test", Runner: r}
	tok, err := src.Token(context.Background())
	if err != nil || tok != testToken {
		t.Fatalf("token %q %v", tok, err)
	}
	want := []string{"auth", "mint-token", "--scope", "vault:test:write", "--ephemeral", "--label", "studiolink"}
	if r.program != "/Users/u/.bun/bin/parachute" || !reflect.DeepEqual(r.args, want) {
		t.Fatalf("argv %s %q", r.program, r.args)
	}
	var path string
	for _, kv := range r.env {
		if strings.Contains(kv, "daemon-secret") {
			t.Fatal("daemon secret passed to the CLI")
		}
		if strings.HasPrefix(kv, "PATH=") {
			path = kv
		}
	}
	if !strings.HasPrefix(path, "PATH=/Users/u/.bun/bin") {
		t.Fatalf("CLI dir must lead PATH (bun shebang): %s", path)
	}
	r.err = errors.New("exit status 1")
	if _, err := src.Token(context.Background()); err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("failure must not surface stderr: %v", err)
	}
	r.err, r.out = nil, "not a token"
	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("garbage accepted as token")
	}
}

func TestFileTokenSourceRequiresPrivateFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(testToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileTokenSource{p}).Token(context.Background()); err == nil {
		t.Fatal("world-readable token file accepted")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := FileTokenSource{p}.Token(context.Background())
	if err != nil || tok != testToken {
		t.Fatalf("%q %v", tok, err)
	}
	link := filepath.Join(dir, "link")
	_ = os.Symlink(p, link)
	if _, err := (FileTokenSource{link}).Token(context.Background()); err == nil {
		t.Fatal("symlinked token file accepted")
	}
}

func TestDiscoverCLI(t *testing.T) {
	present := map[string]bool{"/Users/u/.bun/bin/parachute": true}
	isExec := func(p string) bool { return present[p] }
	noPath := func(string) (string, error) { return "", errors.New("none") }
	if p, err := DiscoverCLI("", noPath, isExec, "/Users/u"); err != nil || p != "/Users/u/.bun/bin/parachute" {
		t.Fatalf("%s %v", p, err)
	}
	if _, err := DiscoverCLI("/nope/parachute", noPath, isExec, "/Users/u"); err == nil {
		t.Fatal("missing configured path must fail")
	}
	if _, err := DiscoverCLI("", noPath, func(string) bool { return false }, ""); err == nil {
		t.Fatal("nothing installed")
	}
}

func TestNotePathSanitizesAndSuffixesTakes(t *testing.T) {
	s := sampleSession()
	s.Title, s.Take = " a/b\\c:d\ne ", 3
	if got := NotePath("/Shows/../Pod/", s); got != "Shows/Pod/2026-10-25 1740 a-b-c-d-e (Take 3)" {
		t.Fatalf("path %q", got)
	}
	s.Title = ""
	if got := NotePath("", s); got != "2026-10-25 1740 p2p.video session (Take 3)" {
		t.Fatalf("empty title %q", got)
	}
}
