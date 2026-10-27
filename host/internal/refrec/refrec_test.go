package refrec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
)

func TestOpusSamplesFromTOC(t *testing.T) {
	for _, tc := range []struct {
		payload []byte
		want    uint32
		ok      bool
	}{
		{[]byte{0x78}, 960, true},        // CELT FB 20 ms, one frame (WebRTC default)
		{[]byte{0x79}, 1920, true},       // CELT FB 20 ms, two frames
		{[]byte{0x7b, 0x03}, 2880, true}, // code 3, three 20 ms frames
		{[]byte{0x08}, 960, true},        // SILK NB 20 ms
		{[]byte{0x18}, 2880, true},       // SILK NB 60 ms
		{[]byte{0x60}, 480, true},        // hybrid SWB 10 ms
		{[]byte{0x7b}, 0, false},         // code 3 without a count byte
		{[]byte{0x7b, 0x07}, 0, false},   // 140 ms: more than Opus allows
		{nil, 0, false},
	} {
		got, ok := OpusSamples(tc.payload)
		if got != tc.want || ok != tc.ok {
			t.Errorf("OpusSamples(% x) = %d %v, want %d %v", tc.payload, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReferenceRecordsOnlyDuringTakeAndLogsEveryPacket(t *testing.T) {
	dir := t.TempDir()
	m := NewManager()
	var now int64 = 1000
	m.SetClock(func() int64 { return now })
	pkt := func(ssrc, ts uint32) *rtp.Packet {
		return &rtp.Packet{Header: rtp.Header{SSRC: ssrc, Timestamp: ts}, Payload: []byte{0x78, 0x01, 0x02}}
	}
	// Before the take: nothing is written.
	if err := m.WriteRTP("s", "ana", pkt(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := m.StartTake("s", dir); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		now += 20_000
		_ = m.WriteRTP("s", "ana", pkt(1, uint32(i*960)))
	}
	// A reconnect: new SSRC and RTP base, same recorder.
	_ = m.WriteRTP("s", "ana", pkt(2, 77))
	// Another session's packets and empty payloads are ignored.
	_ = m.WriteRTP("other", "ana", pkt(1, 0))
	_ = m.WriteRTP("s", "ana", &rtp.Packet{Header: rtp.Header{SSRC: 1}})
	// A participant the hub refuses (a producer) is never written, and Allow
	// is asked once per take.
	asked := 0
	m.Allow = func(_, p string) bool { asked++; return p != "pat" }
	_ = m.WriteRTP("s", "pat", pkt(3, 0))
	_ = m.WriteRTP("s", "pat", pkt(3, 960))
	if _, err := os.Stat(filepath.Join(dir, "pat.ogg")); err == nil || asked != 1 {
		t.Fatalf("producer reference written or Allow asked %d times", asked)
	}
	m.Allow = nil
	// A participant ID that is not a plain file name never creates a file.
	if err := m.WriteRTP("s", "../x", pkt(1, 0)); err == nil {
		t.Fatal("path-like participant id accepted")
	}
	m.StopTake("s")
	_ = m.WriteRTP("s", "ana", pkt(1, 9999)) // after the take

	pkts, err := ReadPacketLog(filepath.Join(dir, "ana.pkt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 6 || pkts[0].ArrivalUs != 21000 || pkts[4].RTPTime != 4*960 || pkts[5].SSRC != 2 || pkts[5].Samples != 960 {
		t.Fatalf("packet log %+v", pkts)
	}
	if info, err := os.Stat(filepath.Join(dir, "ana.ogg")); err != nil || info.Size() == 0 {
		t.Fatalf("ogg missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "x.ogg")); err == nil {
		t.Fatal("wrote outside the refs dir")
	}
	// A torn final record (crash mid-write) is dropped, not an error.
	f, _ := os.OpenFile(filepath.Join(dir, "ana.pkt"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write([]byte{1, 2, 3})
	_ = f.Close()
	if again, err := ReadPacketLog(filepath.Join(dir, "ana.pkt")); err != nil || len(again) != 6 {
		t.Fatalf("torn log: %d %v", len(again), err)
	}
}
