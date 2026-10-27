package httpserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// backdate moves every activity timestamp of a submission (creation, take
// starts, chunk uploads) ago into the past, through a second connection to
// the daemon's database: what a draft looks like after that much idle time.
func (f *recordFixture) backdate(sessionID string, ago time.Duration) {
	f.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(f.cfg.DataDir, "db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	at := time.Now().Add(-ago).UTC().Format(time.RFC3339Nano)
	for _, q := range []string{
		`UPDATE record_submissions SET created_at = ? WHERE session_id = ?`,
		`UPDATE takes SET wall_clock_start = ? WHERE session_id = ?`,
		`UPDATE chunks SET received_at = ? WHERE track_id IN (SELECT t.id FROM tracks t JOIN takes k ON k.id = t.take_id WHERE k.session_id = ?)`,
	} {
		if _, err := db.Exec(q, at, sessionID); err != nil {
			f.t.Fatal(q, err)
		}
	}
}

// draftWithMedia starts a draft, records one stopped take and uploads a
// chunk, so the draft has rows and files.
func (f *recordFixture) draftWithMedia(linkID, secret, name string) startedSubmission {
	f.t.Helper()
	s := f.startSubmission(linkID, secret, name)
	tk := f.startTake(linkID, secret, s)
	// Tracks register while the take records (the per-take quota covers
	// 4 KB even for a zero-length take).
	if _, code := f.uploadTrack(tk.TakeID, s.Submission.UploadToken, "video", "video/webm", []byte(strings.Repeat("v", 4096)), false); code != http.StatusCreated {
		f.t.Fatalf("upload: %d", code)
	}
	if code, raw := f.call("POST", f.subPath(linkID, s.Submission.ID, "/takes/"+tk.TakeID+"/stop"), nil, guestHeaders(secret, s.Key)); code != 200 {
		f.t.Fatalf("stop: %d %s", code, raw)
	}
	return s
}

func (f *recordFixture) drafts() ([]DraftView, map[string]any) {
	f.t.Helper()
	code, raw := f.admin("GET", "/api/v1/recordlinks/drafts", nil)
	if code != 200 {
		f.t.Fatalf("drafts: %d %s", code, raw)
	}
	var out struct {
		Drafts []DraftView    `json:"drafts"`
		Policy map[string]any `json:"policy"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Drafts, out.Policy
}

func (f *recordFixture) sessionDir(id string) string {
	return filepath.Join(f.cfg.DataDir, "sessions", id)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Abandoned drafts are listed with sizes and deleted by hand (one or all)
// with their files; recent activity, a take recording and submitted
// recordings are never touched [SL-P5-4].
func TestAbandonedDraftsListDeleteAndCleanup(t *testing.T) {
	f := newRecordFixture(t)
	link, secret := f.createLink(map[string]any{"title": "Intro", "maxSubmissions": 5})
	sent := f.draftWithMedia(link.ID, secret, "Sent")
	if code, raw := f.call("POST", f.subPath(link.ID, sent.Submission.ID, "/submit"), nil, guestHeaders(secret, sent.Key)); code != 200 {
		t.Fatalf("submit: %d %s", code, raw)
	}
	old := f.draftWithMedia(link.ID, secret, "Old")
	empty := f.startSubmission(link.ID, secret, "Empty")
	recent := f.draftWithMedia(link.ID, secret, "Recent")
	live := f.startSubmission(link.ID, secret, "Live")
	f.backdate(live.Submission.ID, 48*time.Hour)
	f.startTake(link.ID, secret, live) // opened two days ago, recording now
	// Old and Empty were last touched two days ago; Recent uploaded just
	// now. The sent recording is old too, but submitted.
	for _, id := range []string{old.Submission.ID, empty.Submission.ID, sent.Submission.ID} {
		f.backdate(id, 48*time.Hour)
	}
	if !exists(f.sessionDir(old.Submission.ID)) || !exists(f.sessionDir(sent.Submission.ID)) {
		t.Fatal("fixture has no files")
	}
	drafts, policy := f.drafts()
	if policy["uploadGuardMinutes"] != 60.0 || policy["abandonedAfterHours"] != 24.0 || policy["autoCleanup"] != false {
		t.Fatalf("policy %+v", policy)
	}
	byName := map[string]DraftView{}
	for _, d := range drafts {
		byName[d.Name] = d
	}
	if len(drafts) != 4 || byName["Sent"].SessionID != "" {
		t.Fatalf("drafts %+v", drafts)
	}
	if d := byName["Old"]; !d.Abandoned || d.Reason != "idle" || d.Bytes != 4096 || d.Takes != 1 || d.LinkTitle != "Intro" || d.LastUpload.IsZero() {
		t.Fatalf("old %+v", d)
	}
	if d := byName["Empty"]; !d.Abandoned || d.Bytes != 0 {
		t.Fatalf("empty %+v", d)
	}
	if byName["Recent"].Abandoned || byName["Live"].Abandoned || !byName["Live"].Recording {
		t.Fatalf("in-progress drafts listed as abandoned: %+v %+v", byName["Recent"], byName["Live"])
	}
	// The link view counts abandoned drafts apart.
	_, raw := f.admin("GET", "/api/v1/recordlinks/"+link.ID, nil)
	var lv RecordLinkView
	_ = json.Unmarshal(raw, &lv)
	if lv.Drafts != 4 || lv.AbandonedDrafts != 2 {
		t.Fatalf("link drafts %d abandoned %d", lv.Drafts, lv.AbandonedDrafts)
	}

	// Deleting a draft in progress is refused; unknown and submitted are 404.
	for id, want := range map[string]int{recent.Submission.ID: http.StatusConflict, live.Submission.ID: http.StatusConflict, sent.Submission.ID: http.StatusNotFound, "nope": http.StatusNotFound} {
		if code, raw := f.admin("DELETE", "/api/v1/recordlinks/drafts/"+id, nil); code != want {
			t.Errorf("delete %s = %d %s, want %d", id, code, raw, want)
		}
	}
	// One by hand: rows and files go; the guest's key stops working.
	if code, raw := f.admin("DELETE", "/api/v1/recordlinks/drafts/"+old.Submission.ID, nil); code != http.StatusNoContent {
		t.Fatalf("delete old: %d %s", code, raw)
	}
	if exists(f.sessionDir(old.Submission.ID)) {
		t.Fatal("old draft's files survive")
	}
	if _, err := f.st.GetSession(old.Submission.ID); err == nil {
		t.Fatal("old draft's session survives")
	}
	if code, _ := f.call("GET", f.subPath(link.ID, old.Submission.ID, ""), nil, guestHeaders(secret, old.Key)); code != http.StatusNotFound {
		t.Fatalf("deleted draft still readable: %d", code)
	}

	// Activity after the list was read wins: a fresh upload keeps a draft
	// even if it is named in a cleanup.
	f.backdate(recent.Submission.ID, 48*time.Hour)
	tk2 := f.startTake(link.ID, secret, recent)
	f.call("POST", f.subPath(link.ID, recent.Submission.ID, "/takes/"+tk2.TakeID+"/stop"), nil, guestHeaders(secret, recent.Key))
	code, raw := f.admin("POST", "/api/v1/recordlinks/drafts/cleanup", map[string]any{"ids": []string{recent.Submission.ID, empty.Submission.ID}})
	var res struct {
		Deleted, Skipped []string
		Bytes            int64
	}
	_ = json.Unmarshal(raw, &res)
	if code != 200 || len(res.Deleted) != 1 || res.Deleted[0] != empty.Submission.ID || len(res.Skipped) != 1 || res.Skipped[0] != recent.Submission.ID {
		t.Fatalf("targeted cleanup %d %s", code, raw)
	}
	// Clean up everything abandoned: nothing left but in-progress drafts.
	if code, raw := f.admin("POST", "/api/v1/recordlinks/drafts/cleanup", nil); code != 200 {
		t.Fatalf("cleanup all %d %s", code, raw)
	}
	drafts, _ = f.drafts()
	if len(drafts) != 2 {
		t.Fatalf("after cleanup %+v", drafts)
	}
	// The submitted recording is untouched.
	if sub, err := f.st.GetSubmission(sent.Submission.ID); err != nil || sub.Status != store.SubmissionSubmitted || !exists(f.sessionDir(sent.Submission.ID)) {
		t.Fatalf("submitted recording touched: %v %+v", err, sub)
	}
	if takes, _ := f.st.ListTakesBySession(sent.Submission.ID); len(takes) != 1 {
		t.Fatalf("submitted takes %d", len(takes))
	}
}

// Automatic cleanup (daemon start and daily): drafts idle past the expiry,
// and drafts on an expired or turned-off link idle past its grace, are
// deleted; recent and recording drafts and submitted recordings stay; 0
// days turns it off [SL-P5-4].
func TestDraftAutoExpiry(t *testing.T) {
	f := newRecordFixture(t)
	f.cfg.RecordDraftExpiryDays, f.cfg.RecordDraftClosedLinkHours, f.cfg.RecordDraftUploadGuardMinutes = 14, 24, 60
	open, openSecret := f.createLink(map[string]any{"title": "Open", "maxSubmissions": 5})
	closing, closingSecret := f.createLink(map[string]any{"title": "Closing", "maxSubmissions": 5})
	stale := f.draftWithMedia(open.ID, openSecret, "Stale")             // 15 days idle: expired
	week := f.draftWithMedia(open.ID, openSecret, "Week")               // 7 days idle: kept (abandoned)
	sent := f.draftWithMedia(open.ID, openSecret, "Sent")               // submitted, 30 days old
	onClosed := f.draftWithMedia(closing.ID, closingSecret, "OnClosed") // 2 days idle on a revoked link
	freshClosed := f.draftWithMedia(closing.ID, closingSecret, "Fresh") // 2 hours idle on a revoked link
	f.call("POST", f.subPath(open.ID, sent.Submission.ID, "/submit"), nil, guestHeaders(openSecret, sent.Key))
	f.backdate(stale.Submission.ID, 15*24*time.Hour)
	f.backdate(week.Submission.ID, 7*24*time.Hour)
	f.backdate(sent.Submission.ID, 30*24*time.Hour)
	f.backdate(onClosed.Submission.ID, 48*time.Hour)
	f.backdate(freshClosed.Submission.ID, 2*time.Hour)
	if code, _ := f.admin("POST", "/api/v1/recordlinks/"+closing.ID+"/revoke", nil); code != 200 {
		t.Fatal("revoke")
	}

	// Off: nothing happens.
	f.cfg.RecordDraftExpiryDays = 0
	if n, _ := f.srv.CleanupExpiredDrafts(time.Now()); n != 0 {
		t.Fatalf("cleanup with expiry off deleted %d", n)
	}
	f.cfg.RecordDraftExpiryDays = 14

	drafts, _ := f.drafts()
	for _, d := range drafts {
		if d.AutoDeleteAt == nil {
			t.Fatalf("no auto-delete time: %+v", d)
		}
		if d.Name == "OnClosed" && (d.Reason != "closed" || !d.LinkClosed) {
			t.Fatalf("closed-link draft %+v", d)
		}
		if d.Name == "Fresh" && !d.Abandoned { // closed link, idle past the upload guard
			t.Fatalf("fresh closed-link draft not abandoned: %+v", d)
		}
	}

	stop := f.srv.StartDraftJanitor(time.Hour) // runs once at start
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for exists(f.sessionDir(stale.Submission.ID)) || exists(f.sessionDir(onClosed.Submission.ID)) {
		if time.Now().After(deadline) {
			t.Fatal("janitor did not clean up on start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	for _, gone := range []string{stale.Submission.ID, onClosed.Submission.ID} {
		if _, err := f.st.GetSubmission(gone); err == nil {
			t.Errorf("%s survives automatic cleanup", gone)
		}
	}
	for _, kept := range []string{week.Submission.ID, freshClosed.Submission.ID, sent.Submission.ID} {
		if _, err := f.st.GetSubmission(kept); err != nil || !exists(f.sessionDir(kept)) {
			t.Errorf("%s deleted: %v", kept, err)
		}
	}
	// A second pass is a no-op.
	if n, _ := f.srv.CleanupExpiredDrafts(time.Now()); n != 0 {
		t.Fatalf("second pass deleted %d", n)
	}
}

// The drafts routes are host management: refused without the admin
// credential, with a submission's host token, and through Funnel [SL-P5-4].
func TestDraftRoutesHostOnly(t *testing.T) {
	f := newRecordFixture(t)
	link, secret := f.createLink(map[string]any{"title": "Intro"})
	s := f.startSubmission(link.ID, secret, "Ana")
	f.backdate(s.Submission.ID, 48*time.Hour)
	host := signaling.MintHostToken(s.Submission.ID, time.Hour, f.cfg.SecretKey)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/v1/recordlinks/drafts", ""},
		{"POST", "/api/v1/recordlinks/drafts/cleanup", "{}"},
		{"DELETE", "/api/v1/recordlinks/drafts/" + s.Submission.ID, ""},
	}
	for _, tc := range routes {
		for _, h := range []map[string]string{{}, {"Authorization": "Bearer nope"}, {"Authorization": "Bearer " + host}, guestHeaders(secret, s.Key)} {
			if code, _ := f.call(tc.method, tc.path, tc.body, h); code != http.StatusForbidden && code != http.StatusUnauthorized {
				t.Errorf("%s %s with %v = %d", tc.method, tc.path, h, code)
			}
		}
		r := httptest.NewRequest(tc.method, "https://studio.tail1234.ts.net"+tc.path, strings.NewReader(tc.body))
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header.Set("Tailscale-Funnel-Request", "?1")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Header.Set("Authorization", "Bearer "+testDesktopToken)
		w := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s through Funnel = %d", tc.method, tc.path, w.Code)
		}
	}
	if _, err := f.st.GetSubmission(s.Submission.ID); err != nil {
		t.Fatal("a refused request deleted the draft")
	}
}
