package upload

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Upload limits [SL-SEC-1]. Uploads come from admitted guests, possibly over
// Funnel, so each is bounded: per chunk (Config.MaxChunkSizeMB), per sequence
// number, per track (a byte quota derived from the take's length) and per
// participant (tracks per take).
const (
	// MaxSeq is the highest chunk sequence number accepted: ~11 days of 1 s
	// MediaRecorder fragments.
	MaxSeq = 1_000_000
	// MaxTracksPerParticipantTake bounds registrations per participant per
	// take. A normal participant registers camera, mic, PCM and screen (4);
	// the rest is headroom for device switches and re-shares.
	MaxTracksPerParticipantTake = 16
	// QuotaFloorBps is the lowest bitrate the per-track quota assumes, so a
	// low preset never squeezes a PCM (3.1 Mbit/s) or screen track.
	QuotaFloorBps = 12_000_000
	// QuotaGrace is added to the take's duration before the quota is computed.
	QuotaGrace = 10 * time.Minute
	// QuotaFactor is headroom for encoder overshoot and container overhead.
	QuotaFactor = 1.5
	// bodyMinRateBps is the slowest body rate the server waits for: half the
	// client's own 256 kbit/s timeout assumption, so a client always gives up
	// first (web/src/lib/upload.ts).
	bodyMinRateBps = 128_000
	// bodyBaseTimeout is added to every chunk's body deadline.
	bodyBaseTimeout = 30 * time.Second
)

// Handler manages chunked uploads and file assembly.
type Handler struct {
	Store          *store.Store
	Config         *config.Config
	OnAssembleDone func(takeID string, trackID string, assembledPath string)

	// trackLocks holds one *sync.Mutex per track: mutation and finalization of
	// a track are serialized, but one slow upload never blocks other tracks.
	trackLocks sync.Map
	// linkLocks holds one *sync.Mutex per record link (shared storage cap).
	linkLocks sync.Map
	// regMu serializes track registration so the per-participant cap holds.
	regMu sync.Mutex
	// quotaFloorBps overrides QuotaFloorBps in tests.
	quotaFloorBps int
	// now is the test clock; nil is time.Now.
	now func() time.Time
}

// linkLock serializes the storage-cap commit of one record link [SL-P5-3].
func (h *Handler) linkLock(linkID string) *sync.Mutex {
	m, _ := h.linkLocks.LoadOrStore(linkID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (h *Handler) trackLock(trackID string) *sync.Mutex {
	m, _ := h.trackLocks.LoadOrStore(trackID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (h *Handler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// TrackQuota is the most bytes one track of tk may hold: the session's video
// bitrate (at least QuotaFloorBps) for the take's duration plus QuotaGrace,
// times QuotaFactor (docs/ARCHITECTURE.md §10). A recording take's duration
// grows with the wall clock; a stopped take's is fixed.
func (h *Handler) TrackQuota(tk *store.Take) int64 {
	settings := store.DefaultSessionSettings()
	if sess, err := h.Store.GetSession(tk.SessionID); err == nil && sess.PresetJSON != "" {
		_ = json.Unmarshal([]byte(sess.PresetJSON), &settings)
	}
	floor := h.quotaFloorBps
	if floor <= 0 {
		floor = QuotaFloorBps
	}
	rate := max(settings.VideoBitrateBps, floor)
	var dur time.Duration
	switch {
	case tk.StopAtHostUs > tk.StartAtHostUs && tk.StartAtHostUs > 0:
		dur = time.Duration(tk.StopAtHostUs-tk.StartAtHostUs) * time.Microsecond
	case !tk.WallClockStart.IsZero():
		dur = max(h.clock().Sub(tk.WallClockStart), 0)
	}
	return int64(float64(rate) / 8 * (dur + QuotaGrace).Seconds() * QuotaFactor)
}

// CompleteRequest carries payload for completing a track upload.
type CompleteRequest struct {
	LastSeq    int                    `json:"lastSeq"`
	TotalBytes int64                  `json:"totalBytes"`
	Sidecar    map[string]interface{} `json:"sidecar,omitempty"`
}

// StatusResponse is returned by the status endpoint.
type StatusResponse struct {
	Have [][2]int `json:"have"`
}

// ServeHTTP routes upload endpoints.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Parse path: /api/v1/takes/{takeId}/tracks/{trackId}/...
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/takes/")
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[1] == "tracks" && r.Method == http.MethodPost {
		h.registerTrack(w, r, parts[0])
		return
	}
	if len(parts) < 4 || parts[1] != "tracks" {
		http.Error(w, "invalid upload path", http.StatusNotFound)
		return
	}

	takeID := parts[0]
	trackID := parts[2]
	action := parts[3]

	// Authenticate bearer token
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
	claims, err := signaling.VerifyUploadToken(tokenStr, h.Config.SecretKey)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid token: %v", err), http.StatusUnauthorized)
		return
	}

	// A signature is insufficient: bind every operation to the stored take and source owner.
	tk, err := h.Store.GetTake(takeID)
	if err != nil {
		http.Error(w, "take not found", 404)
		return
	}
	part, err := h.Store.GetParticipant(claims.ParticipantID)
	if err != nil || part.SessionID != claims.SessionID || tk.SessionID != claims.SessionID || part.Status == "denied" || part.Status == "pending" {
		http.Error(w, "upload grant does not authorize this take", 403)
		return
	}
	tr, err := h.Store.GetTrack(trackID)
	if err != nil || tr.TakeID != takeID {
		http.Error(w, "track not found in this take", 404)
		return
	}
	src, err := h.Store.GetSource(tr.SourceID)
	if err != nil || src.ParticipantID != claims.ParticipantID {
		http.Error(w, "track belongs to another participant", 403)
		return
	}
	switch action {
	case "chunks":
		if len(parts) < 5 {
			http.Error(w, "missing sequence number", http.StatusBadRequest)
			return
		}
		seq, err := strconv.Atoi(parts[4])
		if err != nil || seq < 0 {
			http.Error(w, "invalid sequence number", http.StatusBadRequest)
			return
		}
		if seq > MaxSeq {
			http.Error(w, fmt.Sprintf("chunk sequence %d exceeds the host's limit of %d fragments per track", seq, MaxSeq), http.StatusRequestEntityTooLarge)
			return
		}
		if r.Method == http.MethodPut {
			// The body is read before any lock is taken (see handlePutChunk).
			h.handlePutChunk(w, r, claims, tk, trackID, seq)
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "status":
		if r.Method == http.MethodGet {
			h.handleStatus(w, r, claims, takeID, trackID)
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "complete":
		if r.Method == http.MethodPost {
			// Serialize finalization with this track's chunk commits.
			mu := h.trackLock(trackID)
			mu.Lock()
			defer mu.Unlock()
			h.handleComplete(w, r, claims, takeID, trackID)
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

// handlePutChunk stores one fragment. The body is streamed to a private temp
// file and hashed BEFORE the track lock is taken, so a slow uplink (or a
// slowloris over Funnel) only ever holds its own request goroutine; the
// commit (idempotency check, quota check, rename, database row) is the only
// part serialized per track [SL-SEC-1].
func (h *Handler) handlePutChunk(w http.ResponseWriter, r *http.Request, claims *signaling.ParticipantClaims, tk *store.Take, trackID string, seq int) {
	maxBytes := h.Config.MaxChunkSizeMB * 1024 * 1024
	if r.ContentLength > maxBytes {
		http.Error(w, fmt.Sprintf("chunk exceeds the host's %d MB fragment limit", h.Config.MaxChunkSizeMB), http.StatusRequestEntityTooLarge)
		return
	}
	// A slow-but-legitimate body gets its size at bodyMinRateBps plus a base;
	// a stalled one is cut off instead of holding a goroutine forever.
	expect := r.ContentLength
	if expect <= 0 {
		expect = maxBytes
	}
	deadline := bodyBaseTimeout + time.Duration(expect*8*int64(time.Second)/bodyMinRateBps)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(deadline))

	tr, err := h.Store.GetTrack(trackID)
	if err != nil {
		http.Error(w, "track unavailable", 404)
		return
	}
	if tr.Status == "assembled" || tr.Status == "processed" {
		http.Error(w, "finalized track cannot accept new fragments", 409)
		return
	}
	quota := h.TrackQuota(tk)
	if r.ContentLength > 0 {
		if stored, err := h.Store.StoredChunkBytes(trackID); err == nil && stored+r.ContentLength > quota {
			quotaExceeded(w, quota)
			return
		}
	}

	chunkDir := filepath.Join(h.Config.DataDir, "sessions", tk.SessionID, "takes", tk.ID, "raw", trackID)
	if err := os.MkdirAll(chunkDir, 0o700); err != nil {
		http.Error(w, "failed to create chunk dir", http.StatusInternalServerError)
		return
	}
	f, err := os.CreateTemp(chunkDir, fmt.Sprintf(".%06d-*.part", seq)) // 0600
	if err != nil {
		http.Error(w, "failed to create chunk file", 500)
		return
	}
	tempPath := f.Name()
	defer os.Remove(tempPath) // no-op once renamed
	hasher := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, hasher), http.MaxBytesReader(w, r.Body, maxBytes))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		var tooBig *http.MaxBytesError
		if errors.As(copyErr, &tooBig) {
			http.Error(w, fmt.Sprintf("chunk exceeds the host's %d MB fragment limit", h.Config.MaxChunkSizeMB), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	if syncErr != nil || closeErr != nil {
		http.Error(w, "failed to persist chunk file", 500)
		return
	}
	if n == 0 {
		http.Error(w, "empty media fragment", 400)
		return
	}
	computedHash := hex.EncodeToString(hasher.Sum(nil))
	expectedHash := r.Header.Get("X-Chunk-SHA256")
	if expectedHash != "" && !strings.EqualFold(expectedHash, computedHash) {
		http.Error(w, "chunk hash mismatch", http.StatusBadRequest)
		return
	}

	// Commit: serialized per track so concurrent retries cannot replace a
	// verified chunk or race past the quota.
	mu := h.trackLock(trackID)
	mu.Lock()
	defer mu.Unlock()

	exists, existingHash, err := h.Store.CheckChunkExists(trackID, seq)
	if err == nil && exists {
		if strings.EqualFold(existingHash, computedHash) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"already_stored"}`))
			return
		}
		http.Error(w, "chunk sequence conflict with different hash", http.StatusConflict)
		return
	}
	if tr, err = h.Store.GetTrack(trackID); err != nil {
		http.Error(w, "track unavailable", 404)
		return
	}
	if tr.Status == "assembled" || tr.Status == "processed" {
		http.Error(w, "finalized track cannot accept new fragments", 409)
		return
	}
	stored, err := h.Store.StoredChunkBytes(trackID)
	if err != nil {
		http.Error(w, "cannot check track quota", http.StatusInternalServerError)
		return
	}
	if stored+n > quota {
		slog.Warn("Upload refused: track quota exceeded", "trackId", trackID, "participantId", claims.ParticipantID, "storedBytes", stored, "quotaBytes", quota)
		quotaExceeded(w, quota)
		return
	}
	// A record link's submissions share one storage cap [SL-P5-3]. The link
	// lock (taken after the track lock, always in that order) serializes the
	// check-and-commit across every track under the link.
	if link, err := h.Store.SubmissionLink(tk.SessionID); err == nil {
		lm := h.linkLock(link.ID)
		lm.Lock()
		defer lm.Unlock()
		used, err := h.Store.LinkStoredBytes(link.ID)
		if err != nil {
			http.Error(w, "cannot check the link's storage limit", http.StatusInternalServerError)
			return
		}
		if used+n > link.MaxTotalBytes {
			slog.Warn("Upload refused: record link storage cap", "linkId", link.ID, "trackId", trackID, "usedBytes", used, "capBytes", link.MaxTotalBytes)
			http.Error(w, fmt.Sprintf("this recording link has used the host's storage limit (%d MB); the recording stays on this device", link.MaxTotalBytes>>20), http.StatusRequestEntityTooLarge)
			return
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "cannot check the take's owner", http.StatusInternalServerError)
		return
	}
	chunkPath := filepath.Join(chunkDir, fmt.Sprintf("%06d.bin", seq))
	if err := os.Rename(tempPath, chunkPath); err != nil {
		http.Error(w, "failed to finalize chunk file", http.StatusInternalServerError)
		return
	}
	chunkRecord := &store.Chunk{
		TrackID:    trackID,
		Seq:        seq,
		Size:       n,
		SHA256:     computedHash,
		ReceivedAt: time.Now().UTC(),
	}
	if err := h.Store.StoreChunk(chunkRecord); err != nil {
		http.Error(w, "failed to store chunk record", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"status":"stored"}`))
}

func quotaExceeded(w http.ResponseWriter, quota int64) {
	http.Error(w, fmt.Sprintf("track exceeds the host's storage limit for this take (%d MB); the recording stays on this device", quota>>20), http.StatusRequestEntityTooLarge)
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request, claims *signaling.ParticipantClaims, takeID, trackID string) {
	seqs, err := h.Store.GetStoredChunkSeqs(trackID)
	if err != nil {
		http.Error(w, "failed to retrieve chunks", http.StatusInternalServerError)
		return
	}

	ranges := ComputeContiguousRanges(seqs)
	resp := StatusResponse{Have: ranges}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handleComplete(w http.ResponseWriter, r *http.Request, claims *signaling.ParticipantClaims, takeID, trackID string) {
	var req CompleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.LastSeq < 0 || req.TotalBytes <= 0 {
		http.Error(w, "empty recording cannot be finalized", 400)
		return
	}
	seqs, err := h.Store.GetStoredChunkSeqs(trackID)
	if err != nil {
		http.Error(w, "failed to query chunks", http.StatusInternalServerError)
		return
	}

	if req.LastSeq >= len(seqs) || !IsContiguousUpTo(seqs, req.LastSeq) {
		http.Error(w, "missing chunks in range 0..lastSeq", http.StatusBadRequest)
		return
	}

	if len(seqs) != req.LastSeq+1 {
		http.Error(w, "manifest omits acknowledged fragments", 409)
		return
	}
	tk, err := h.Store.GetTake(takeID)
	if err != nil {
		http.Error(w, "take not found", http.StatusNotFound)
		return
	}

	rawDir := filepath.Join(h.Config.DataDir, "sessions", tk.SessionID, "takes", takeID, "raw")
	chunkDir := filepath.Join(rawDir, trackID)

	// Fetch track to know extension
	tr, err := h.Store.GetTrack(trackID)
	ext := ".webm"
	if err == nil && tr != nil {
		if strings.Contains(tr.Mime, "mp4") {
			ext = ".mp4"
		} else if strings.Contains(tr.Mime, "wav") {
			ext = ".wav"
		}
	}

	assembledPath := filepath.Join(rawDir, fmt.Sprintf("%s%s", trackID, ext))
	assembledTemp := assembledPath + ".assembling"
	assembledFile, err := os.OpenFile(assembledTemp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		http.Error(w, "failed to create assembled file", http.StatusInternalServerError)
		return
	}
	defer assembledFile.Close()
	defer os.Remove(assembledTemp)

	hasher := sha256.New()
	multiWriter := io.MultiWriter(assembledFile, hasher)

	var totalBytes int64
	for i := 0; i <= req.LastSeq; i++ {
		chunkPath := filepath.Join(chunkDir, fmt.Sprintf("%06d.bin", i))
		cFile, err := os.Open(chunkPath)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to open chunk %d: %v", i, err), http.StatusInternalServerError)
			return
		}
		// Verify durable chunk bytes still match the acknowledged digest.
		chunkHash := sha256.New()
		n, err := io.Copy(io.MultiWriter(multiWriter, chunkHash), cFile)
		_ = cFile.Close()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to copy chunk %d: %v", i, err), http.StatusInternalServerError)
			return
		}
		_, storedHash, checkErr := h.Store.CheckChunkExists(trackID, i)
		if checkErr != nil || hex.EncodeToString(chunkHash.Sum(nil)) != storedHash {
			http.Error(w, "stored chunk integrity failure", 409)
			return
		}
		totalBytes += n
	}

	if totalBytes != req.TotalBytes {
		http.Error(w, "total byte count mismatch", 409)
		return
	}
	if err := assembledFile.Sync(); err != nil {
		http.Error(w, "cannot persist assembly", 500)
		return
	}
	if err := assembledFile.Close(); err != nil {
		http.Error(w, "cannot close assembly", 500)
		return
	}
	if err := os.Rename(assembledTemp, assembledPath); err != nil {
		http.Error(w, "cannot publish assembly", 500)
		return
	}
	overallHash := hex.EncodeToString(hasher.Sum(nil))

	// Write sidecar JSON if provided
	if req.Sidecar != nil {
		sidecarPath := filepath.Join(rawDir, fmt.Sprintf("%s.sidecar.json", trackID))
		sidecarBytes, _ := json.MarshalIndent(req.Sidecar, "", "  ")
		if err := os.WriteFile(sidecarPath, sidecarBytes, 0600); err != nil {
			http.Error(w, "cannot persist timing sidecar", 500)
			return
		}
	}

	// Update track record
	if tr != nil {
		tr.LastSeq = req.LastSeq
		tr.TotalBytes = totalBytes
		tr.SHA256 = overallHash
		tr.Status = "assembled"
		if req.Sidecar["pcm"] != nil {
			if complete, ok := req.Sidecar["captureComplete"].(bool); !ok || !complete {
				tr.Status = "partial"
			}
		}
		if err := h.Store.UpdateTrack(tr); err != nil {
			http.Error(w, "cannot persist assembly receipt", 500)
			return
		}
	}

	slog.Info("Track assembly completed", "trackId", trackID, "totalBytes", totalBytes, "sha256", overallHash)

	if h.OnAssembleDone != nil {
		go h.OnAssembleDone(takeID, trackID, assembledPath)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":        "assembled",
		"assembledPath": assembledPath,
		"totalBytes":    totalBytes,
		"sha256":        overallHash,
	})
}

// registerTrack creates server-owned identifiers; clients cannot overwrite an existing source.
func (h *Handler) registerTrack(w http.ResponseWriter, r *http.Request, takeID string) {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := signaling.VerifyUploadToken(auth, h.Config.SecretKey)
	if err != nil {
		http.Error(w, "participant authorization required", 401)
		return
	}
	tk, err := h.Store.GetTake(takeID)
	if err != nil {
		http.Error(w, "take not found", 404)
		return
	}
	part, err := h.Store.GetParticipant(claims.ParticipantID)
	if err != nil || part.Status != "admitted" || part.SessionID != claims.SessionID || tk.SessionID != claims.SessionID {
		http.Error(w, "not admitted to this take", 403)
		return
	}
	// A producer is in the call but never recorded [SL-P1-11].
	if part.Role == signaling.RoleProducer {
		http.Error(w, "a producer is not recorded", http.StatusForbidden)
		return
	}
	if tk.Status != "recording" {
		http.Error(w, "take is not recording", 409)
		return
	}
	var req struct {
		Kind  string `json:"kind"`
		Mime  string `json:"mime"`
		Label string `json:"label"`
		// Source is "screen" for a screen-share recording (video, with the
		// shared tab/system audio muxed when present). It may be registered
		// at any point while the take is recording.
		Source string `json:"source,omitempty"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req) != nil || (req.Kind != "audio" && req.Kind != "video") || !(strings.HasPrefix(req.Mime, req.Kind+"/webm") || strings.HasPrefix(req.Mime, req.Kind+"/mp4") || req.Mime == "audio/wav") {
		http.Error(w, "unsupported recording metadata", 400)
		return
	}
	if (req.Source != "" && req.Source != "screen") || (req.Source == "screen" && req.Kind != "video") {
		http.Error(w, "unsupported recording source", 400)
		return
	}
	req.Label = signaling.SanitizeDisplayText(req.Label, 120)
	// Count-then-create is serialized so concurrent registrations cannot
	// slip past the per-participant cap [SL-SEC-1].
	h.regMu.Lock()
	defer h.regMu.Unlock()
	if n, err := h.Store.CountParticipantTracks(takeID, part.ID); err != nil {
		http.Error(w, "cannot check track limit", 500)
		return
	} else if n >= MaxTracksPerParticipantTake {
		slog.Warn("Track registration refused: per-participant cap", "takeId", takeID, "participantId", part.ID, "tracks", n)
		http.Error(w, fmt.Sprintf("this device already registered %d recording tracks in this take, the host's limit", n), http.StatusTooManyRequests)
		return
	}
	sourceID := "src_" + uuid.NewString()
	trackID := "trk_" + uuid.NewString()
	kind := "camera"
	if req.Kind == "audio" {
		kind = "mic"
	} else if req.Source == "screen" {
		kind = "screen"
	}
	if err := h.Store.CreateSource(&store.Source{ID: sourceID, ParticipantID: part.ID, Kind: kind, Label: req.Label}); err != nil {
		http.Error(w, "cannot create source", 500)
		return
	}
	tr := &store.Track{ID: trackID, TakeID: takeID, SourceID: sourceID, Kind: req.Kind, Mime: req.Mime, LastSeq: -1, Status: "recording"}
	if err := h.Store.CreateTrack(tr); err != nil {
		http.Error(w, "cannot create track", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(tr)
}
