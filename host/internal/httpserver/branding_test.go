package httpserver

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
)

// Branding [SL-P5-2]: logo validation (magic bytes, decoded header, type,
// size), served headers, brand settings, role enforcement and the live push.

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0xC0
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (h *roomsHarness) upload(path, token, ctype string, body []byte) (int, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.ts.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestValidateLogo(t *testing.T) {
	pngData := testPNG(t, 64, 32)
	cases := []struct {
		name, ctype string
		data        []byte
		status      int
	}{
		{"png", "image/png", pngData, 0},
		{"jpeg", "image/jpeg", testJPEG(t), 0},
		{"svg declared", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), http.StatusUnsupportedMediaType},
		{"svg as png", "image/png", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), http.StatusUnsupportedMediaType},
		{"html as jpeg", "image/jpeg", []byte(`<html><script>alert(1)</script></html>`), http.StatusUnsupportedMediaType},
		{"png declared jpeg", "image/jpeg", pngData, http.StatusUnsupportedMediaType},
		{"magic then garbage", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), []byte("<script>alert(1)</script>")...), http.StatusUnsupportedMediaType},
		{"gif", "image/gif", []byte("GIF89a\x01\x00\x01\x00"), http.StatusUnsupportedMediaType},
		{"no type", "", pngData, http.StatusUnsupportedMediaType},
		{"empty", "image/png", nil, http.StatusBadRequest},
		{"too wide", "image/png", testPNG(t, signaling.BrandLogoMaxDimension+1, 1), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		typ, err := validateLogo(tc.ctype, tc.data)
		if tc.status == 0 {
			if err != nil || typ != tc.ctype {
				t.Errorf("%s: rejected: %v", tc.name, err)
			}
			continue
		}
		le, ok := err.(*errLogo)
		if !ok || le.status != tc.status {
			t.Errorf("%s: got %v, want %d", tc.name, err, tc.status)
		}
	}
}

func TestBrandingRoutesLogoAndRoles(t *testing.T) {
	h := newRoomsHarness(t)
	sess := h.instantSession()
	base := "/api/v1/sessions/" + sess.ID + "/branding"

	// Defaults before anything is set.
	code, raw := h.do("GET", base, "", bearer(sess.HostToken))
	var got signaling.BrandSettings
	_ = json.Unmarshal(raw, &got)
	if code != 200 || got.Accent != signaling.DefaultBrandAccent || got.LowerThirdsMode != signaling.LowerThirdsSpeaker || got.Logo != nil {
		t.Fatalf("defaults %d %s", code, raw)
	}

	// The host is in the call: brand changes reach it live.
	host, _ := h.dial(sess.ID, sess.HostToken, nil)
	hello(t, host, "Host", "host")
	until(t, host, "initial branding", func(e signaling.Envelope) bool { return e.Type == signaling.TypeBranding })

	code, raw = h.do("PUT", base, `{"showName":"The ‮Long Take","accent":"#E07A5F","tileLabels":true,"lowerThirdsMode":"manual"}`, bearer(sess.HostToken))
	_ = json.Unmarshal(raw, &got)
	if code != 200 || got.ShowName != "The Long Take" || got.Accent != "#e07a5f" || !got.TileLabels || got.LowerThirdsMode != "manual" {
		t.Fatalf("put %d %s", code, raw)
	}
	var pushed signaling.BrandingPayload
	until(t, host, "branding push", func(e signaling.Envelope) bool {
		if e.Type != signaling.TypeBranding {
			return false
		}
		_ = json.Unmarshal(e.Payload, &pushed)
		return pushed.ShowName == "The Long Take"
	})
	if pushed.Accent != "#e07a5f" || !pushed.TileLabels || pushed.LowerThirdsMode != "manual" {
		t.Fatalf("pushed %+v", pushed)
	}
	for _, bad := range []string{`{"accent":"red"}`, `{"accent":"#fff"}`, `{"accent":"#000000","lowerThirdsMode":"always"}`, `{"accent":"#000000","extra":1}`} {
		if code, _ := h.do("PUT", base, bad, bearer(sess.HostToken)); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", bad, code)
		}
	}

	// Logo: upload, serve with the right headers, replace, reject, remove.
	pngData := testPNG(t, 120, 60)
	if code, raw := h.upload(base+"/logo", sess.HostToken, "image/png", pngData); code != 200 {
		t.Fatalf("upload %d %s", code, raw)
	} else if _ = json.Unmarshal(raw, &got); got.Logo == nil || got.Logo.Type != "image/png" || got.Logo.Size != len(pngData) || len(got.Logo.Version) != 16 {
		t.Fatalf("logo meta %s", raw)
	}
	until(t, host, "logo push", func(e signaling.Envelope) bool {
		var b signaling.BrandingPayload
		_ = json.Unmarshal(e.Payload, &b)
		return e.Type == signaling.TypeBranding && b.LogoVersion == got.Logo.Version
	})
	req, _ := http.NewRequest("GET", h.ts.URL+base+"/logo", nil)
	req.Header.Set("Authorization", "Bearer "+sess.HostToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(body, pngData) {
		t.Fatalf("serve logo %d (%d bytes)", resp.StatusCode, len(body))
	}
	for k, want := range map[string]string{"Content-Type": "image/png", "X-Content-Type-Options": "nosniff", "Content-Security-Policy": "default-src 'none'; sandbox"} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)
	if code, _ := h.upload(base+"/logo", sess.HostToken, "image/svg+xml", svg); code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg upload %d", code)
	}
	if code, _ := h.upload(base+"/logo", sess.HostToken, "image/png", svg); code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg disguised as png %d", code)
	}
	huge := append(append([]byte{}, pngData...), make([]byte, signaling.BrandLogoMaxBytes)...)
	if code, _ := h.upload(base+"/logo", sess.HostToken, "image/png", huge); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload %d", code)
	}
	// A rejected upload leaves the stored logo alone.
	if code, raw := h.do("GET", base, "", bearer(sess.HostToken)); code != 200 || !strings.Contains(string(raw), `"image/png"`) {
		t.Fatalf("logo lost after rejected uploads: %s", raw)
	}
	jpg := testJPEG(t)
	if code, raw := h.upload(base+"/logo", sess.HostToken, "image/jpeg", jpg); code != 200 || !strings.Contains(string(raw), "image/jpeg") {
		t.Fatalf("jpeg upload %d %s", code, raw)
	}

	// Negative: no credential, the guest invite, a participant grant, the
	// producer link and the desktop token alone are all refused (host token
	// required), for every method.
	prodTok := linkToken(t, h.producerLink(sess.ID, sess.HostToken, "rotate").URL)
	grant := signaling.MintUploadToken("p_guest", sess.ID, 0x7fffffff, h.cfg.SecretKey)
	for _, tok := range []string{"", sess.Token, prodTok, grant, testDesktopToken} {
		for _, tc := range []struct{ method, path, body string }{
			{"GET", base, ""}, {"PUT", base, `{"accent":"#000000"}`},
			{"GET", base + "/logo", ""}, {"POST", base + "/logo", string(pngData)}, {"DELETE", base + "/logo", ""},
		} {
			req, _ := http.NewRequest(tc.method, h.ts.URL+tc.path, strings.NewReader(tc.body))
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			req.Header.Set("Content-Type", "image/png")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s with %q = %d, want denied", tc.method, tc.path, tok[:min(8, len(tok))], resp.StatusCode)
			}
		}
	}
	if code, raw := h.do("GET", base, "", bearer(sess.HostToken)); !strings.Contains(string(raw), "image/jpeg") || code != 200 {
		t.Fatalf("a denied request changed the logo: %s", raw)
	}

	if code, raw := h.do("DELETE", base+"/logo", "", bearer(sess.HostToken)); code != 200 || strings.Contains(string(raw), `"logo"`) {
		t.Fatalf("delete %d %s", code, raw)
	}
	if code, _ := h.do("GET", base+"/logo", "", bearer(sess.HostToken)); code != http.StatusNotFound {
		t.Fatalf("deleted logo still served: %d", code)
	}
}

// A room's brand is managed from the room (admin credential) and used by
// every meeting opened from it.
func TestRoomBrandingAppliesToItsMeetings(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	if code, raw := h.do("PUT", "/api/v1/rooms/"+room.ID+"/branding", `{"showName":"The Podcast","accent":"#112233","tileLabels":false}`, nil); code != 200 {
		t.Fatalf("room put %d %s", code, raw)
	}
	if code, raw := h.upload("/api/v1/rooms/"+room.ID+"/branding/logo", testDesktopToken, "image/png", testPNG(t, 10, 10)); code != 200 {
		t.Fatalf("room logo %d %s", code, raw)
	}
	// Without the admin credential the room routes are refused.
	if code, _ := h.do("GET", "/api/v1/rooms/"+room.ID+"/branding", "", http.Header{"Authorization": {"Bearer nope"}}); code != http.StatusForbidden {
		t.Fatalf("room branding without admin = %d", code)
	}
	start := h.start(room.ID)
	code, raw := h.do("GET", "/api/v1/sessions/"+start.SessionID+"/branding", "", bearer(start.HostToken))
	var got signaling.BrandSettings
	_ = json.Unmarshal(raw, &got)
	if code != 200 || got.ShowName != "The Podcast" || got.Accent != "#112233" || got.Logo == nil {
		t.Fatalf("meeting brand %d %s", code, raw)
	}
	// Editing from the meeting edits the room.
	if code, _ := h.do("PUT", "/api/v1/sessions/"+start.SessionID+"/branding", `{"showName":"Ep 2","accent":"#112233"}`, bearer(start.HostToken)); code != 200 {
		t.Fatal(code)
	}
	if _, raw := h.do("GET", "/api/v1/rooms/"+room.ID+"/branding", "", nil); !strings.Contains(string(raw), `"Ep 2"`) {
		t.Fatalf("room brand %s", raw)
	}
}
