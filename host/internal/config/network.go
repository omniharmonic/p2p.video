package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DefaultPublicSTUN are the public STUN servers used when policy allows them
// (ADR-005). They only ever see IP addresses, never media.
var DefaultPublicSTUN = []string{"stun:stun.cloudflare.com:3478", "stun:stun.l.google.com:19302"}

// FunnelTURNPort is the public Funnel port that terminates TLS and forwards
// plain TCP to the embedded TURN listener (ADR-004).
const FunnelTURNPort = 8443

// FunnelTURNURL derives the `turns:` URL reached through Tailscale Funnel from
// the public HTTPS URL, e.g. https://studio.tail1234.ts.net ->
// turns:studio.tail1234.ts.net:8443?transport=tcp.
func FunnelTURNURL(publicURL string) (string, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return "", fmt.Errorf("parse public url: %w", err)
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return "", errors.New("funnel TURN needs an https --public-url (the Funnel hostname)")
	}
	return fmt.Sprintf("turns:%s:%d?transport=tcp", u.Hostname(), FunnelTURNPort), nil
}

// ParseTURNURLs splits a comma-separated list of TURN URLs and validates each.
func ParseTURNURLs(list string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(list, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if err := ValidateTURNURL(raw); err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// ValidateTURNURL accepts turn:/turns: URIs (RFC 7065) with a host and port.
func ValidateTURNURL(raw string) error {
	scheme, rest, ok := strings.Cut(raw, ":")
	if !ok || (scheme != "turn" && scheme != "turns") {
		return fmt.Errorf("TURN url %q must use the turn or turns scheme", raw)
	}
	hostport, query, _ := strings.Cut(rest, "?")
	host, port, found := strings.Cut(hostport, ":")
	if !found || host == "" || port == "" || strings.ContainsAny(hostport, "/@ ") {
		return fmt.Errorf("TURN url %q needs host:port", raw)
	}
	if query != "" && query != "transport=tcp" && query != "transport=udp" {
		return fmt.Errorf("TURN url %q has an unsupported query", raw)
	}
	return nil
}
