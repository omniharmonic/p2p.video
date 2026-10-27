package signaling

import "sort"

// DesktopWaiting is a guest waiting for admission. It carries no credentials.
type DesktopWaiting struct {
	ParticipantID string `json:"participantId"`
	Name          string `json:"name"`
}

// DesktopSession summarizes one live session for the native host app.
type DesktopSession struct {
	ID              string           `json:"id"`
	Connected       int              `json:"connected"`
	Admitted        int              `json:"admitted"`
	HostConnected   bool             `json:"hostConnected"`
	Waiting         []DesktopWaiting `json:"waiting"`
	Recording       bool             `json:"recording"`
	TakeID          string           `json:"takeId,omitempty"`
	TakeNumber      int              `json:"takeNumber,omitempty"`
	UploadingTracks int              `json:"uploadingTracks"`
}

// DesktopSnapshot returns in-memory call state for sessions with at least one
// connected socket or a take still recording, sorted by session ID. It never
// includes tokens.
func (h *Hub) DesktopSnapshot() []DesktopSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]DesktopSession, 0, len(h.sessionGroups))
	for sid, grp := range h.sessionGroups {
		if len(grp) == 0 {
			continue
		}
		ds := DesktopSession{ID: sid, Waiting: []DesktopWaiting{}}
		if tk := h.activeTake[sid]; tk != nil {
			ds.Recording, ds.TakeID, ds.TakeNumber = true, tk.ID, tk.Number
		}
		for _, c := range grp {
			ds.Connected++
			c.mu.RLock()
			status, name, role, initialized := c.Status, c.Name, c.Role, c.initialized
			for _, tr := range c.Tracks {
				if tr.State == "recording" || tr.State == "uploading" || tr.BufferedBytes > 0 {
					ds.UploadingTracks++
				}
			}
			c.mu.RUnlock()
			switch {
			case status == "admitted":
				ds.Admitted++
				if role == "host" {
					ds.HostConnected = true
				}
			case status == "pending" && initialized:
				ds.Waiting = append(ds.Waiting, DesktopWaiting{ParticipantID: c.ParticipantID, Name: name})
			}
		}
		sort.Slice(ds.Waiting, func(i, j int) bool { return ds.Waiting[i].ParticipantID < ds.Waiting[j].ParticipantID })
		out = append(out, ds)
	}
	// A take keeps recording after every socket has left (e.g. the host's
	// studio window closed mid-take) [SL-P2-3]: still report it, so the app
	// warns before quitting and never restarts the daemon under it.
	for sid, tk := range h.activeTake {
		if tk == nil || len(h.sessionGroups[sid]) > 0 {
			continue
		}
		out = append(out, DesktopSession{ID: sid, Waiting: []DesktopWaiting{}, Recording: true, TakeID: tk.ID, TakeNumber: tk.Number})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
