package turn

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	pturn "github.com/pion/turn/v5"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestMintFollowsTURNRESTConvention(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := Mint(testSecret, "p_abc", DefaultTTL, now)
	if c.Username != "1700021600:p_abc" {
		t.Fatalf("username=%q", c.Username)
	}
	// Interop: pion's own TURN REST auth handler must derive the same key.
	live := Mint(testSecret, "p_abc", time.Hour, time.Now())
	_, key, ok := pturn.LongTermTURNRESTAuthHandler(string(testSecret), nil)(&pturn.RequestAttributes{Username: live.Username, Realm: "r"})
	if !ok || string(key) != string(pturn.GenerateAuthKey(live.Username, "r", live.Credential)) {
		t.Fatal("credential incompatible with pion TURN REST handler")
	}
	if c.Credential != password(testSecret, c.Username) {
		t.Fatal("credential is not base64(HMAC-SHA1(secret, username))")
	}
}

func TestVerify(t *testing.T) {
	now := time.Now()
	c := Mint(testSecret, "p_guest", time.Hour, now)
	if pid, err := Verify(testSecret, c.Username, c.Credential, now); err != nil || pid != "p_guest" {
		t.Fatalf("valid credential rejected: %q %v", pid, err)
	}
	if _, err := Verify(testSecret, c.Username, c.Credential, now.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired accepted: %v", err)
	}
	if _, err := Verify([]byte("another-secret-another-secret!!!"), c.Username, c.Credential, now); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	// Re-labelling the username for a different participant invalidates the HMAC.
	forged := strings.Replace(c.Username, "p_guest", "p_host", 1)
	if _, err := Verify(testSecret, forged, c.Credential, now); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("credential reused for another participant: %v", err)
	}
	for _, u := range []string{"", "p_guest", "abc:p", "123:", "123:a:b"} {
		if _, err := Verify(testSecret, u, c.Credential, now); !errors.Is(err, ErrMalformedUsername) {
			t.Errorf("username %q: %v", u, err)
		}
	}
}

func TestNewSecretIsRandom(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSecret()
	if len(a) != SecretSize || string(a) == string(b) {
		t.Fatal("secret not random")
	}
}

func startTestServer(t *testing.T, authorize func(string) bool, allow func(*net.UDPAddr) bool) *Server {
	t.Helper()
	s, err := Start(Config{ListenAddr: "127.0.0.1:0", Secret: testSecret, Authorize: authorize, AllowPeer: allow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func allocate(t *testing.T, s *Server, user, pass string) (net.PacketConn, *pturn.Client, error) {
	t.Helper()
	tcp, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	c, err := pturn.NewClient(&pturn.ClientConfig{
		TURNServerAddr: s.Addr().String(), STUNServerAddr: s.Addr().String(),
		Conn: pturn.NewSTUNConn(tcp), Username: user, Password: pass, Realm: DefaultRealm,
		RTO: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	relay, err := c.Allocate()
	return relay, c, err
}

func TestServerRelaysToAllowedLoopbackPeerOnly(t *testing.T) {
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	blocked, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	peerPort := peer.LocalAddr().(*net.UDPAddr).Port
	s := startTestServer(t, nil, func(a *net.UDPAddr) bool { return a.IP.IsLoopback() && a.Port == peerPort })

	c := Mint(testSecret, "p_guest", time.Hour, time.Now())
	relay, _, err := allocate(t, s, c.Username, c.Credential)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	defer relay.Close()
	raddr := relay.LocalAddr().(*net.UDPAddr)
	if !raddr.IP.IsLoopback() {
		t.Fatalf("relay candidate %v is not loopback", raddr)
	}
	if _, err := relay.WriteTo([]byte("to-blocked"), blocked.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.WriteTo([]byte("hello-sfu"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, from, err := peer.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "hello-sfu" {
		t.Fatalf("peer did not receive relayed data: %q %v", buf[:n], err)
	}
	if from.(*net.UDPAddr).Port != raddr.Port {
		t.Fatalf("datagram came from %v, want relay %v", from, raddr)
	}
	// Return path: peer -> relay -> client.
	if _, err := peer.WriteTo([]byte("hello-guest"), from); err != nil {
		t.Fatal(err)
	}
	_ = relay.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err = relay.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "hello-guest" {
		t.Fatalf("client did not receive return data: %q %v", buf[:n], err)
	}
	_ = blocked.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := blocked.ReadFrom(buf); err == nil {
		t.Fatalf("relay delivered to a disallowed port: %q", buf[:n])
	}
}

func TestServerRejectsBadCredentials(t *testing.T) {
	authorized := map[string]bool{"p_guest": true}
	s := startTestServer(t, func(pid string) bool { return authorized[pid] }, nil)
	good := Mint(testSecret, "p_guest", time.Hour, time.Now())
	cases := map[string][2]string{
		"wrong password":      {good.Username, "bm9wZQ=="},
		"expired":             {Mint(testSecret, "p_guest", -time.Minute, time.Now()).Username, Mint(testSecret, "p_guest", -time.Minute, time.Now()).Credential},
		"wrong secret":        {good.Username, Mint([]byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"), "p_guest", time.Hour, time.Now()).Credential},
		"unadmitted":          {Mint(testSecret, "p_pending", time.Hour, time.Now()).Username, Mint(testSecret, "p_pending", time.Hour, time.Now()).Credential},
		"malformed username":  {"p_guest", good.Credential},
		"relabel participant": {strings.Replace(good.Username, "p_guest", "p_other", 1), good.Credential},
	}
	for name, cred := range cases {
		t.Run(name, func(t *testing.T) {
			if relay, _, err := allocate(t, s, cred[0], cred[1]); err == nil {
				_ = relay.Close()
				t.Fatal("allocation succeeded")
			}
		})
	}
	relay, _, err := allocate(t, s, good.Username, good.Credential)
	if err != nil {
		t.Fatalf("good credentials rejected: %v", err)
	}
	_ = relay.Close()
}
