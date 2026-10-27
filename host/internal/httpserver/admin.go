package httpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Host management authorization [SL-SEC-1].
//
// Two levels guard the host's own routes:
//
//   - managementAllowed (network position): a direct loopback request with a
//     loopback Host header, or the configured owner over private Tailscale
//     Serve. Funnel is always refused. Routes that also demand a host token
//     (requireHost, host-token WebSockets) use this: the token is the
//     credential, the position keeps it off Funnel and the rest of the tailnet.
//
//   - adminAllowed (credential): routes that MINT host tokens or reveal room
//     links (create session, rooms, home, doctor, tailscale status) need more
//     than "the request came from this Mac", because any local process or OS
//     user can open a loopback socket. On loopback they need either the
//     per-boot admin cookie (set by a launch-token navigation, below) or
//     `Authorization: Bearer <desktop token>` (the Mac app, p2pvideo, tests). The
//     owner-over-Serve path is authorized by Tailscale identity as before.
//
// Launch flow: the daemon writes a fresh random launch token to
// <data-dir>/.launch_token (0600) at every start. The Mac app, the launcher
// and `p2pvideo dashboard` open http://127.0.0.1:<port>/?launch=<token>. On that
// top-level loopback navigation the daemon sets the admin cookie (HttpOnly,
// SameSite=Strict, per-boot random value, never derived from the token) and
// redirects to the same URL without the token, so it never stays in the
// address bar, history or a Referer.

const launchParam = "launch"

// adminCookieName is per port: browsers share cookies across ports of one
// host, and two daemons (dev and app) must not overwrite each other's cookie.
func adminCookieName(r *http.Request) string {
	_, port, err := net.SplitHostPort(r.Host)
	if err != nil || port == "" {
		port = "80"
	}
	return "studiolink_admin_" + port
}

// adminSession is this process's admin cookie value, random per boot.
func (s *Server) adminSession() string {
	s.adminOnce.Do(func() {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			// Without entropy no cookie is ever issued or accepted.
			slog.Error("admin session entropy unavailable; dashboard sign-in disabled", "err", err)
			return
		}
		s.adminValue = hex.EncodeToString(b)
	})
	return s.adminValue
}

func secretEqual(got, want string) bool {
	return want != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// adminCredential reports whether a loopback request carries the admin
// cookie or the desktop token.
func (s *Server) adminCredential(r *http.Request) bool {
	if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && secretEqual(got, s.DesktopToken) {
		return true
	}
	if c, err := r.Cookie(adminCookieName(r)); err == nil && secretEqual(c.Value, s.adminSession()) {
		return true
	}
	return false
}

// adminAllowed gates routes that mint host credentials or reveal room links.
func (s *Server) adminAllowed(r *http.Request) bool {
	if localManagement(r) {
		return s.adminCredential(r)
	}
	return s.ownerServeAllowed(r)
}

// adminDenied writes the 403 for a missing admin credential.
func adminDenied(w http.ResponseWriter, r *http.Request, what string) {
	if localManagement(r) {
		http.Error(w, what+" needs the host dashboard sign-in on this Mac. Open the dashboard from the p2p.video app (or run `p2pvideo dashboard`).", http.StatusForbidden)
		return
	}
	http.Error(w, what+" is available only on the local host dashboard", http.StatusForbidden)
}

// handleLaunch consumes ?launch=<token> on a top-level page navigation. It
// reports whether it wrote the response.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") || !r.URL.Query().Has(launchParam) {
		return false
	}
	q := r.URL.Query()
	token := q.Get(launchParam)
	q.Del(launchParam)
	clean := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	mode := r.Header.Get("Sec-Fetch-Mode")
	switch {
	case r.Method != http.MethodGet:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	case !localManagement(r) || (mode != "" && mode != "navigate"):
		slog.Warn("Dashboard launch refused: not a direct loopback navigation")
	case !secretEqual(token, s.LaunchToken):
		slog.Warn("Dashboard launch refused: stale or wrong launch token")
	default:
		if v := s.adminSession(); v != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     adminCookieName(r),
				Value:    v,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		}
	}
	// Always drop the token from the address bar, valid or not.
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, clean.String(), http.StatusSeeOther)
	return true
}

// contentSecurityPolicy applies to every response. The embedded UI is Vite
// output: one module script from 'self', no inline scripts or handlers, no
// eval. AudioWorklet modules are emitted as files ('self'); the compositor's
// timer worker is a blob: URL. Inline style attributes are used.
// 'wasm-unsafe-eval' lets the page compile WebAssembly (MediaPipe's
// background segmentation, served from /mediapipe/ on our own origin)
// [SL-P1-7]; it does not allow eval() or new Function().
const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self' blob:; " +
	"connect-src 'self' ws: wss:; img-src 'self' data: blob:; media-src 'self' blob:; " +
	"style-src 'self' 'unsafe-inline'; font-src 'self'; object-src 'none'; frame-ancestors 'none'; " +
	"base-uri 'none'; form-action 'self'"

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	// Invite and host URLs carry tokens: never send them as a Referer.
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// requestReadTimeout bounds reading any request body except chunk uploads
// (which set their own size-based deadline) and WebSockets (which manage
// their own deadlines after the upgrade).
const requestReadTimeout = 30 * time.Second

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
