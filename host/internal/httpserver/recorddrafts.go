package httpserver

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Abandoned record-link drafts [SL-P5-4] (DECISIONS 2026-10-26). A guest who
// opens a record link and never sends leaves a draft submission: a session
// with takes and uploaded media. The host sees abandoned drafts in Home →
// Recordings (size, last activity) and can delete one or all; the daemon
// also deletes them on start and daily:
//
//   - abandoned (listed, deletable by hand): no activity for
//     recordDraftWindow (24 h), or the link is expired/turned off and the
//     draft has had no activity within the upload guard;
//   - expired (deleted automatically): no activity for
//     RecordDraftExpiryDays (14), or the link is expired/turned off and the
//     draft has had no activity for RecordDraftClosedLinkHours (24).
//
// Activity is the latest take start or chunk upload (or the draft's
// creation). A draft with a take recording or activity within the upload
// guard (60 min) is never deleted, by hand or automatically; the check and
// the row delete are one transaction (store.DeleteIdleDraft). Submitted
// recordings are never touched here.
//
//	GET    /api/v1/recordlinks/drafts            list drafts + policy
//	POST   /api/v1/recordlinks/drafts/cleanup    delete abandoned (all, or {"ids":[…]})
//	DELETE /api/v1/recordlinks/drafts/{id}       delete one abandoned draft
//
// All admin-only, never through Funnel (handleRecordLinks checks first).

// draftPolicy is the effective cleanup configuration.
type draftPolicy struct {
	Auto            bool          `json:"autoCleanup"`
	Expiry          time.Duration `json:"-"`
	ClosedLinkGrace time.Duration `json:"-"`
	UploadGuard     time.Duration `json:"-"`
	AbandonedAfter  time.Duration `json:"-"`
	// Wire view in whole units for the dashboard.
	ExpiryDays         int `json:"expiryDays"`
	ClosedLinkHours    int `json:"closedLinkHours"`
	UploadGuardMinutes int `json:"uploadGuardMinutes"`
	AbandonedAfterH    int `json:"abandonedAfterHours"`
}

func (s *Server) draftPolicy() draftPolicy {
	guardMin := s.Config.RecordDraftUploadGuardMinutes
	if guardMin <= 0 {
		guardMin = config.DefaultRecordDraftUploadGuardMinutes
	}
	p := draftPolicy{
		Auto:               s.Config.RecordDraftExpiryDays > 0,
		Expiry:             time.Duration(s.Config.RecordDraftExpiryDays) * 24 * time.Hour,
		ClosedLinkGrace:    time.Duration(max(s.Config.RecordDraftClosedLinkHours, 0)) * time.Hour,
		UploadGuard:        time.Duration(guardMin) * time.Minute,
		AbandonedAfter:     recordDraftWindow,
		ExpiryDays:         s.Config.RecordDraftExpiryDays,
		ClosedLinkHours:    max(s.Config.RecordDraftClosedLinkHours, 0),
		UploadGuardMinutes: guardMin,
		AbandonedAfterH:    int(recordDraftWindow.Hours()),
	}
	// Neither threshold may undercut the upload guard.
	p.ClosedLinkGrace = max(p.ClosedLinkGrace, p.UploadGuard)
	p.AbandonedAfter = max(p.AbandonedAfter, p.UploadGuard)
	if p.Auto {
		p.Expiry = max(p.Expiry, p.AbandonedAfter)
	}
	return p
}

// abandonedCutoff is the latest activity a draft may have to be listed as
// abandoned (and deleted by hand).
func (p draftPolicy) abandonedCutoff(d *store.DraftActivity, now time.Time) time.Time {
	if d.LinkClosed {
		return now.Add(-p.UploadGuard)
	}
	return now.Add(-p.AbandonedAfter)
}

// expiryCutoff is the latest activity a draft may have to be deleted
// automatically; ok is false when automatic cleanup is off.
func (p draftPolicy) expiryCutoff(d *store.DraftActivity, now time.Time) (time.Time, bool) {
	if !p.Auto {
		return time.Time{}, false
	}
	if d.LinkClosed {
		return now.Add(-p.ClosedLinkGrace), true
	}
	return now.Add(-p.Expiry), true
}

// DraftView is a draft on the dashboard.
type DraftView struct {
	store.DraftActivity
	Abandoned bool `json:"abandoned"`
	// Reason is "closed" (the link is expired or turned off) or "idle".
	Reason string `json:"reason,omitempty"`
	// AutoDeleteAt is when automatic cleanup will delete it (omitted when
	// automatic cleanup is off).
	AutoDeleteAt *time.Time `json:"autoDeleteAt,omitempty"`
}

func (s *Server) draftViews(now time.Time) ([]DraftView, draftPolicy, error) {
	p := s.draftPolicy()
	drafts, err := s.Store.ListDraftActivity(now)
	if err != nil {
		return nil, p, err
	}
	out := make([]DraftView, 0, len(drafts))
	for i := range drafts {
		d := &drafts[i]
		v := DraftView{DraftActivity: *d}
		v.Abandoned = !d.Recording && !d.LastActivity.After(p.abandonedCutoff(d, now))
		if v.Abandoned {
			v.Reason = "idle"
			if d.LinkClosed {
				v.Reason = "closed"
			}
		}
		if p.Auto {
			grace := p.Expiry
			if d.LinkClosed {
				grace = p.ClosedLinkGrace
			}
			at := d.LastActivity.Add(grace).UTC()
			v.AutoDeleteAt = &at
		}
		out = append(out, v)
	}
	return out, p, nil
}

// deleteDraft deletes one draft (rows, then files) if its latest activity
// is at or before cutoff. recordMu keeps a take from starting meanwhile.
func (s *Server) deleteDraft(sessionID string, cutoff time.Time, why string) (*store.DraftActivity, error) {
	s.recordMu.Lock()
	d, err := s.Store.DeleteIdleDraft(sessionID, cutoff)
	s.recordMu.Unlock()
	if err != nil {
		return d, err
	}
	if err := os.RemoveAll(filepath.Join(s.Config.DataDir, "sessions", sessionID)); err != nil {
		slog.Warn("remove draft files failed", "sessionId", sessionID, "err", err)
	}
	slog.Info("Record draft deleted", "linkId", d.LinkID, "sessionId", sessionID, "bytes", d.Bytes, "takes", d.Takes, "reason", why)
	return d, nil
}

// CleanupExpiredDrafts deletes the drafts automatic cleanup applies to and
// reports how many and how many bytes of chunks went.
func (s *Server) CleanupExpiredDrafts(now time.Time) (deleted int, bytes int64) {
	p := s.draftPolicy()
	if !p.Auto {
		return 0, 0
	}
	drafts, err := s.Store.ListDraftActivity(now)
	if err != nil {
		slog.Warn("Draft cleanup: cannot list drafts", "err", err)
		return 0, 0
	}
	for i := range drafts {
		d := &drafts[i]
		cutoff, _ := p.expiryCutoff(d, now)
		if d.Recording || d.LastActivity.After(cutoff) {
			continue
		}
		why := "idle"
		if d.LinkClosed {
			why = "link closed"
		}
		if got, err := s.deleteDraft(d.SessionID, cutoff, "expired: "+why); err == nil {
			deleted++
			bytes += got.Bytes
		} else if !errors.Is(err, store.ErrDraftBusy) && !errors.Is(err, sql.ErrNoRows) {
			slog.Warn("Draft cleanup failed", "sessionId", d.SessionID, "err", err)
		}
	}
	if deleted > 0 {
		slog.Info("Abandoned record drafts cleaned up", "deleted", deleted, "bytes", bytes)
	}
	return deleted, bytes
}

// StartDraftJanitor runs CleanupExpiredDrafts now and then every interval
// (daily in the daemon) until stop is called.
func (s *Server) StartDraftJanitor(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		s.CleanupExpiredDrafts(time.Now())
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				s.CleanupExpiredDrafts(now)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// handleRecordDrafts serves /api/v1/recordlinks/drafts[...]; the caller
// has checked adminAllowed.
func (s *Server) handleRecordDrafts(w http.ResponseWriter, r *http.Request, rest string) {
	now := time.Now()
	switch {
	case rest == "" && r.Method == http.MethodGet:
		views, p, err := s.draftViews(now)
		if err != nil {
			http.Error(w, "cannot list drafts", http.StatusInternalServerError)
			return
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{"drafts": views, "policy": p})
	case rest == "cleanup" && r.Method == http.MethodPost:
		var in struct {
			IDs []string `json:"ids"`
		}
		if r.ContentLength != 0 && !decodeBody(w, r, &in) {
			return
		}
		views, _, err := s.draftViews(now)
		if err != nil {
			http.Error(w, "cannot list drafts", http.StatusInternalServerError)
			return
		}
		want := map[string]bool{}
		for _, id := range in.IDs {
			want[id] = true
		}
		p := s.draftPolicy()
		deleted, skipped := []string{}, []string{}
		var bytes int64
		for i := range views {
			v := &views[i]
			if len(want) > 0 && !want[v.SessionID] {
				continue
			}
			if !v.Abandoned {
				if len(want) > 0 {
					skipped = append(skipped, v.SessionID)
				}
				continue
			}
			d, err := s.deleteDraft(v.SessionID, p.abandonedCutoff(&v.DraftActivity, now), "host cleanup")
			if err != nil {
				skipped = append(skipped, v.SessionID)
				continue
			}
			deleted = append(deleted, v.SessionID)
			bytes += d.Bytes
		}
		writeSecretJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "skipped": skipped, "bytes": bytes})
	case rest != "" && !strings.Contains(rest, "/") && r.Method == http.MethodDelete:
		views, _, err := s.draftViews(now)
		if err != nil {
			http.Error(w, "cannot list drafts", http.StatusInternalServerError)
			return
		}
		var v *DraftView
		for i := range views {
			if views[i].SessionID == rest {
				v = &views[i]
			}
		}
		if v == nil {
			http.Error(w, "draft not found", http.StatusNotFound)
			return
		}
		if !v.Abandoned {
			http.Error(w, "this recording is still in progress; it can be deleted once it has been idle", http.StatusConflict)
			return
		}
		if _, err := s.deleteDraft(v.SessionID, s.draftPolicy().abandonedCutoff(&v.DraftActivity, now), "host"); errors.Is(err, store.ErrDraftBusy) {
			http.Error(w, "this recording is in use again; try later", http.StatusConflict)
			return
		} else if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "draft not found", http.StatusNotFound)
			return
		} else if err != nil {
			http.Error(w, "cannot delete the draft", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
