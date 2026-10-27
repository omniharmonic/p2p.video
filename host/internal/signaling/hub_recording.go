package signaling

// Recording control [SL-TECH-1]: shared take start/stop and the session's
// active take.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/omniharmonic/studiolink/host/internal/clock"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func (h *Hub) startRecording(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeTake[sessionID] != nil {
		return
	}

	takes, _ := h.Store.ListTakesBySession(sessionID)
	nextTakeNum := len(takes) + 1

	startAtHostUs := clock.MonotonicNowUs() + 1_500_000 // 1.5s countdown
	takeID := fmt.Sprintf("take_%s_%02d", sessionID, nextTakeNum)

	tk := &store.Take{
		ID:             takeID,
		SessionID:      sessionID,
		Number:         nextTakeNum,
		StartAtHostUs:  startAtHostUs,
		WallClockStart: clock.HostWallClockFromMonotonic(startAtHostUs),
		Status:         "recording",
	}
	expected := make([]string, 0)
	for _, c := range h.sessionGroups[sessionID] {
		// A phone is an optional extra source: it never blocks a take. A
		// producer is never recorded [SL-P1-11].
		if c.getStatus() == "admitted" && c.Role != RoleSubsource && c.Role != RoleProducer {
			expected = append(expected, c.ParticipantID)
		}
	}
	if err := h.Store.CreateTakeWithParticipants(tk, expected); err != nil {
		slog.Error("Failed to create take", "err", err)
		return
	}
	h.activeTake[sessionID] = tk
	if h.References != nil {
		// Alignment reference only (ADR-007): never exported [SL-P0-10].
		refs := filepath.Join(h.Config.DataDir, "sessions", sessionID, "takes", takeID, "refs")
		if err := h.References.StartTake(sessionID, refs); err != nil {
			slog.Warn("Reference recording unavailable; alignment stays clock-based", "takeId", takeID, "err", err)
		}
	}

	payload := RecordStartPayload{
		TakeID:        takeID,
		TakeNumber:    nextTakeNum,
		StartAtHostUs: startAtHostUs,
		Preset:        "1080p30",
	}
	raw, _ := json.Marshal(payload)
	env := &Envelope{Type: TypeRecordStart, Payload: raw}

	if grp, ok := h.sessionGroups[sessionID]; ok {
		for _, c := range grp {
			// Producers learn about the take from room_state only: they never record.
			if c.getStatus() == "admitted" && c.Role != RoleProducer {
				c.SendEnvelope(env)
			}
		}
	}
	slog.Info("Recording started for take", "takeId", takeID, "takeNumber", nextTakeNum, "startAtUs", startAtHostUs)
}

func (h *Hub) stopRecording(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	tk, ok := h.activeTake[sessionID]
	if !ok {
		return
	}
	stopAtHostUs := clock.MonotonicNowUs()
	tk.StopAtHostUs = stopAtHostUs
	tk.Status = "stopped"
	_ = h.Store.UpdateTake(tk)
	delete(h.activeTake, sessionID)
	if h.References != nil {
		h.References.StopTake(sessionID)
	}

	payload := RecordStopPayload{
		TakeID:       tk.ID,
		StopAtHostUs: stopAtHostUs,
	}
	raw, _ := json.Marshal(payload)
	env := &Envelope{Type: TypeRecordStop, Payload: raw}

	if grp, ok := h.sessionGroups[sessionID]; ok {
		for _, c := range grp {
			if c.getStatus() == "admitted" && c.Role != RoleProducer {
				c.SendEnvelope(env)
			}
		}
	}
	slog.Info("Recording stopped for take", "takeId", tk.ID, "stopAtUs", stopAtHostUs)
}

func (h *Hub) currentTakeID(sessionID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if take := h.activeTake[sessionID]; take != nil {
		return take.ID
	}
	return ""
}

// RecordsReference reports whether a participant's received microphone may
// be kept as an alignment reference (refrec.Manager.Allow): admitted people
// of this session who are recorded. A producer is never recorded, so never
// referenced either [SL-P1-11][SL-P0-10].
func (h *Hub) RecordsReference(sessionID, participantID string) bool {
	h.mu.RLock()
	c := h.clients[participantID]
	h.mu.RUnlock()
	return c != nil && c.SessionID == sessionID && c.Role != RoleProducer && c.getStatus() == "admitted"
}
