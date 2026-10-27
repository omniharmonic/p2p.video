package signaling

import "testing"

// A guest-controlled recorder state was once rendered as HTML in the host
// dashboard; the hub must only relay known states (security review C1).
func TestSanitizeRecorderStatusRejectsUnknownState(t *testing.T) {
	for _, state := range []string{`<img src=x onerror=alert(1)>`, "", "Recording", "done "} {
		if _, ok := SanitizeRecorderStatus(RecorderStatusPayload{TrackID: "trk_1", State: state}); ok {
			t.Errorf("state %q accepted", state)
		}
	}
	got, ok := SanitizeRecorderStatus(RecorderStatusPayload{TrackID: "trk_1", State: "uploading", UploadedBytes: -5, LastSeq: -9})
	if !ok || got.UploadedBytes != 0 || got.LastSeq != -1 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}
