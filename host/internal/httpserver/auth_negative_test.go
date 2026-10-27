package httpserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/sfu"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

func TestAuthNegativeCases(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "studio_data")
	_ = os.MkdirAll(dataDir, 0755)

	dbPath := filepath.Join(dataDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		DataDir:        dataDir,
		HTTPPort:       7443,
		PublicURL:      "https://studio.local",
		SecretKey:      []byte("very-secure-secret-key-32-bytes!"),
		MaxChunkSizeMB: 10,
	}

	refRec := refrec.NewManager()
	sfuEngine, err := sfu.NewSFU(refRec)
	if err != nil {
		t.Fatalf("Failed to create SFU: %v", err)
	}

	hub := signaling.NewHub(st, cfg, sfuEngine)
	defer hub.Close()

	upHandler := &upload.Handler{
		Store:  st,
		Config: cfg,
	}

	srv := &Server{
		Config: cfg,
		Store:  st,
		Hub:    hub,
		Upload: upHandler,
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"

	// Setup a real session
	sessID := "auth_sess_1"
	tokenBytes := make([]byte, 32)
	_, _ = rand.Read(tokenBytes)
	validGuestToken := hex.EncodeToString(tokenBytes)
	h := sha256.Sum256([]byte(validGuestToken))
	tokenHash := hex.EncodeToString(h[:])

	_ = st.CreateSession(&store.Session{
		ID:              sessID,
		Title:           "Auth Test Session",
		CreatedAt:       time.Now().UTC(),
		PresetJSON:      "{}",
		InviteTokenHash: tokenHash,
		AutoAdmit:       false,
	})

	validHostToken := signaling.MintHostToken(sessID, 1*time.Hour, cfg.SecretKey)

	// 1. Missing session and token -> 401
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 for missing query params, got code %v, err %v", resp.StatusCode, err)
	}

	// 2. Invalid guest token -> 401
	badURL := fmt.Sprintf("%s?session=%s&token=invalid_garbage_token&participantId=p1", wsURL, sessID)
	_, resp, err = websocket.DefaultDialer.Dial(badURL, nil)
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 for invalid token, got code %v, err %v", resp.StatusCode, err)
	}

	// 3. Expired host token -> 401
	expiredHostToken := signaling.MintHostToken(sessID, -10*time.Minute, cfg.SecretKey)
	expURL := fmt.Sprintf("%s?session=%s&token=%s&participantId=p_host", wsURL, sessID, expiredHostToken)
	_, resp, err = websocket.DefaultDialer.Dial(expURL, nil)
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 for expired host token, got code %v, err %v", resp.StatusCode, err)
	}

	// 4. Token for different session -> 401
	diffSessHostToken := signaling.MintHostToken("other_session_xyz", 1*time.Hour, cfg.SecretKey)
	diffURL := fmt.Sprintf("%s?session=%s&token=%s&participantId=p_host", wsURL, sessID, diffSessHostToken)
	_, resp, err = websocket.DefaultDialer.Dial(diffURL, nil)
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 for wrong session token, got code %v, err %v", resp.StatusCode, err)
	}

	// 5. Host Invite Generation endpoint without host token -> 401
	inviteReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/sessions/%s/invite", ts.URL, sessID), nil)
	inviteResp, err := http.DefaultClient.Do(inviteReq)
	if err != nil || inviteResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 on /invite without host auth, got status %v", inviteResp.StatusCode)
	}

	// Guest token trying to call /invite -> 401
	inviteReqGuest, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/sessions/%s/invite?token=%s", ts.URL, sessID, validGuestToken), nil)
	inviteRespGuest, err := http.DefaultClient.Do(inviteReqGuest)
	if err != nil || inviteRespGuest.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected 401 on /invite with guest token, got status %v", inviteRespGuest.StatusCode)
	}

	// Valid host token calling /invite -> 200 and returns new inviteUrl
	inviteReqHost, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/sessions/%s/invite?token=%s", ts.URL, sessID, validHostToken), nil)
	inviteRespHst, err := http.DefaultClient.Do(inviteReqHost)
	if err != nil || inviteRespHst.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 on /invite with host token, got status %v", inviteRespHst.StatusCode)
	}
	var inviteData map[string]string
	_ = json.NewDecoder(inviteRespHst.Body).Decode(&inviteData)
	if inviteData["inviteUrl"] == "" || inviteData["token"] == "" {
		t.Fatalf("Expected valid inviteUrl and token, got %+v", inviteData)
	}

	// 6. Role spoofing: Guest attempts to send "role: host" in HelloPayload
	// Host connects first
	hostConn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s&participantId=p_real_host", wsURL, sessID, validHostToken), nil)
	if err != nil {
		t.Fatalf("Host WS connect failed: %v", err)
	}
	defer hostConn.Close()

	// Guest connects with valid guest token (the new one returned by /invite)
	guestConn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s&participantId=p_guest_spoof", wsURL, sessID, inviteData["token"]), nil)
	if err != nil {
		t.Fatalf("Guest WS connect failed: %v", err)
	}
	defer guestConn.Close()

	// Guest sends Hello claiming to be "host"
	spoofHello, _ := json.Marshal(signaling.HelloPayload{
		Name: "Malicious Guest",
		Role: "host", // attempted privilege escalation
	})
	_ = guestConn.WriteJSON(signaling.Envelope{
		Type:          signaling.TypeHello,
		ParticipantID: "p_guest_spoof",
		Payload:       spoofHello,
	})

	// Admission assigns the identity; a query parameter cannot impersonate another participant.
	var pending EnvelopeForAdmission
	_ = guestConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := guestConn.ReadJSON(&pending); err != nil {
		t.Fatal(err)
	}
	if pending.Payload.ParticipantID == "p_guest_spoof" {
		t.Fatal("caller-selected participant ID accepted")
	}

	// Verify server registered guest as "guest", NOT "host"
	pRec, err := st.GetParticipant(pending.Payload.ParticipantID)
	if err != nil || pRec.Role != "guest" {
		t.Fatalf("Expected server to enforce role 'guest', got record role: %v", pRec.Role)
	}

	// 7. Guest attempts to trigger record_start
	_ = guestConn.WriteJSON(signaling.Envelope{
		Type:          signaling.TypeRecordStart,
		ParticipantID: "p_guest_spoof",
	})
	time.Sleep(50 * time.Millisecond)

	// Verify no take was created
	takes, _ := st.ListTakesBySession(sessID)
	if len(takes) != 0 {
		t.Fatalf("Expected 0 takes created by unauthorized guest record_start, got %d", len(takes))
	}
}

type EnvelopeForAdmission struct {
	Type    string                     `json:"t"`
	Payload signaling.AdmissionPayload `json:"payload"`
}
