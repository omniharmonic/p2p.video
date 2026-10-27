package httpserver

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

// recordFixture is a daemon with the record-link routes, an upload handler
// and (when ffmpeg is installed) the export pipeline.
type recordFixture struct {
	t   *testing.T
	st  *store.Store
	cfg *config.Config
	srv *Server
	ts  *httptest.Server
}

func newRecordFixture(t *testing.T) *recordFixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{DataDir: root, PublicURL: "https://studio.tail1234.ts.net", SecretKey: []byte("record-link-test-secret-0123456789"), MaxChunkSizeMB: 10}
	hub := signaling.NewHub(st, cfg, nil)
	t.Cleanup(hub.Close)
	ff, _ := ffmpeg.NewRunner()
	srv := &Server{Config: cfg, Store: st, Hub: hub, DesktopToken: testDesktopToken,
		Upload: &upload.Handler{Store: st, Config: cfg}, Pipeline: postproc.NewPipeline(st, cfg, ff)}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &recordFixture{t: t, st: st, cfg: cfg, srv: srv, ts: ts}
}

// call does a request with headers and returns status + body.
func (f *recordFixture) call(method, path string, body any, headers map[string]string) (int, []byte) {
	f.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rd)
	if err != nil {
		f.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (f *recordFixture) admin(method, path string, body any) (int, []byte) {
	return f.call(method, path, body, map[string]string{"Authorization": "Bearer " + testDesktopToken, "Content-Type": "application/json"})
}

func (f *recordFixture) createLink(body map[string]any) (RecordLinkView, string) {
	f.t.Helper()
	code, raw := f.admin("POST", "/api/v1/recordlinks", body)
	if code != http.StatusCreated {
		f.t.Fatalf("create link: %d %s", code, raw)
	}
	var v RecordLinkView
	if err := json.Unmarshal(raw, &v); err != nil {
		f.t.Fatal(err)
	}
	u, err := url.Parse(v.URL)
	if err != nil || u.Path != "/record" || u.Query().Get("link") != v.ID {
		f.t.Fatalf("link URL %q", v.URL)
	}
	return v, u.Query().Get("token")
}

func guestHeaders(secret, key string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + secret, "Content-Type": "application/json"}
	if key != "" {
		h[submissionKeyHeader] = key
	}
	return h
}

type startedSubmission struct {
	Key        string                `json:"key"`
	Submission RecordGuestSubmission `json:"submission"`
}

func (f *recordFixture) startSubmission(linkID, secret, name string) startedSubmission {
	f.t.Helper()
	code, raw := f.call("POST", "/api/v1/record/"+linkID+"/submissions", map[string]string{"name": name}, guestHeaders(secret, ""))
	if code != http.StatusCreated {
		f.t.Fatalf("start submission: %d %s", code, raw)
	}
	var s startedSubmission
	if err := json.Unmarshal(raw, &s); err != nil {
		f.t.Fatal(err)
	}
	if len(s.Key) != 64 || s.Submission.UploadToken == "" || s.Submission.Status != store.SubmissionDraft {
		f.t.Fatalf("submission %+v", s)
	}
	return s
}

func (f *recordFixture) subPath(linkID, sid, rest string) string {
	return "/api/v1/record/" + linkID + "/submissions/" + sid + rest
}

func (f *recordFixture) startTake(linkID, secret string, s startedSubmission) RecordTakeStart {
	f.t.Helper()
	code, raw := f.call("POST", f.subPath(linkID, s.Submission.ID, "/takes"), nil, guestHeaders(secret, s.Key))
	if code != http.StatusCreated {
		f.t.Fatalf("start take: %d %s", code, raw)
	}
	var tk RecordTakeStart
	_ = json.Unmarshal(raw, &tk)
	return tk
}

// uploadTrack registers a track in the take and uploads data as one chunk;
// complete=true finalizes it with a sidecar.
func (f *recordFixture) uploadTrack(takeID, grant, kind, mime string, data []byte, complete bool) (string, int) {
	f.t.Helper()
	auth := map[string]string{"Authorization": "Bearer " + grant, "Content-Type": "application/json"}
	code, raw := f.call("POST", "/api/v1/takes/"+takeID+"/tracks", map[string]string{"kind": kind, "mime": mime, "label": "Camera + microphone"}, auth)
	if code != http.StatusCreated {
		return "", code
	}
	var tr store.Track
	_ = json.Unmarshal(raw, &tr)
	sum := sha256.Sum256(data)
	code, raw = f.call("PUT", fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/0", takeID, tr.ID), data,
		map[string]string{"Authorization": "Bearer " + grant, "X-Chunk-SHA256": hex.EncodeToString(sum[:])})
	if code != http.StatusCreated {
		f.t.Logf("chunk: %d %s", code, raw)
		return tr.ID, code
	}
	if complete {
		body := upload.CompleteRequest{LastSeq: 0, TotalBytes: int64(len(data)), Sidecar: map[string]any{
			"trackId": tr.ID, "kind": kind, "clockFit": map[string]any{"a": 0.0, "b": 1.0, "rttP10Ms": 1.0},
			"events": []map[string]any{{"type": "recorder-start", "perfTime": 0, "mediaTimeUs": 0}, {"type": "recorder-stop", "perfTime": 1000, "mediaTimeUs": 0}},
		}}
		code, raw = f.call("POST", fmt.Sprintf("/api/v1/takes/%s/tracks/%s/complete", takeID, tr.ID), body, auth)
		if code != http.StatusOK {
			f.t.Fatalf("complete: %d %s", code, raw)
		}
	}
	return tr.ID, http.StatusCreated
}

// The whole guest flow, the caps and every refusal [SL-P5-3].
func TestRecordLinkGuestFlowAndSecurity(t *testing.T) {
	f := newRecordFixture(t)
	link, secret := f.createLink(map[string]any{"title": "Guest intro", "prompt": "# Intro\nSay who you are.", "maxSubmissions": 2})
	if link.MaxDurationSec != 1800 || link.MaxTotalBytes != 20<<30 || !link.LinkEnabled || link.Expired || len(secret) != 64 {
		t.Fatalf("defaults: %+v", link.RecordLink)
	}
	// The secret is not stored.
	stored, _ := f.st.GetRecordLink(link.ID)
	if stored.SecretHash == secret || stored.SecretHash != hashToken(secret) || len(stored.Nonce) != 32 {
		t.Fatal("record link secret stored in the clear or weak")
	}

	// Link info: the right secret only; every other failure is the same 401.
	code, raw := f.call("GET", "/api/v1/record/"+link.ID, nil, guestHeaders(secret, ""))
	var info RecordLinkPublic
	_ = json.Unmarshal(raw, &info)
	if code != 200 || info.Title != "Guest intro" || info.Prompt != "# Intro\nSay who you are." || !info.Accepting {
		t.Fatalf("info: %d %s", code, raw)
	}
	for _, tc := range []struct{ id, secret string }{{link.ID, ""}, {link.ID, strings.Repeat("0", 64)}, {"rl_nope", secret}} {
		code, raw := f.call("GET", "/api/v1/record/"+tc.id, nil, guestHeaders(tc.secret, ""))
		if code != http.StatusUnauthorized || !strings.Contains(string(raw), "isn’t valid") {
			t.Fatalf("wrong link %+v: %d %s", tc, code, raw)
		}
	}

	a := f.startSubmission(link.ID, secret, "Ana\u202e")
	b := f.startSubmission(link.ID, secret, "Ben")
	if a.Submission.Name != "Ana" {
		t.Fatalf("name not sanitized: %q", a.Submission.Name)
	}
	// A guest can't see or touch another guest's submission, or any without its key.
	for _, key := range []string{"", b.Key, strings.Repeat("a", 64)} {
		if code, _ := f.call("GET", f.subPath(link.ID, a.Submission.ID, ""), nil, guestHeaders(secret, key)); code != http.StatusNotFound {
			t.Fatalf("submission readable with key %q: %d", key, code)
		}
		if code, _ := f.call("POST", f.subPath(link.ID, a.Submission.ID, "/takes"), nil, guestHeaders(secret, key)); code != http.StatusNotFound {
			t.Fatalf("take started with key %q: %d", key, code)
		}
	}
	// B's upload grant does not reach A's takes.
	tkA := f.startTake(link.ID, secret, a)
	if _, code := f.uploadTrack(tkA.TakeID, b.Submission.UploadToken, "video", "video/webm", []byte("x"), false); code != http.StatusForbidden {
		t.Fatalf("other guest's grant registered a track: %d", code)
	}

	// Clock pings answer with host time.
	code, raw = f.call("POST", f.subPath(link.ID, a.Submission.ID, "/clock"), map[string]int64{"t0": 42}, guestHeaders(secret, a.Key))
	var pong signaling.ClockPongPayload
	_ = json.Unmarshal(raw, &pong)
	if code != 200 || pong.T0 != 42 || pong.T1 <= 0 || pong.T2 < pong.T1 {
		t.Fatalf("clock: %d %s", code, raw)
	}

	// One take at a time; record, re-record (discard), record again.
	if code, _ := f.call("POST", f.subPath(link.ID, a.Submission.ID, "/takes"), nil, guestHeaders(secret, a.Key)); code != http.StatusConflict {
		t.Fatalf("second concurrent take: %d", code)
	}
	if _, code := f.uploadTrack(tkA.TakeID, a.Submission.UploadToken, "video", "video/webm", []byte("first-take"), true); code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	if code, _ := f.call("POST", f.subPath(link.ID, a.Submission.ID, "/takes/"+tkA.TakeID+"/stop"), nil, guestHeaders(secret, a.Key)); code != 200 {
		t.Fatalf("stop: %d", code)
	}
	takeDir := filepath.Join(f.cfg.DataDir, "sessions", a.Submission.ID, "takes", tkA.TakeID)
	if _, err := os.Stat(takeDir); err != nil {
		t.Fatalf("take files missing: %v", err)
	}
	if code, _ := f.call("DELETE", f.subPath(link.ID, a.Submission.ID, "/takes/"+tkA.TakeID), nil, guestHeaders(secret, a.Key)); code != http.StatusNoContent {
		t.Fatalf("discard: %d", code)
	}
	if _, err := os.Stat(takeDir); !os.IsNotExist(err) {
		t.Fatal("discarded take's files remain")
	}
	tk2 := f.startTake(link.ID, secret, a)
	if tk2.Number != 2 || tk2.TakeID == tkA.TakeID {
		t.Fatalf("take numbers reused: %+v", tk2)
	}
	if _, code := f.uploadTrack(tk2.TakeID, a.Submission.UploadToken, "video", "video/webm", []byte("second-take"), true); code != http.StatusCreated {
		t.Fatalf("upload 2: %d", code)
	}
	// Can't submit while recording.
	if code, _ := f.call("POST", f.subPath(link.ID, a.Submission.ID, "/submit"), nil, guestHeaders(secret, a.Key)); code != http.StatusConflict {
		t.Fatalf("submit while recording: %d", code)
	}
	f.call("POST", f.subPath(link.ID, a.Submission.ID, "/takes/"+tk2.TakeID+"/stop"), nil, guestHeaders(secret, a.Key))
	code, raw = f.call("POST", f.subPath(link.ID, a.Submission.ID, "/submit"), nil, guestHeaders(secret, a.Key))
	var submitted RecordGuestSubmission
	_ = json.Unmarshal(raw, &submitted)
	if code != 200 || submitted.Status != store.SubmissionSubmitted || len(submitted.Takes) != 1 || submitted.Takes[0].Tracks[0].Status != "assembled" {
		t.Fatalf("submit: %d %s", code, raw)
	}
	// A submitted recording is frozen.
	if code, _ := f.call("POST", f.subPath(link.ID, a.Submission.ID, "/takes"), nil, guestHeaders(secret, a.Key)); code != http.StatusConflict {
		t.Fatalf("take after submit: %d", code)
	}
	if code, _ := f.call("DELETE", f.subPath(link.ID, a.Submission.ID, "/takes/"+tk2.TakeID), nil, guestHeaders(secret, a.Key)); code != http.StatusConflict {
		t.Fatalf("discard after submit: %d", code)
	}

	// The submission is a recording, never a call: no WebSocket with the link
	// secret, the grant, or even a host token.
	wsURL := "ws" + strings.TrimPrefix(f.ts.URL, "http") + "/api/v1/ws"
	for _, tok := range []string{secret, a.Submission.UploadToken, signaling.MintHostToken(a.Submission.ID, time.Hour, f.cfg.SecretKey)} {
		if c, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s", wsURL, a.Submission.ID, url.QueryEscape(tok)), nil); err == nil {
			c.Close()
			t.Fatal("submission session accepted a WebSocket")
		} else if resp == nil || (resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized) {
			t.Fatalf("submission WS: %v", err)
		}
	}
	// And the link secret opens no meeting either.
	resp, _ := adminPost(f.ts.URL+"/api/v1/sessions", `{"title":"Call"}`)
	var meeting CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&meeting)
	if c, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s", wsURL, meeting.ID, secret), nil); err == nil {
		c.Close()
		t.Fatal("record link secret joined a call")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("meeting WS with link secret: %v", err)
	}
	// Nor a host route.
	if code, _ := f.call("GET", "/api/v1/recordlinks", nil, map[string]string{"Authorization": "Bearer " + secret}); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("link secret listed record links: %d", code)
	}

	// Submission cap: b submits (2 of 2), then no more submissions or new drafts.
	tkB := f.startTake(link.ID, secret, b)
	f.uploadTrack(tkB.TakeID, b.Submission.UploadToken, "video", "video/webm", []byte("b"), true)
	f.call("POST", f.subPath(link.ID, b.Submission.ID, "/takes/"+tkB.TakeID+"/stop"), nil, guestHeaders(secret, b.Key))
	if code, raw := f.call("POST", f.subPath(link.ID, b.Submission.ID, "/submit"), nil, guestHeaders(secret, b.Key)); code != 200 {
		t.Fatalf("b submit: %d %s", code, raw)
	}
	if code, _ := f.call("POST", "/api/v1/record/"+link.ID+"/submissions", map[string]string{"name": "Cy"}, guestHeaders(secret, "")); code != http.StatusConflict {
		t.Fatalf("submission past the cap: %d", code)
	}
	_, raw = f.call("GET", "/api/v1/record/"+link.ID, nil, guestHeaders(secret, ""))
	_ = json.Unmarshal(raw, &info)
	if info.Accepting || info.Reason != "full" {
		t.Fatalf("full link still accepting: %s", raw)
	}

	// The host sees both submissions, with host tokens that work on the
	// ordinary take routes; drafts are counted but carry no token.
	code, raw = f.admin("GET", "/api/v1/recordlinks/"+link.ID, nil)
	var hv RecordLinkView
	_ = json.Unmarshal(raw, &hv)
	if code != 200 || hv.Submitted != 2 || len(hv.Submissions) != 2 || hv.UsedBytes == 0 {
		t.Fatalf("host view: %d %s", code, raw)
	}
	for _, sv := range hv.Submissions {
		if sv.HostToken == "" || len(sv.Takes) != 1 || !sv.Takes[0].OriginalsReady {
			t.Fatalf("submission view %+v", sv)
		}
		if code, _ := f.call("GET", "/api/v1/takes/"+sv.Takes[0].ID, nil, map[string]string{"Authorization": "Bearer " + sv.HostToken}); code != 200 {
			t.Fatalf("take detail with submission host token: %d", code)
		}
	}
	if strings.Contains(string(raw), stored.SecretHash) || strings.Contains(string(raw), a.Key) {
		t.Fatal("host view leaked a hash or submission key")
	}
	// Submissions are not "recent meetings".
	_, raw = f.admin("GET", "/api/v1/home", nil)
	if strings.Contains(string(raw), a.Submission.ID) {
		t.Fatal("submission listed as a meeting")
	}

	// Deleting a submission removes its files and its grant stops working.
	if code, _ := f.admin("DELETE", "/api/v1/recordlinks/"+link.ID+"/submissions/"+b.Submission.ID, nil); code != http.StatusNoContent {
		t.Fatalf("delete submission: %d", code)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.DataDir, "sessions", b.Submission.ID)); !os.IsNotExist(err) {
		t.Fatal("deleted submission's files remain")
	}
	if code, _ := f.call("GET", "/api/v1/takes/"+tkB.TakeID+"/tracks/x/status", nil, map[string]string{"Authorization": "Bearer " + b.Submission.UploadToken}); code == 200 {
		t.Fatal("deleted submission's grant still works")
	}
	if code, _ := f.admin("DELETE", "/api/v1/recordlinks/"+link.ID, nil); code != http.StatusConflict {
		t.Fatalf("link with submissions deleted: %d", code)
	}
}

// Rotation, revocation and expiry refuse the old link at once, for every
// guest call; uploads of takes already recorded keep their grant [SL-P5-3].
func TestRecordLinkRotateRevokeExpire(t *testing.T) {
	f := newRecordFixture(t)
	link, secret := f.createLink(map[string]any{"title": "Promo", "expiresInHours": 1})
	a := f.startSubmission(link.ID, secret, "Ana")
	tk := f.startTake(link.ID, secret, a)

	code, raw := f.admin("POST", "/api/v1/recordlinks/"+link.ID+"/rotate", nil)
	var rotated RecordLinkView
	_ = json.Unmarshal(raw, &rotated)
	newSecret := ""
	if u, err := url.Parse(rotated.URL); err == nil {
		newSecret = u.Query().Get("token")
	}
	if code != 200 || newSecret == "" || newSecret == secret {
		t.Fatalf("rotate: %d %s", code, raw)
	}
	for _, p := range []string{"", "/submissions/" + a.Submission.ID, "/submissions/" + a.Submission.ID + "/takes/" + tk.TakeID + "/stop"} {
		method := "GET"
		if strings.HasSuffix(p, "/stop") {
			method = "POST"
		}
		if code, _ := f.call(method, "/api/v1/record/"+link.ID+p, nil, guestHeaders(secret, a.Key)); code != http.StatusUnauthorized {
			t.Fatalf("rotated-away secret on %q: %d", p, code)
		}
	}
	if code, _ := f.call("GET", f.subPath(link.ID, a.Submission.ID, ""), nil, guestHeaders(newSecret, a.Key)); code != 200 {
		t.Fatalf("new secret + key: %d", code)
	}
	// The recorded take still uploads under its grant.
	if _, code := f.uploadTrack(tk.TakeID, a.Submission.UploadToken, "audio", "audio/wav", []byte("RIFF"), false); code != http.StatusCreated {
		t.Fatalf("upload after rotation: %d", code)
	}

	// Expiry: the host can expire a link now; its correct secret then gets 410.
	code, raw = f.admin("PATCH", "/api/v1/recordlinks/"+link.ID, map[string]any{"expiresAt": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)})
	if code != 200 {
		t.Fatalf("expire: %d %s", code, raw)
	}
	for _, p := range []string{"", "/submissions"} {
		method := "GET"
		if p != "" {
			method = "POST"
		}
		code, raw := f.call(method, "/api/v1/record/"+link.ID+p, map[string]string{"name": "Late"}, guestHeaders(newSecret, ""))
		if code != http.StatusGone || !strings.Contains(string(raw), "expired") {
			t.Fatalf("expired link %q: %d %s", p, code, raw)
		}
	}
	// A wrong secret on an expired link learns nothing.
	if code, _ := f.call("GET", "/api/v1/record/"+link.ID, nil, guestHeaders(secret, "")); code != http.StatusUnauthorized {
		t.Fatalf("expired link with old secret: %d", code)
	}
	// A new link can't be created already expired; caps are validated.
	for _, body := range []map[string]any{
		{"title": "x", "expiresAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		{"title": "x", "expiresInHours": 24 * 91},
		{"title": "x", "maxDurationSec": 5},
		{"title": "x", "maxSubmissions": 0},
		{"title": "x", "maxTotalMB": 1},
		{"title": ""},
		{"title": "x", "prompt": strings.Repeat("a", 64*1024+1)},
		{"title": "x", "roomId": "room_missing"},
	} {
		if code, _ := f.admin("POST", "/api/v1/recordlinks", body); code != http.StatusBadRequest {
			t.Fatalf("invalid link %v accepted: %d", body, code)
		}
	}

	// Revocation: 401 for everyone, and the dashboard shows no URL.
	link2, secret2 := f.createLink(map[string]any{"title": "Two"})
	code, raw = f.admin("POST", "/api/v1/recordlinks/"+link2.ID+"/revoke", nil)
	var revoked RecordLinkView
	_ = json.Unmarshal(raw, &revoked)
	if code != 200 || revoked.LinkEnabled || revoked.URL != "" {
		t.Fatalf("revoke: %d %s", code, raw)
	}
	if code, _ := f.call("GET", "/api/v1/record/"+link2.ID, nil, guestHeaders(secret2, "")); code != http.StatusUnauthorized {
		t.Fatalf("revoked link: %d", code)
	}
}

// Server-side caps: the take stops at its limit (so its upload quota is
// fixed), and the link's storage cap refuses chunks past it [SL-P5-3].
func TestRecordLinkDurationAndStorageCaps(t *testing.T) {
	f := newRecordFixture(t)
	link, secret := f.createLink(map[string]any{"title": "Caps"})
	l, _ := f.st.GetRecordLink(link.ID)
	l.MaxDurationSec, l.MaxTotalBytes = 1, 1000 // below the API minimums, for speed
	if err := f.st.UpdateRecordLink(l); err != nil {
		t.Fatal(err)
	}
	a := f.startSubmission(link.ID, secret, "Ana")
	tk := f.startTake(link.ID, secret, a)
	if _, code := f.uploadTrack(tk.TakeID, a.Submission.UploadToken, "video", "video/webm", bytes.Repeat([]byte{1}, 800), false); code != http.StatusCreated {
		t.Fatalf("first chunk: %d", code)
	}
	// Past the link's 1000-byte cap.
	if _, code := f.uploadTrack(tk.TakeID, a.Submission.UploadToken, "audio", "audio/webm", bytes.Repeat([]byte{2}, 300), false); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunk past the link cap: %d", code)
	}
	// The take outlives its 1 s limit (+2 s grace): the server stops it.
	deadline := time.Now().Add(6 * time.Second)
	var got *store.Take
	for time.Now().Before(deadline) {
		got, _ = f.st.GetTake(tk.TakeID)
		if got.Status != "recording" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got.Status != "stopped" {
		t.Fatalf("take not stopped at its limit: %+v", got)
	}
	if d := float64(got.StopAtHostUs-got.StartAtHostUs) / 1e6; d > 3.01 {
		t.Fatalf("stopped take lasts %.2fs, past limit + grace", d)
	}
	// No new tracks in a stopped take.
	if _, code := f.uploadTrack(tk.TakeID, a.Submission.UploadToken, "audio", "audio/webm", []byte{1}, false); code != http.StatusConflict {
		t.Fatalf("track registered in a stopped take: %d", code)
	}
	// A restart loses the timer: the sweep stops overdue takes on the next request.
	// A take left recording by a previous daemon run (an hour ago).
	stale := &store.Take{ID: "take_" + a.Submission.ID + "_99", SessionID: a.Submission.ID, Number: 99, StartAtHostUs: 5_000_000,
		WallClockStart: time.Now().Add(-time.Hour), Status: "recording"}
	if err := f.st.CreateTakeWithParticipants(stale, []string{a.Submission.ParticipantID}); err != nil {
		t.Fatal(err)
	}
	f.call("GET", f.subPath(link.ID, a.Submission.ID, ""), nil, guestHeaders(secret, a.Key))
	if got, _ := f.st.GetTake(stale.ID); got.Status != "stopped" || got.StopAtHostUs != 5_000_000+3_000_000 {
		t.Fatalf("overdue take not swept to its limit: %+v", got)
	}
}

// Drafts open at once are bounded, and guest entry points are rate-limited
// like call joins [SL-P5-3].
func TestRecordLinkDraftCapAndRateLimit(t *testing.T) {
	f := newRecordFixture(t)
	link, secret := f.createLink(map[string]any{"title": "Flood", "maxSubmissions": 100})
	for i := 0; i < recordDraftsOpen; i++ {
		f.startSubmission(link.ID, secret, fmt.Sprintf("G%d", i))
	}
	if code, _ := f.call("POST", "/api/v1/record/"+link.ID+"/submissions", map[string]string{"name": "Six"}, guestHeaders(secret, "")); code != http.StatusTooManyRequests {
		t.Fatalf("draft past the cap: %d", code)
	}
	limited := false
	for i := 0; i < joinAddrBurst+5; i++ {
		if code, _ := f.call("GET", "/api/v1/record/"+link.ID, nil, guestHeaders(strings.Repeat("0", 64), "")); code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("record link guesses are not rate-limited")
	}
}

// Through Funnel, the guest side works and every record-link management
// route is denied, even with the desktop token or a host token [SL-P5-3].
func TestRecordLinkFunnel(t *testing.T) {
	f := newRecordFixture(t)
	link, secret := f.createLink(map[string]any{"title": "Public"})
	funnel := func(r *http.Request) {
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header.Set("Tailscale-Funnel-Request", "?1")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
	}
	do := func(method, path, body string, h map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://studio.tail1234.ts.net"+path, strings.NewReader(body))
		funnel(r)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/api/v1/record/"+link.ID, "", guestHeaders(secret, "")); w.Code != 200 {
		t.Fatalf("guest info through Funnel: %d %s", w.Code, w.Body)
	}
	w := do("POST", "/api/v1/record/"+link.ID+"/submissions", `{"name":"Ana"}`, guestHeaders(secret, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("guest submission through Funnel: %d %s", w.Code, w.Body)
	}
	var s startedSubmission
	_ = json.Unmarshal(w.Body.Bytes(), &s)
	host := signaling.MintHostToken(s.Submission.ID, time.Hour, f.cfg.SecretKey)
	for _, tok := range []string{testDesktopToken, host} {
		for _, tc := range []struct{ method, path, body string }{
			{"GET", "/api/v1/recordlinks", ""},
			{"POST", "/api/v1/recordlinks", `{"title":"x"}`},
			{"GET", "/api/v1/recordlinks/" + link.ID, ""},
			{"PATCH", "/api/v1/recordlinks/" + link.ID, `{"title":"y"}`},
			{"DELETE", "/api/v1/recordlinks/" + link.ID, ""},
			{"POST", "/api/v1/recordlinks/" + link.ID + "/rotate", ""},
			{"POST", "/api/v1/recordlinks/" + link.ID + "/revoke", ""},
			{"DELETE", "/api/v1/recordlinks/" + link.ID + "/submissions/" + s.Submission.ID, ""},
		} {
			if w := do(tc.method, tc.path, tc.body, map[string]string{"Authorization": "Bearer " + tok}); w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s through Funnel = %d", tc.method, tc.path, w.Code)
			}
		}
	}
}

// A submission exports like a take: editor package (decodes), originals ZIP
// [SL-P5-3]. Needs ffmpeg.
func TestRecordSubmissionExports(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	f := newRecordFixture(t)
	if f.srv.Pipeline.FFmpeg == nil {
		t.Skip("ffmpeg runner unavailable")
	}
	link, secret := f.createLink(map[string]any{"title": "Export me"})
	a := f.startSubmission(link.ID, secret, "Ana")
	tk := f.startTake(link.ID, secret, a)
	media := filepath.Join(t.TempDir(), "take.webm")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "libvpx", "-c:a", "libopus", "-shortest", media).CombinedOutput(); err != nil {
		t.Fatalf("make media: %v %s", err, out)
	}
	data, _ := os.ReadFile(media)
	f.uploadTrack(tk.TakeID, a.Submission.UploadToken, "video", "video/webm", data, true)
	f.call("POST", f.subPath(link.ID, a.Submission.ID, "/takes/"+tk.TakeID+"/stop"), nil, guestHeaders(secret, a.Key))
	if code, raw := f.call("POST", f.subPath(link.ID, a.Submission.ID, "/submit"), nil, guestHeaders(secret, a.Key)); code != 200 {
		t.Fatalf("submit: %d %s", code, raw)
	}
	_, raw := f.admin("GET", "/api/v1/recordlinks/"+link.ID, nil)
	var hv RecordLinkView
	_ = json.Unmarshal(raw, &hv)
	token := hv.Submissions[0].HostToken
	auth := map[string]string{"Authorization": "Bearer " + token}
	if code, raw := f.call("POST", "/api/v1/takes/"+tk.TakeID+"/postprocess", nil, auth); code != 200 {
		t.Fatalf("export: %d %s", code, raw)
	}
	for _, p := range []string{"/download", "/originals"} {
		code, body := f.call("GET", "/api/v1/takes/"+tk.TakeID+p, nil, auth)
		if code != 200 {
			t.Fatalf("%s: %d %s", p, code, body)
		}
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil || len(zr.File) == 0 {
			t.Fatalf("%s is not a ZIP: %v", p, err)
		}
		if p == "/download" {
			var manifest bool
			for _, zf := range zr.File {
				manifest = manifest || strings.HasSuffix(zf.Name, "manifest.json")
			}
			if !manifest {
				t.Fatal("editor package without manifest.json")
			}
		}
	}
	// The editor video decodes.
	out := filepath.Join(f.cfg.DataDir, "sessions", a.Submission.ID, "takes", tk.TakeID, "out", "video")
	entries, _ := os.ReadDir(out)
	if len(entries) == 0 {
		t.Fatal("no conformed video")
	}
	if b, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", filepath.Join(out, entries[0].Name())).CombinedOutput(); err != nil || !strings.Contains(string(b), "video") {
		t.Fatalf("conformed video does not decode: %v %s", err, b)
	}
}
