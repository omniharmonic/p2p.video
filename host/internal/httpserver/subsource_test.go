package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// wsAdmission connects, says hello and returns the admission it receives.
func wsAdmission(t *testing.T, wsURL string, hello signaling.HelloPayload) (*websocket.Conn, signaling.AdmissionPayload) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %d: %v", code, err)
	}
	raw, _ := json.Marshal(hello)
	_ = conn.WriteJSON(signaling.Envelope{Type: signaling.TypeHello, Payload: raw})
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var env signaling.Envelope
		if err := conn.ReadJSON(&env); err != nil {
			t.Fatalf("read admission: %v", err)
		}
		if env.Type == signaling.TypeAdmission {
			var a signaling.AdmissionPayload
			_ = json.Unmarshal(env.Payload, &a)
			return conn, a
		}
	}
}

func dialStatus(wsURL string) int {
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		conn.Close()
		return http.StatusSwitchingProtocols
	}
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// Phone camera links [SL-P1-9]: minted only for an admitted participant (also
// through Funnel, like the guest page), spent once at the WebSocket, and the
// phone reconnects with its own grant.
func TestSubsourceLinkMintAndPhoneSocket(t *testing.T) {
	srv, hub, st, _, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
	invite := "guest-invite-secret"
	h := sha256.Sum256([]byte(invite))
	_ = st.CreateSession(&store.Session{ID: "sess", CreatedAt: time.Now(), PresetJSON: "{}", InviteTokenHash: hex.EncodeToString(h[:]), AutoAdmit: true})

	guestConn, adm := wsAdmission(t, wsBase+"?session=sess&token="+invite, signaling.HelloPayload{Name: "Ada", Role: "guest"})
	defer guestConn.Close()
	if adm.Status != "admitted" || adm.UploadToken == "" {
		t.Fatalf("guest admission %+v", adm)
	}
	mint := func(auth string, funnel bool) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/sessions/sess/subsource", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if funnel {
			req.Header.Set("Tailscale-Funnel-Request", "?1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for name, auth := range map[string]string{"none": "", "invite": invite, "host token": signaling.MintHostToken("sess", time.Hour, srv.Config.SecretKey), "foreign grant": signaling.MintUploadToken(adm.ParticipantID, "other", time.Hour, srv.Config.SecretKey)} {
		if resp := mint(auth, false); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s minted a phone link: %d", name, resp.StatusCode)
		}
	}
	resp := mint(adm.UploadToken, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint through Funnel: %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	var link signaling.SubsourceLinkResponse
	_ = json.NewDecoder(resp.Body).Decode(&link)
	if !strings.HasPrefix(link.Path, "/phone.html?") || link.BaseURL != "" || link.ExpiresAt <= time.Now().UnixMilli() || link.ExpiresAt > time.Now().Add(11*time.Minute).UnixMilli() {
		t.Fatalf("link %+v", link)
	}
	u, _ := url.Parse(link.Path)
	capability := u.Query().Get("token")

	// Without role=subsource the capability is not a guest invite.
	if code := dialStatus(wsBase + "?session=sess&token=" + url.QueryEscape(capability)); code != http.StatusUnauthorized {
		t.Fatalf("capability as guest token: %d", code)
	}
	phoneURL := wsBase + "?role=subsource&session=sess&token=" + url.QueryEscape(capability)
	phoneConn, phone := wsAdmission(t, phoneURL, signaling.HelloPayload{Name: "Mallory", Role: "host"})
	if phone.Status != "admitted" || phone.Role != signaling.RoleSubsource || phone.ParentParticipantID != adm.ParticipantID || phone.Name != "Ada · Phone" {
		t.Fatalf("phone admission %+v", phone)
	}
	// Single use.
	if code := dialStatus(phoneURL); code != http.StatusUnauthorized {
		t.Fatalf("capability reused: %d", code)
	}
	// A phone reconnects with its own grant.
	phoneConn.Close()
	again, back := wsAdmission(t, wsBase+"?role=subsource&session=sess&token="+url.QueryEscape(phone.UploadToken), signaling.HelloPayload{Role: signaling.RoleSubsource, ResumeToken: phone.UploadToken})
	defer again.Close()
	if back.Status != "admitted" || back.ParticipantID != phone.ParticipantID || back.Role != signaling.RoleSubsource {
		t.Fatalf("phone resume %+v", back)
	}
	// A phone cannot mint phones.
	if resp := mint(phone.UploadToken, false); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("phone minted a phone link: %d", resp.StatusCode)
	}
}
