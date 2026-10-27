package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LaunchTokenFile holds the per-boot dashboard launch token [SL-SEC-1]. The
// Mac app, the zsh launcher, `p2pvideo dashboard` and the browser tests read it
// (0600, owner only) and open http://127.0.0.1:<port>/?launch=<token>; the
// daemon then sets an HttpOnly admin cookie and drops the token from the URL.
// Other local users cannot read the file, so they cannot reach the management
// API that mints host tokens and shows room links.
const LaunchTokenFile = ".launch_token"

// WriteLaunchToken creates a fresh launch token for this daemon run and
// replaces the file atomically. Each restart invalidates the previous token
// and every admin cookie issued with it.
func WriteLaunchToken(dataDir string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("launch token entropy: %w", err)
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	f, err := os.CreateTemp(dataDir, LaunchTokenFile+".*") // 0600
	if err != nil {
		return "", fmt.Errorf("create launch token: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after rename
	if _, err := f.WriteString(token); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write launch token: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close launch token: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dataDir, LaunchTokenFile)); err != nil {
		return "", fmt.Errorf("publish launch token: %w", err)
	}
	return token, nil
}

// ReadLaunchToken reads the current launch token (for p2pvideo and tests).
func ReadLaunchToken(dataDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, LaunchTokenFile))
	if err != nil {
		return "", fmt.Errorf("read launch token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) < 32 {
		return "", fmt.Errorf("launch token in %s is too short", dataDir)
	}
	return token, nil
}
