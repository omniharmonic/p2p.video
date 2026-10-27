package postproc

import (
	"log/slog"

	"github.com/omniharmonic/studiolink/host/internal/export"
)

// manifestParticipants lists the people who own the take's exported
// sources, in source order, with their lower-third text [SL-P5-2]. A
// missing lower third falls back to the display name with no title.
func (p *Pipeline) manifestParticipants(sessionID string, sources []export.ExportSource) []export.ManifestParticipant {
	lts, err := p.Store.ListLowerThirds(sessionID)
	if err != nil {
		slog.Warn("Lower thirds unavailable for the manifest", "sessionId", sessionID, "err", err)
	}
	seen := map[string]bool{}
	var out []export.ManifestParticipant
	for _, src := range sources {
		if src.ParticipantID == "" || seen[src.ParticipantID] {
			continue
		}
		seen[src.ParticipantID] = true
		part, err := p.Store.GetParticipant(src.ParticipantID)
		if err != nil || part == nil {
			continue
		}
		mp := export.ManifestParticipant{ID: part.ID, Name: part.Name, Role: part.Role, LowerThirdName: part.Name}
		if lt, ok := lts[part.ID]; ok {
			if lt.Name != "" {
				mp.LowerThirdName = lt.Name
			}
			mp.Title = lt.Title
		}
		out = append(out, mp)
	}
	return out
}
