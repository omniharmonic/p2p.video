package livestream

import (
	"net/url"
	"sort"
	"strings"
)

const redacted = "****"

// Redactor removes stream keys and publish URLs from text (ffmpeg stderr,
// errors) before it is logged or returned to a client.
type Redactor struct{ secrets []string }

// NewRedactor collects every secret representation for the destinations:
// the key (raw and escaped), the full publish URL and the URL path, which
// for custom servers may itself carry the key.
func NewRedactor(dests []Destination) *Redactor {
	seen := map[string]bool{}
	var secrets []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			secrets = append(secrets, s)
		}
	}
	for _, d := range dests {
		if d.Key != "" {
			add(d.Key)
			add(url.QueryEscape(d.Key))
			add(url.PathEscape(d.Key))
		}
		full := d.publishURL()
		add(full)
		if u, err := url.Parse(full); err == nil {
			if len(u.Path) > 1 {
				add(u.Path)
				add(strings.TrimPrefix(u.Path, "/"))
			}
			add(u.RawQuery)
		}
	}
	// Longest first, so a URL is replaced before its key substring.
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return &Redactor{secrets: secrets}
}

// Redact replaces every known secret with ****.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// ServerLabel returns scheme://host[:port] for display: never the path,
// query or key.
func ServerLabel(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid server"
	}
	return u.Scheme + "://" + u.Host
}
