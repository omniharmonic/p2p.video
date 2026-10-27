package httpserver

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/postproc"
)

func (s *Server) handleTakeDownload(w http.ResponseWriter, r *http.Request, takeID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	take, err := s.Store.GetTake(takeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.requireHost(w, r, take.SessionID) {
		return
	}
	if s.Pipeline == nil {
		http.Error(w, "editor package unavailable", 503)
		return
	}
	path, err := s.Pipeline.PrepareDownload(takeID)
	if err != nil {
		http.Error(w, "editor package is not ready: "+err.Error(), 409)
		return
	}
	defer os.Remove(path)
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "cannot open editor package", 500)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="p2p.video-editor-package.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "p2p.video-editor-package.zip", time.Time{}, file)
}

// handleOriginalsDownload streams the quick-delivery package: each
// participant's assembled originals plus manifest and offsets, available
// while the conformed export is still running (SL-P0-9). Host-only, like the
// editor package; streamed so it starts immediately whatever its size.
func (s *Server) handleOriginalsDownload(w http.ResponseWriter, r *http.Request, takeID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	take, err := s.Store.GetTake(takeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.requireHost(w, r, take.SessionID) {
		return
	}
	if s.Pipeline == nil {
		http.Error(w, "originals unavailable", 503)
		return
	}
	pkg, err := s.Pipeline.PlanOriginals(takeID)
	if err != nil {
		http.Error(w, "originals are not ready: "+err.Error(), 409)
		return
	}
	files, err := pkg.OpenFiles()
	if err != nil {
		http.Error(w, "cannot open originals", 500)
		return
	}
	defer postproc.CloseFiles(files)
	name := fmt.Sprintf("p2p.video-take-%02d-originals.zip", take.Number)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := pkg.WriteZip(w, files); err != nil {
		// Headers may be sent; the truncated ZIP fails to open rather than
		// passing as complete.
		slog.Warn("originals download failed", "takeId", takeID, "err", err)
	}
}
