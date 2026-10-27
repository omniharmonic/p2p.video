package sfu

import "testing"

func TestSDPHasCodec(t *testing.T) {
	// A Firefox (no OpenH264) offer: VP8/VP9 video, Opus audio.
	firefox := "v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 109\r\na=rtpmap:109 opus/48000/2\r\nm=video 9 UDP/TLS/RTP/SAVPF 120 121\r\na=rtpmap:120 VP8/90000\r\na=rtpmap:121 VP9/90000\r\n"
	cases := []struct {
		sdp, mime string
		want      bool
	}{
		{firefox, "video/VP8", true},
		{firefox, "video/vp9", true},
		{firefox, "video/H264", false},
		{firefox, "audio/opus", true},
		// opus listed under audio does not make "video/opus" acceptable.
		{firefox, "video/opus", false},
		{"", "video/H264", true},
		// No video section yet: the SFU's own offer adds one, nothing refuses.
		{"v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\n", "video/H264", true},
		{firefox, "garbage", true},
	}
	for _, c := range cases {
		if got := sdpHasCodec(c.sdp, c.mime); got != c.want {
			t.Errorf("sdpHasCodec(%q) = %v, want %v", c.mime, got, c.want)
		}
	}
}
