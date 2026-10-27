package livestream

// Integration tests against real ffmpeg [SL-P3-3]. They skip when no ffmpeg
// with h264_videotoolbox, lavfi (to make a source) and an RTMP listener is
// available. STUDIO_FFMPEG_DIR selects a build (e.g. the bundled LGPL one in
// build/media-tools/bin); the source clip may come from any ffmpeg on PATH.
// Nothing here talks to an external service: every sink is on 127.0.0.1.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type ffTools struct{ ffmpeg, ffprobe string }

func findFFmpeg(t *testing.T) ffTools {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	var ff ffTools
	if dir := os.Getenv("STUDIO_FFMPEG_DIR"); dir != "" {
		ff = ffTools{filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "ffprobe")}
	} else {
		var err error
		if ff.ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
			t.Skip("ffmpeg not found")
		}
		if ff.ffprobe, err = exec.LookPath("ffprobe"); err != nil {
			t.Skip("ffprobe not found")
		}
	}
	out, err := exec.Command(ff.ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil || !bytes.Contains(out, []byte("h264_videotoolbox")) {
		t.Skip("ffmpeg without h264_videotoolbox")
	}
	return ff
}

// sourceClip makes an H.264/AAC FLV test clip with any ffmpeg that has lavfi.
func sourceClip(t *testing.T, ff ffTools, seconds int) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "src.flv")
	candidates := []string{ff.ffmpeg}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		candidates = append(candidates, p)
	}
	for _, bin := range candidates {
		cmd := exec.Command(bin, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-t", strconv.Itoa(seconds), "-c:v", "h264_videotoolbox", "-b:v", "1M", "-g", "60", "-c:a", "aac", "-f", "flv", out)
		if cmd.Run() == nil {
			return out
		}
	}
	t.Skip("no ffmpeg with lavfi to make a source clip")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type sink struct {
	cmd  *exec.Cmd
	out  string
	logs *bytes.Buffer
	done chan struct{}
}

// startSink runs an ffmpeg RTMP listener that accepts exactly one publish
// to /live/<key> and records it.
func startSink(t *testing.T, ff ffTools, port int, key, out string) *sink {
	t.Helper()
	s := &sink{out: out, logs: &bytes.Buffer{}, done: make(chan struct{})}
	s.cmd = exec.Command(ff.ffmpeg, "-hide_banner", "-loglevel", "error", "-listen", "1",
		"-i", "rtmp://127.0.0.1:"+strconv.Itoa(port)+"/live/"+key, "-c", "copy", "-y", out)
	s.cmd.Stderr = s.logs
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.cmd.Wait(); close(s.done) }()
	t.Cleanup(func() { _ = s.cmd.Process.Kill(); <-s.done })
	// ffmpeg -listen accepts one connection, so it can't be probed; give it
	// time to bind.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output(); len(out) > 0 {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

func (s *sink) wait(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(d):
		t.Fatalf("sink did not finish; logs: %s", s.logs)
	}
}

type probe struct {
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		SampleRate string `json:"sample_rate"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func probeFile(t *testing.T, ff ffTools, path string) (p probe, duration float64) {
	t.Helper()
	out, err := exec.Command(ff.ffprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	duration, _ = strconv.ParseFloat(p.Format.Duration, 64)
	return p, duration
}

func assertH264AAC(t *testing.T, ff ffTools, path string, minSec float64) float64 {
	t.Helper()
	p, d := probeFile(t, ff, path)
	var v, a bool
	for _, s := range p.Streams {
		v = v || (s.CodecType == "video" && s.CodecName == "h264" && s.Width == 1280 && s.Height == 720)
		a = a || (s.CodecType == "audio" && s.CodecName == "aac" && s.SampleRate == "48000")
	}
	if !v || !a || d < minSec {
		t.Fatalf("%s: want h264 1280x720 + aac 48k >= %.1fs, got %+v (%.2fs)", filepath.Base(path), minSec, p.Streams, d)
	}
	// Decodes end to end.
	if out, err := exec.Command(ff.ffmpeg, "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("decode %s: %v %s", path, err, out)
	}
	return d
}

// testCA makes a self-signed certificate for 127.0.0.1 and the pool that
// trusts it. Only tests trust it: production uses the system store.
func testCA(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "studiolink-test-ingest"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// tlsProxy terminates TLS on 127.0.0.1 and forwards plain TCP to backend:
// a local stand-in for an rtmps:// ingest.
func tlsProxy(t *testing.T, cert tls.Certificate, backend int) int {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if err := c.(*tls.Conn).Handshake(); err != nil {
					return
				}
				b, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(backend))
				if err != nil {
					return
				}
				defer b.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(b, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, b); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// pacedSource feeds a file to Ingest in chunks, spreading it over d.
type pacedSource struct {
	data   []byte
	chunk  int
	every  time.Duration
	pos    int
	closed chan struct{}
	once   sync.Once
}

func newPacedSource(t *testing.T, path string, d time.Duration) *pacedSource {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const chunk = 32 * 1024
	n := max(1, len(data)/chunk)
	return &pacedSource{data: data, chunk: chunk, every: d / time.Duration(n), closed: make(chan struct{})}
}

func (p *pacedSource) next() ([]byte, error) {
	if p.pos >= len(p.data) {
		return nil, io.EOF
	}
	select {
	case <-p.closed:
		return nil, io.EOF
	case <-time.After(p.every):
	}
	end := min(p.pos+p.chunk, len(p.data))
	b := p.data[p.pos:end]
	p.pos = end
	return b, nil
}

func (p *pacedSource) abort() { p.once.Do(func() { close(p.closed) }) }

func waitStatus(t *testing.T, m *Manager, sess, what string, d time.Duration, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		st, _ := m.Status(sess)
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func syncLogger(buf *lockedBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

const integrationKey = "sl-it-SECRET-key-5b9e"

// TestIntegrationRTMPSPublishKeyNotInArgv publishes the real encoder's
// output over rtmps:// (Go crypto/tls, certificate verified against a
// test-only root) to a local TLS-terminating proxy in front of an ffmpeg
// RTMP listener, and checks the encoder's argv in `ps` never shows the key.
func TestIntegrationRTMPSPublishKeyNotInArgv(t *testing.T) {
	ff := findFFmpeg(t)
	src := sourceClip(t, ff, 10)
	cert, pool := testCA(t)
	sinkPort := freePort(t)
	out := filepath.Join(t.TempDir(), "rtmps.flv")
	sk := startSink(t, ff, sinkPort, integrationKey, out)
	proxyPort := tlsProxy(t, cert, sinkPort)

	logs := &lockedBuffer{}
	var pids []int
	var pidMu sync.Mutex
	exec := ExecLauncher(ff.ffmpeg)
	launch := func(argv []string) (*Process, error) {
		p, err := exec(argv)
		if err == nil {
			pidMu.Lock()
			pids = append(pids, p.Pid)
			pidMu.Unlock()
		}
		return p, err
	}
	m := NewManager(launch, Options{Logger: syncLogger(logs), TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
	if _, err := m.Start("it", StartRequest{Preset: "720p30", Destinations: []Destination{
		{Name: "Local RTMPS", URL: "rtmps://127.0.0.1:" + strconv.Itoa(proxyPort) + "/live", Key: integrationKey},
	}}); err != nil {
		t.Fatal(err)
	}
	src1 := newPacedSource(t, src, 6*time.Second)
	ingestDone := make(chan error, 1)
	go func() { ingestDone <- m.Ingest("it", src1.next, src1.abort) }()

	st := waitStatus(t, m, "it", "destination live", 20*time.Second, func(s Status) bool {
		return len(s.Destinations) == 1 && s.Destinations[0].State == DestLive
	})
	if st.Destinations[0].Server != "rtmps://127.0.0.1:"+strconv.Itoa(proxyPort) {
		t.Fatalf("server label %q", st.Destinations[0].Server)
	}
	// The encoder is running: its argv must hold neither key nor destination.
	pidMu.Lock()
	pid := pids[0]
	pidMu.Unlock()
	args, err := psArgs(pid)
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	if !strings.Contains(args, "h264_videotoolbox") {
		t.Fatalf("ps did not show the encoder argv: %q", args)
	}
	for _, leak := range []string{integrationKey, "rtmp", strconv.Itoa(proxyPort)} {
		if strings.Contains(args, leak) {
			t.Fatalf("encoder argv shows %q: %q", leak, args)
		}
	}
	if err := <-ingestDone; err != nil {
		t.Fatal(err)
	}
	st, _ = m.Status("it")
	if st.Destinations[0].BytesOut < 500_000 {
		t.Fatalf("too little sent: %+v", st.Destinations[0])
	}
	if _, err := m.Stop("it"); err != nil {
		t.Fatal(err)
	}
	if !m.Wait("it", 5*time.Second) {
		t.Fatal("publisher did not exit")
	}
	sk.wait(t, 10*time.Second)
	assertH264AAC(t, ff, out, 8)
	if strings.Contains(logs.String(), integrationKey) {
		t.Fatal("logs contain the stream key")
	}
}

func psArgs(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out)), err
}

// TestIntegrationRTMPSRejectsUntrustedCertificate: with the production trust
// store (TLS nil), a self-signed ingest is refused, never streamed to.
func TestIntegrationRTMPSRejectsUntrustedCertificate(t *testing.T) {
	cert, _ := testCA(t)
	// The backend is never reached; any closed port will do.
	proxyPort := tlsProxy(t, cert, freePort(t))
	target, err := parseTarget(Destination{URL: "rtmps://127.0.0.1:" + strconv.Itoa(proxyPort) + "/live", Key: integrationKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dialRTMP(context.Background(), target, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted certificate accepted or wrong error: %v", err)
	}
	// Hostname verification: the test root is trusted, but not for this name.
	_, pool := testCA(t)
	target.host = "ingest.example.test"
	if _, err := dialRTMP(context.Background(), target, &tls.Config{RootCAs: pool}, time.Second); err == nil {
		t.Fatal("certificate for the wrong host accepted")
	}
}

// TestIntegrationReconnectAfterSinkRestart kills one destination's sink
// mid-stream and restarts it: that destination reconnects with backoff and
// resumes at a keyframe, and the other destination never notices.
func TestIntegrationReconnectAfterSinkRestart(t *testing.T) {
	ff := findFFmpeg(t)
	src := sourceClip(t, ff, 16)
	dir := t.TempDir()
	portA, portB := freePort(t), freePort(t)
	sinkA := startSink(t, ff, portA, integrationKey, filepath.Join(dir, "a.flv"))
	sinkB := startSink(t, ff, portB, integrationKey, filepath.Join(dir, "b1.flv"))

	logs := &lockedBuffer{}
	m := NewManager(ExecLauncher(ff.ffmpeg), Options{Logger: syncLogger(logs), BackoffBase: 300 * time.Millisecond, BackoffMax: time.Second, MaxAttempts: 20})
	if _, err := m.Start("rc", StartRequest{Preset: "720p30", Destinations: []Destination{
		{Name: "Stays up", URL: "rtmp://127.0.0.1:" + strconv.Itoa(portA) + "/live", Key: integrationKey},
		{Name: "Flaky", URL: "rtmp://127.0.0.1:" + strconv.Itoa(portB) + "/live", Key: integrationKey},
	}}); err != nil {
		t.Fatal(err)
	}
	feed := newPacedSource(t, src, 14*time.Second)
	ingestDone := make(chan error, 1)
	go func() { ingestDone <- m.Ingest("rc", feed.next, feed.abort) }()
	waitStatus(t, m, "rc", "both live", 20*time.Second, func(s Status) bool {
		return s.Destinations[0].State == DestLive && s.Destinations[1].State == DestLive
	})
	time.Sleep(time.Second)
	_ = sinkB.cmd.Process.Kill()
	<-sinkB.done
	st := waitStatus(t, m, "rc", "flaky reconnecting", 10*time.Second, func(s Status) bool {
		return s.Destinations[1].State == DestReconnecting
	})
	if st.Destinations[0].State != DestLive {
		t.Fatalf("healthy destination disturbed: %+v", st.Destinations[0])
	}
	if !strings.Contains(strings.Join(st.Warnings, "\n"), "Flaky: reconnecting") || st.Destinations[1].Error == "" {
		t.Fatalf("no reconnect warning/reason: %+v", st)
	}
	if strings.Contains(st.Destinations[1].Error, integrationKey) {
		t.Fatal("error leaks the key")
	}
	time.Sleep(time.Second)
	b2 := filepath.Join(dir, "b2.flv")
	sinkB2 := startSink(t, ff, portB, integrationKey, b2)
	st = waitStatus(t, m, "rc", "flaky live again", 15*time.Second, func(s Status) bool {
		return s.Destinations[1].State == DestLive && s.Destinations[1].Reconnects > 0
	})
	if st.Destinations[0].Reconnects != 0 || st.Destinations[0].State != DestLive {
		t.Fatalf("healthy destination reconnected: %+v", st.Destinations[0])
	}
	time.Sleep(2 * time.Second)
	if _, err := m.Stop("rc"); err != nil {
		t.Fatal(err)
	}
	<-ingestDone
	if !m.Wait("rc", 5*time.Second) {
		t.Fatal("publishers did not exit")
	}
	sinkA.wait(t, 10*time.Second)
	sinkB2.wait(t, 10*time.Second)
	dA := assertH264AAC(t, ff, filepath.Join(dir, "a.flv"), 6)
	dB := assertH264AAC(t, ff, b2, 1.5)
	if dB >= dA {
		t.Fatalf("restarted sink got %.1fs, healthy one %.1fs", dB, dA)
	}
	if strings.Contains(logs.String(), integrationKey) {
		t.Fatal("logs contain the stream key")
	}
}
