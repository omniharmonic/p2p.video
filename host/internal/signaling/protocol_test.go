package signaling

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestProtocolFixturesRoundTrip decodes each fixture into its Go payload type
// with unknown fields rejected, re-encodes it and checks the JSON is unchanged,
// so protocol.ts and testdata/protocol stay in step with protocol.go.
func TestProtocolFixturesRoundTrip(t *testing.T) {
	cases := map[string]struct {
		typ string
		new func() any
	}{
		"chat.json":             {TypeChat, func() any { return &ChatMessage{} }},
		"chat-history.json":     {TypeChatHistory, func() any { return &ChatHistoryPayload{} }},
		"hand.json":             {TypeHand, func() any { return &HandPayload{} }},
		"reaction.json":         {TypeReaction, func() any { return &ReactionPayload{} }},
		"roster.json":           {TypeRoster, func() any { return &RosterPayload{} }},
		"room-state.json":       {TypeRoomState, func() any { return &RoomStatePayload{} }},
		"mute-request.json":     {TypeMuteRequest, func() any { return &MuteRequestPayload{} }},
		"remove.json":           {TypeRemove, func() any { return &RemovePayload{} }},
		"room-lock.json":        {TypeRoomLock, func() any { return &RoomLockPayload{} }},
		"action-error.json":     {TypeActionError, func() any { return &ActionErrorPayload{} }},
		"admission-denied.json": {TypeAdmission, func() any { return &AdmissionPayload{} }},
		"admission-busy.json":   {TypeAdmission, func() any { return &AdmissionPayload{} }},
		// Phone camera [SL-P1-9]
		"admission-subsource.json": {TypeAdmission, func() any { return &AdmissionPayload{} }},
		"roster-subsource.json":    {TypeRoster, func() any { return &RosterPayload{} }},
		"participant-state.json":   {TypeParticipantState, func() any { return &ParticipantStatePayload{} }},
		"source-add.json":          {TypeSourceAdd, func() any { return &SourceAddPayload{} }},
		"source-remove.json":       {TypeSourceRemove, func() any { return &SourceRemovePayload{} }},
		"screen-stop.json":         {TypeScreenStop, func() any { return &ScreenStopPayload{} }},
		"sdp-renegotiate.json":     {TypeSdpOffer, func() any { return &SdpPayload{} }},
		"session-end.json":         {TypeSessionEnd, func() any { return &SessionEndPayload{} }},
		"net-stats.json":           {TypeNetStats, func() any { return &NetStatsPayload{} }},
		// Live captions [SL-P4-2]
		"caption.json":         {TypeCaption, func() any { return &CaptionPayload{} }},
		"caption-state.json":   {TypeCaptionState, func() any { return &CaptionStatePayload{} }},
		"caption-control.json": {TypeCaptionControl, func() any { return &CaptionControlPayload{} }},
		// Recording mic mode [SL-P1-10]
		"preflight-recording-mic.json": {TypePreflight, func() any { return &PreflightPayload{} }},
		"room-state-mic.json":          {TypeRoomState, func() any { return &RoomStatePayload{} }},
		// Producer [SL-P1-11]
		"roster-producer.json":    {TypeRoster, func() any { return &RosterPayload{} }},
		"admission-producer.json": {TypeAdmission, func() any { return &AdmissionPayload{} }},
		// Teleprompter [SL-P5-1]
		"prompter.json":        {TypePrompter, func() any { return &PrompterPayload{} }},
		"prompter-hidden.json": {TypePrompter, func() any { return &PrompterPayload{} }},
		"prompter-edit.json":   {TypePrompterEdit, func() any { return &PrompterEditPayload{} }},
		"prompter-scroll.json": {TypePrompterScroll, func() any { return &PrompterScrollPayload{} }},
		// Branding and lower thirds [SL-P5-2]
		"branding.json":    {TypeBranding, func() any { return &BrandingPayload{} }},
		"lower-third.json": {TypeLowerThird, func() any { return &LowerThirdPayload{} }},
	}
	for file, tc := range cases {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "protocol", file))
			if err != nil {
				t.Fatal(err)
			}
			var env Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			if env.Type != tc.typ {
				t.Fatalf("type %q, want %q", env.Type, tc.typ)
			}
			dec := json.NewDecoder(bytes.NewReader(env.Payload))
			dec.DisallowUnknownFields()
			v := tc.new()
			if err := dec.Decode(v); err != nil {
				t.Fatal(err)
			}
			again, _ := json.Marshal(v)
			var want, got any
			_ = json.Unmarshal(env.Payload, &want)
			_ = json.Unmarshal(again, &got)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round trip changed payload:\n%s\n%s", env.Payload, again)
			}
		})
	}
}

func TestTokenMintAndVerify(t *testing.T) {
	secret := []byte("test-super-secret-key-123456789")
	partID := "p_alice"
	sessID := "s_ep1"

	token := MintUploadToken(partID, sessID, 1*time.Hour, secret)
	if token == "" {
		t.Fatal("MintUploadToken returned empty string")
	}

	claims, err := VerifyUploadToken(token, secret)
	if err != nil {
		t.Fatalf("VerifyUploadToken failed: %v", err)
	}
	if claims.ParticipantID != partID || claims.SessionID != sessID {
		t.Fatalf("Claims mismatch: got %+v", claims)
	}

	// Tampered token
	badToken := token + "xyz"
	if _, err := VerifyUploadToken(badToken, secret); err == nil {
		t.Error("Expected error on tampered token")
	}

	// Expired token
	expToken := MintUploadToken(partID, sessID, -1*time.Minute, secret)
	if _, err := VerifyUploadToken(expToken, secret); err == nil {
		t.Error("Expected error on expired token")
	}
}

func TestProtocolEnvelopes(t *testing.T) {
	req := HelloPayload{
		Name: "Alice",
		Role: "guest",
		Capabilities: map[string]bool{
			"webcodecs": true,
			"opfs":      true,
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	env := Envelope{
		Type:          TypeHello,
		ParticipantID: "p_1",
		Payload:       raw,
	}

	envBytes, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal env failed: %v", err)
	}

	var parsed Envelope
	if err := json.Unmarshal(envBytes, &parsed); err != nil {
		t.Fatalf("Unmarshal env failed: %v", err)
	}
	if parsed.Type != TypeHello || parsed.ParticipantID != "p_1" {
		t.Errorf("Unexpected parsed env: %+v", parsed)
	}
}
