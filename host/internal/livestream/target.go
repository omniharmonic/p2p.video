package livestream

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// errBadDestination never carries the URL or key: both may be secret.
var errBadDestination = errors.New("server URL must be rtmp:// or rtmps:// with a host and an application path, and the key must be printable ASCII without spaces")

// rtmpTarget is one parsed destination. stream is the stream key (secret).
type rtmpTarget struct {
	secure bool
	host   string // hostname, for TLS SNI and verification
	addr   string // host:port to dial
	app    string
	tcURL  string
	stream string
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f {
			return false
		}
	}
	return true
}

// parseTarget splits a server URL and key into what RTMP's connect and
// publish commands carry. With a separate key the whole URL path (and query)
// is the application, so a platform's server URL is used verbatim; without
// one the last path segment is the stream key, as in rtmp://host/app/KEY.
func parseTarget(d Destination) (rtmpTarget, error) {
	raw := strings.TrimSpace(d.URL)
	if raw == "" || len(raw) > 2048 || len(d.Key) > maxKeyLen || !printableASCII(raw) || !printableASCII(d.Key) {
		return rtmpTarget{}, errBadDestination
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "rtmp" && u.Scheme != "rtmps") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return rtmpTarget{}, errBadDestination
	}
	t := rtmpTarget{secure: u.Scheme == "rtmps", host: u.Hostname()}
	port := u.Port()
	if port == "" {
		port = "1935"
		if t.secure {
			port = "443"
		}
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return rtmpTarget{}, errBadDestination
	}
	t.addr = net.JoinHostPort(t.host, port)
	path := strings.Trim(u.EscapedPath(), "/")
	query := ""
	if u.RawQuery != "" {
		query = "?" + u.RawQuery
	}
	if d.Key != "" {
		t.app, t.stream = path+query, d.Key
	} else {
		i := strings.LastIndex(path, "/")
		if i <= 0 || i == len(path)-1 {
			return rtmpTarget{}, errBadDestination
		}
		t.app, t.stream = path[:i], path[i+1:]+query
	}
	if t.app == "" || strings.HasPrefix(t.app, "?") {
		return rtmpTarget{}, errBadDestination
	}
	t.tcURL = u.Scheme + "://" + u.Host + "/" + t.app
	return t, nil
}
