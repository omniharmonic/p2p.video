// Package turn runs p2p.video's embedded TURN relay and mints the short-lived
// credentials handed to admitted participants.
//
// Credentials follow the TURN REST convention
// (draft-uberti-behave-turn-rest-00): username = "<expiryUnix>:<participantId>",
// credential = base64(HMAC-SHA1(secret, username)). The secret is generated per
// daemon run, held only in memory and never logged.
package turn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // TURN REST credentials are defined over HMAC-SHA1.
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultTTL is how long minted TURN credentials stay valid. Long enough for a
// recording session; a reconnect re-admits and mints fresh credentials.
const DefaultTTL = 6 * time.Hour

// SecretSize is the number of random bytes in a generated TURN secret.
const SecretSize = 32

var (
	ErrMalformedUsername = errors.New("turn: malformed username")
	ErrExpired           = errors.New("turn: credentials expired")
	ErrBadCredential     = errors.New("turn: credential mismatch")
)

// Credentials are the long-term TURN credentials given to one participant.
type Credentials struct {
	Username   string
	Credential string
	Expires    time.Time
}

// NewSecret returns a fresh random TURN shared secret.
func NewSecret() ([]byte, error) {
	b := make([]byte, SecretSize)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate turn secret: %w", err)
	}
	return b, nil
}

// Mint creates credentials for participantID valid until now+ttl.
func Mint(secret []byte, participantID string, ttl time.Duration, now time.Time) Credentials {
	exp := now.Add(ttl).Unix()
	username := strconv.FormatInt(exp, 10) + ":" + participantID
	return Credentials{Username: username, Credential: password(secret, username), Expires: time.Unix(exp, 0)}
}

// Verify checks a username/credential pair and returns the participant it was
// minted for. It is used by tests and diagnostics; the TURN server itself
// authenticates with the message-integrity key derived from the same password.
func Verify(secret []byte, username, credential string, now time.Time) (string, error) {
	pid, err := ParseUsername(username, now)
	if err != nil {
		return "", err
	}
	if !hmac.Equal([]byte(password(secret, username)), []byte(credential)) {
		return "", ErrBadCredential
	}
	return pid, nil
}

// ParseUsername validates the "<expiry>:<participantId>" shape and expiry.
func ParseUsername(username string, now time.Time) (string, error) {
	ts, pid, ok := strings.Cut(username, ":")
	if !ok || pid == "" || strings.Contains(pid, ":") {
		return "", ErrMalformedUsername
	}
	exp, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", ErrMalformedUsername
	}
	if now.Unix() >= exp {
		return "", ErrExpired
	}
	return pid, nil
}

func password(secret []byte, username string) string {
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
