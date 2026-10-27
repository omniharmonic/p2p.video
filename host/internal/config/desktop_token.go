package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DesktopTokenFile is the per-data-directory credential the native host app
// presents to the loopback-only desktop state endpoint. It is created 0600 next
// to .secret_key: any process that can read it can already read the signing key.
const DesktopTokenFile = ".desktop_token"

// LoadOrCreateDesktopToken returns the hex desktop control token, creating it
// on first use. Errors are returned so callers disable the endpoint instead of
// serving it without a credential.
func LoadOrCreateDesktopToken(dataDir string) (string, error) {
	path := filepath.Join(dataDir, DesktopTokenFile)
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 32 {
			return "", fmt.Errorf("desktop token %s is too short", path)
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read desktop token: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("desktop token entropy: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return LoadOrCreateDesktopToken(dataDir)
	}
	if err != nil {
		return "", fmt.Errorf("create desktop token: %w", err)
	}
	if _, err := f.WriteString(token); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write desktop token: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close desktop token: %w", err)
	}
	return token, nil
}
