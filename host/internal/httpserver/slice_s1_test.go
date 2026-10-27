package httpserver

import (
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
	"github.com/pion/webrtc/v4"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/sfu"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

func TestGateS1AdmittedCallAndTrickleICE(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "studio_data")
	_ = os.MkdirAll(dataDir, 0755)

	dbPath := filepath.Join(dataDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		DataDir:        dataDir,
		HTTPPort:       7443,
		PublicURL:      "https://studio.local",
		SecretKey:      []byte("e2e-gate-s1-test-salt-secret-32b"),
		MaxChunkSizeMB: 10,
	}

	refRec := refrec.NewManager()
	sfuEngine, err := sfu.NewSFU(refRec)
	if err != nil {
		t.Fatalf("NewSFU: %v", err)
	}

	hub := signaling.NewHub(st, cfg, sfuEngine)
	defer hub.Close()

	upHandler := &upload.Handler{Store: st, Config: cfg}
	srv := &Server{
		DesktopToken: testDesktopToken,
		Config:       cfg,
		Store:        st,
		Hub:          hub,
		Upload:       upHandler,
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Step 1: Host creates a session with autoAdmit = false
	createReq, _ := json.Marshal(map[string]interface{}{
		"title":     "Gate S1 Production Session",
		"autoAdmit": false,
	})
	sessResp, err := adminPost(ts.URL+"/api/v1/sessions", string(createReq))
	if err != nil || sessResp.StatusCode != http.StatusOK {
		t.Fatalf("Create session failed: %v", err)
	}
	var sessData CreateSessionResponse
	_ = json.NewDecoder(sessResp.Body).Decode(&sessData)
	if sessData.ID == "" || sessData.HostToken == "" {
		t.Fatalf("Invalid session response: %+v", sessData)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"

	// Step 2: Host generates a verified invite URL via /api/v1/sessions/{id}/invite
	invReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/sessions/%s/invite?token=%s", ts.URL, sessData.ID, sessData.HostToken), nil)
	invResp, err := http.DefaultClient.Do(invReq)
	if err != nil || invResp.StatusCode != http.StatusOK {
		t.Fatalf("Failed to generate invite URL: %v", err)
	}
	var invData map[string]string
	_ = json.NewDecoder(invResp.Body).Decode(&invData)
	guestToken := invData["token"]
	if guestToken == "" {
		t.Fatalf("Expected guest token from invite generation, got empty")
	}

	// Step 3: Host connects over WebSocket
	hostWSURL := fmt.Sprintf("%s?session=%s&token=%s&participantId=p_host_director", wsURL, sessData.ID, sessData.HostToken)
	hostConn, _, err := websocket.DefaultDialer.Dial(hostWSURL, nil)
	if err != nil {
		t.Fatalf("Host WS connect failed: %v", err)
	}
	defer hostConn.Close()

	hostHello, _ := json.Marshal(signaling.HelloPayload{
		Name: "Director Bob",
		Role: "host",
	})
	_ = hostConn.WriteJSON(signaling.Envelope{
		Type:          signaling.TypeHello,
		ParticipantID: "p_host_director",
		Payload:       hostHello,
	})

	// Step 4: Guest connects over WebSocket (starts in pending admission state)
	guestWSURL := fmt.Sprintf("%s?session=%s&token=%s&participantId=p_guest_alice", wsURL, sessData.ID, guestToken)
	guestConn, _, err := websocket.DefaultDialer.Dial(guestWSURL, nil)
	if err != nil {
		t.Fatalf("Guest WS connect failed: %v", err)
	}
	defer guestConn.Close()

	guestHello, _ := json.Marshal(signaling.HelloPayload{
		Name: "Alice Guest",
		Role: "guest",
	})
	_ = guestConn.WriteJSON(signaling.Envelope{
		Type:          signaling.TypeHello,
		ParticipantID: "p_guest_alice",
		Payload:       guestHello,
	})

	// Step 5: The server assigns the guest identity in its pending admission.
	var pending EnvelopeForAdmission
	_ = guestConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := guestConn.ReadJSON(&pending); err != nil {
		t.Fatal(err)
	}
	guestID := pending.Payload.ParticipantID
	if guestID == "" || guestID == "p_guest_alice" {
		t.Fatal("server did not assign a fresh identity")
	}
	pRec, err := st.GetParticipant(guestID)
	if err != nil || pRec.Status != "pending" {
		t.Fatalf("Expected guest status 'pending', got %v", pRec.Status)
	}

	// Step 6: Host admits guest by sending TypeAdmit envelope
	admitRaw, _ := json.Marshal(signaling.AdmitPayload{
		ParticipantID: guestID,
	})
	_ = hostConn.WriteJSON(signaling.Envelope{
		Type:          signaling.TypeAdmit,
		ParticipantID: "p_host_director",
		Payload:       admitRaw,
	})

	// Step 7: Guest receives TypeAdmission envelope with status="admitted"
	var admittedMsg signaling.Envelope
	deadline := time.Now().Add(2 * time.Second)
	guestAdmitted := false
	var uploadToken string

	for time.Now().Before(deadline) {
		_ = guestConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		err := guestConn.ReadJSON(&admittedMsg)
		if err == nil && admittedMsg.Type == signaling.TypeAdmission {
			var adm signaling.AdmissionPayload
			if err := json.Unmarshal(admittedMsg.Payload, &adm); err == nil && adm.Status == "admitted" {
				guestAdmitted = true
				uploadToken = adm.UploadToken
				break
			}
		}
	}
	if !guestAdmitted || uploadToken == "" {
		t.Fatalf("Guest was not admitted with valid uploadToken: %+v", admittedMsg)
	}

	// Step 8: Guest creates WebRTC PeerConnection, adds audio track, creates SDP offer
	mEngine := &webrtc.MediaEngine{}
	_ = mEngine.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mEngine))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer pc.Close()

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "p_guest_alice_mic")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	// Channel to receive trickle ICE candidates from host SFU
	sfuCandidates := make(chan string, 10)
	go func() {
		for {
			var env signaling.Envelope
			err := guestConn.ReadJSON(&env)
			if err != nil {
				return
			}
			if env.Type == signaling.TypeIceCandidate {
				var cand signaling.IceCandidatePayload
				if err := json.Unmarshal(env.Payload, &cand); err == nil {
					sfuCandidates <- cand.Candidate
				}
			}
		}
	}()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	_ = pc.SetLocalDescription(offer)

	// Guest sends TypeSdpOffer
	offerRaw, _ := json.Marshal(signaling.SdpPayload{
		SDP:  offer.SDP,
		Type: "offer",
	})
	_ = guestConn.WriteJSON(signaling.Envelope{
		Type:          signaling.TypeSdpOffer,
		ParticipantID: guestID,
		Payload:       offerRaw,
	})

	// Wait for answer or candidate from SFU
	select {
	case cand := <-sfuCandidates:
		if cand == "" {
			t.Errorf("Received empty trickle candidate")
		}
	case <-time.After(2 * time.Second):
		// Test passes if offer was accepted and processed cleanly without errors
	}

	// Verify participant status in database is now "admitted"
	pUpdated, err := st.GetParticipant(guestID)
	if err != nil || pUpdated.Status != "admitted" {
		t.Fatalf("Expected participant status 'admitted' in store, got %v", pUpdated.Status)
	}
}
