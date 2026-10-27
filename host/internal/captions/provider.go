package captions

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProviderProtocol is the caption provider protocol version.
const ProviderProtocol = 1

// Provider control message types (JSON text frames). See
// docs/integrations/CAPTIONS.md for direction and fields.
const (
	MsgHello       = "hello"        // P→D first message
	MsgReady       = "ready"        // D→P hello accepted
	MsgStreamStart = "stream_start" // D→P a new audio stream begins
	MsgStreamEnd   = "stream_end"   // D→P no more audio for a stream
	MsgResult      = "result"       // P→D a partial or final recognition result
	MsgError       = "error"        // P→D a stream failed; D→P a protocol error
)

// Provider limits on untrusted recognizer input.
const (
	maxProviderName   = 64
	maxProviderLangs  = 64
	maxSegmentIDLen   = 64
	maxProviderMsg    = 16 << 10
	maxResultsPerSec  = 40
	maxProviderErrMsg = 200
)

// ProviderMessage is every provider control message. Fields not used by a
// message type are omitted.
type ProviderMessage struct {
	T string `json:"t"`

	// hello / ready
	Protocol  int      `json:"protocol,omitempty"`
	Name      string   `json:"name,omitempty"`
	Engine    string   `json:"engine,omitempty"`
	Languages []string `json:"languages,omitempty"`

	// stream_start / stream_end / result / error
	StreamID   string `json:"streamId,omitempty"`
	Language   string `json:"language,omitempty"`
	SampleRate int    `json:"sampleRate,omitempty"`
	Encoding   string `json:"encoding,omitempty"`

	// result: times are milliseconds from the stream's first sample.
	SegmentID string `json:"segmentId,omitempty"`
	Text      string `json:"text,omitempty"`
	IsFinal   bool   `json:"isFinal,omitempty"`
	StartMs   int64  `json:"startMs,omitempty"`
	EndMs     int64  `json:"endMs,omitempty"`

	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ErrProviderHello rejects a provider whose first message is not a valid hello.
var ErrProviderHello = errors.New("captions: provider must start with a protocol 1 hello")

// ParseProviderHello validates a provider's first message.
func ParseProviderHello(raw []byte) (ProviderMessage, error) {
	var m ProviderMessage
	if len(raw) > maxProviderMsg || json.Unmarshal(raw, &m) != nil || m.T != MsgHello || m.Protocol != ProviderProtocol {
		return ProviderMessage{}, ErrProviderHello
	}
	m.Name = cleanText(m.Name, maxProviderName)
	m.Engine = cleanText(m.Engine, maxProviderName)
	langs := make([]string, 0, min(len(m.Languages), maxProviderLangs))
	for _, l := range m.Languages {
		if len(langs) == maxProviderLangs {
			break
		}
		if ValidLanguage(l) {
			langs = append(langs, l)
		}
	}
	m.Languages = langs
	return m, nil
}

var languageRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8}){0,2}$`)

// ValidLanguage accepts a short BCP 47 tag such as "en-US".
func ValidLanguage(tag string) bool {
	return len(tag) <= 16 && languageRE.MatchString(tag)
}

// cleanText makes untrusted recognizer text safe to relay: valid UTF-8,
// control characters (including newlines) folded to spaces, trimmed and
// bounded. Clients still render it as text only.
func cleanText(s string, maxRunes int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	return s
}
