package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg" // registers JPEG for image.DecodeConfig
	_ "image/png"  // registers PNG for image.DecodeConfig
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Branding [SL-P5-2]: brand settings and the logo of a room or instant
// meeting. Every route is host management:
//
//	GET|PUT          /api/v1/sessions/{id}/branding        requireHost
//	GET|POST|DELETE  /api/v1/sessions/{id}/branding/logo   requireHost
//	GET|PUT          /api/v1/rooms/{id}/branding           adminAllowed
//	GET|POST|DELETE  /api/v1/rooms/{id}/branding/logo      adminAllowed
//
// A meeting opened from a room reads and writes the room's settings. The
// logo is PNG or JPEG only (an SVG can carry script), at most 1 MB, and must
// start with the declared format's magic bytes and decode as an image of at
// most 4096×4096. It is served from our own origin with the stored type,
// nosniff and a sandboxing CSP, so the livestream compositor can draw it
// under img-src 'self' blob:.

var (
	pngMagic  = []byte("\x89PNG\r\n\x1a\n")
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
)

// errLogo is a validation failure with its HTTP status.
type errLogo struct {
	status int
	msg    string
}

func (e *errLogo) Error() string { return e.msg }

// validateLogo checks a logo upload: the declared type must be PNG or JPEG,
// the bytes must start with that format's signature and decode (header) to
// a sane size. It returns the content type to store.
func validateLogo(declared string, data []byte) (string, error) {
	mt, _, err := mime.ParseMediaType(declared)
	if err != nil {
		mt = ""
	}
	var want []byte
	switch mt {
	case "image/png":
		want = pngMagic
	case "image/jpeg":
		want = jpegMagic
	default:
		return "", &errLogo{http.StatusUnsupportedMediaType, "Logos must be PNG or JPEG (SVG isn't accepted)."}
	}
	if len(data) == 0 {
		return "", &errLogo{http.StatusBadRequest, "The logo file is empty."}
	}
	if len(data) > signaling.BrandLogoMaxBytes {
		return "", &errLogo{http.StatusRequestEntityTooLarge, "Logos can be at most 1 MB."}
	}
	if !bytes.HasPrefix(data, want) {
		return "", &errLogo{http.StatusUnsupportedMediaType, "The file isn't a valid " + strings.TrimPrefix(mt, "image/") + " image."}
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != mt {
		return "", &errLogo{http.StatusUnsupportedMediaType, "The file isn't a valid " + strings.TrimPrefix(mt, "image/") + " image."}
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > signaling.BrandLogoMaxDimension || cfg.Height > signaling.BrandLogoMaxDimension {
		return "", &errLogo{http.StatusUnprocessableEntity, "Logos can be at most 4096×4096 pixels."}
	}
	return mt, nil
}

// brandSettings is the public view of a scope's stored brand.
func brandSettings(p *store.Production) signaling.BrandSettings {
	out := signaling.BrandSettings{
		ShowName:        p.Brand.ShowName,
		Accent:          signaling.DefaultBrandAccent,
		TileLabels:      p.Brand.TileLabels,
		LowerThirdsMode: signaling.LowerThirdsSpeaker,
	}
	if signaling.ValidAccent(p.Brand.Accent) {
		out.Accent = strings.ToLower(p.Brand.Accent)
	}
	if signaling.ValidLowerThirdsMode(p.Brand.LowerThirds) {
		out.LowerThirdsMode = p.Brand.LowerThirds
	}
	if p.LogoSHA != "" {
		out.Logo = &signaling.BrandLogo{Type: p.LogoType, Size: p.LogoSize, Version: p.LogoSHA[:min(16, len(p.LogoSHA))]}
	}
	return out
}

// handleSessionBranding serves /api/v1/sessions/{id}/branding[/logo].
func (s *Server) handleSessionBranding(w http.ResponseWriter, r *http.Request, sessID, sub string) {
	if !s.requireHost(w, r, sessID) {
		return
	}
	sess, err := s.Store.GetSession(sessID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.serveBranding(w, r, store.ProductionScope(sess), sub)
}

// handleRoomBranding serves /api/v1/rooms/{id}/branding[/logo]; the caller
// has checked adminAllowed.
func (s *Server) handleRoomBranding(w http.ResponseWriter, r *http.Request, room *store.Room, sub string) {
	s.serveBranding(w, r, store.RoomProducerScope(room.ID), sub)
}

func (s *Server) serveBranding(w http.ResponseWriter, r *http.Request, scope, sub string) {
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.writeBrand(w, scope)
	case sub == "" && r.Method == http.MethodPut:
		var in signaling.BrandSettings
		if !decodeBody(w, r, &in) {
			return
		}
		brand := store.Brand{
			ShowName:   signaling.SanitizeDisplayText(in.ShowName, signaling.BrandShowNameMaxRunes),
			Accent:     strings.ToLower(in.Accent),
			TileLabels: in.TileLabels,
		}
		if !signaling.ValidAccent(brand.Accent) {
			http.Error(w, "accent must be a #rrggbb colour", http.StatusBadRequest)
			return
		}
		brand.LowerThirds = in.LowerThirdsMode
		if brand.LowerThirds == "" {
			brand.LowerThirds = signaling.LowerThirdsSpeaker
		}
		if !signaling.ValidLowerThirdsMode(brand.LowerThirds) {
			http.Error(w, "lowerThirdsMode must be speaker, manual or off", http.StatusBadRequest)
			return
		}
		if err := s.Store.SetBrand(scope, brand); err != nil {
			slog.Error("Saving brand settings failed", "scope", scope, "err", err)
			http.Error(w, "cannot save brand settings", http.StatusInternalServerError)
			return
		}
		slog.Info("Brand settings updated", "scope", scope)
		s.brandingChanged(scope)
		s.writeBrand(w, scope)
	case sub == "logo" && r.Method == http.MethodGet:
		data, typ, sha, err := s.Store.Logo(scope)
		if err != nil {
			http.Error(w, "cannot read logo", http.StatusInternalServerError)
			return
		}
		if data == nil || (typ != "image/png" && typ != "image/jpeg") {
			http.Error(w, "no logo", http.StatusNotFound)
			return
		}
		h := w.Header()
		h.Set("Content-Type", typ)
		h.Set("Content-Length", strconv.Itoa(len(data)))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		h.Set("Content-Disposition", "inline")
		h.Set("Cache-Control", "private, no-cache")
		h.Set("ETag", `"`+sha+`"`)
		_, _ = w.Write(data)
	case sub == "logo" && r.Method == http.MethodPost:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, signaling.BrandLogoMaxBytes+1))
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || len(body) > signaling.BrandLogoMaxBytes {
			http.Error(w, "Logos can be at most 1 MB.", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "cannot read upload", http.StatusBadRequest)
			return
		}
		typ, err := validateLogo(r.Header.Get("Content-Type"), body)
		if err != nil {
			var le *errLogo
			if errors.As(err, &le) {
				http.Error(w, le.msg, le.status)
				return
			}
			http.Error(w, "invalid logo", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		if err := s.Store.SetLogo(scope, body, typ, hex.EncodeToString(sum[:])); err != nil {
			slog.Error("Saving the logo failed", "scope", scope, "err", err)
			http.Error(w, "cannot save logo", http.StatusInternalServerError)
			return
		}
		slog.Info("Logo uploaded", "scope", scope, "type", typ, "bytes", len(body))
		s.brandingChanged(scope)
		s.writeBrand(w, scope)
	case sub == "logo" && r.Method == http.MethodDelete:
		if err := s.Store.SetLogo(scope, nil, "", ""); err != nil {
			http.Error(w, "cannot remove logo", http.StatusInternalServerError)
			return
		}
		slog.Info("Logo removed", "scope", scope)
		s.brandingChanged(scope)
		s.writeBrand(w, scope)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) writeBrand(w http.ResponseWriter, scope string) {
	p, err := s.Store.GetProduction(scope)
	if err != nil {
		http.Error(w, "cannot read brand settings", http.StatusInternalServerError)
		return
	}
	writeSecretJSON(w, http.StatusOK, brandSettings(p))
}

// handleRoomTitles serves /api/v1/rooms/{id}/titles (the caller has checked
// adminAllowed): the lower thirds a room remembers by display name for its
// next episode [SL-P5-4].
//
//	GET                       list
//	PUT {displayName,name,title}  remember (both empty forgets)
//	DELETE ?key=<name key>    forget
//
// Changes apply from the next time the person joins; a live meeting keeps
// its own lower thirds (edited in Branding & titles).
func (s *Server) handleRoomTitles(w http.ResponseWriter, r *http.Request, room *store.Room) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		var in struct {
			DisplayName string `json:"displayName"`
			Name        string `json:"name"`
			Title       string `json:"title"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		display := signaling.SanitizeDisplayText(in.DisplayName, signaling.LowerThirdNameMaxRunes)
		if store.LowerThirdKey(display) == "" {
			http.Error(w, "displayName is required", http.StatusBadRequest)
			return
		}
		name := signaling.SanitizeDisplayText(in.Name, signaling.LowerThirdNameMaxRunes)
		if name == display {
			name = ""
		}
		title := signaling.SanitizeDisplayText(in.Title, signaling.LowerThirdTitleMaxRunes)
		if err := s.Store.RememberRoomLowerThird(room.ID, display, name, title); err != nil {
			slog.Error("Saving a room title failed", "roomId", room.ID, "err", err)
			http.Error(w, "cannot save the title", http.StatusInternalServerError)
			return
		}
		slog.Info("Room title saved", "roomId", room.ID)
	case http.MethodDelete:
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "key is required", http.StatusBadRequest)
			return
		}
		if err := s.Store.ForgetRoomLowerThird(room.ID, key); err != nil {
			http.Error(w, "cannot forget the title", http.StatusInternalServerError)
			return
		}
		slog.Info("Room title forgotten", "roomId", room.ID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	titles, err := s.Store.ListRoomLowerThirds(room.ID)
	if err != nil {
		http.Error(w, "cannot read titles", http.StatusInternalServerError)
		return
	}
	writeSecretJSON(w, http.StatusOK, map[string]any{"titles": titles})
}

// brandingChanged pushes the new brand to the live meeting using scope.
func (s *Server) brandingChanged(scope string) {
	if s.Hub == nil {
		return
	}
	if id, ok := strings.CutPrefix(scope, "session:"); ok {
		s.Hub.BrandingChanged(id)
		return
	}
	if id, ok := strings.CutPrefix(scope, "room:"); ok {
		if sess, err := s.Store.LiveRoomSession(id); err == nil {
			s.Hub.BrandingChanged(sess.ID)
		}
	}
}
