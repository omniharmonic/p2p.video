package config

import (
	"crypto/rand"
	"os"
	"path/filepath"
)

// Config represents runtime configuration for p2pvideod.
type Config struct {
	DataDir             string
	ExportDir           string // Empty uses DataDir/exports.
	HTTPPort            int
	DevMode             bool
	NoFunnel            bool
	OwnerTailscaleLogin string // Opt-in owner identity, accepted only through private Serve.
	PublicURL           string
	TailnetDomain       string
	SecretKey           []byte
	MaxChunkSizeMB      int64

	// ICE / public-guest networking (ARCHITECTURE §3, ADR-004/005).
	PublicSTUN bool     // Advertise public STUN to clients and use it in the SFU.
	STUNURLs   []string // Public STUN servers used when PublicSTUN is set.
	UDPPort    int      // SFU single UDP mux port; 0 = ephemeral ports.
	TURNListen string   // Embedded TURN TCP listen address (loopback); "" disables.
	TURNURLs   []string // TURN URLs advertised to admitted participants.
	TURNSecret []byte   // Per-run TURN HMAC secret. Never log.

	// Ecosystem handoffs (ROADMAP P4, SL-P4-1). All optional.
	MinutesCLI         string // Absolute minutes-cli path; empty discovers it.
	ParachuteURL       string // Vault hub origin; empty disables "Send to Parachute".
	ParachuteVault     string // Vault name (default "default").
	ParachuteFolder    string // Vault folder for session notes (default "Meetings").
	ParachuteCLI       string // Absolute parachute CLI path for minting tokens; empty discovers it.
	ParachuteTokenFile string // Optional 0600 token file used instead of minting. Never log its contents.

	// Abandoned record-link drafts [SL-P5-4] (DECISIONS 2026-10-26).
	// RecordDraftExpiryDays: drafts idle this long are deleted automatically
	// on start and daily; 0 turns automatic cleanup off entirely.
	RecordDraftExpiryDays int
	// RecordDraftClosedLinkHours: drafts on an expired or turned-off link
	// are deleted once idle this long (automatic cleanup only).
	RecordDraftClosedLinkHours int
	// RecordDraftUploadGuardMinutes: a draft with a take start or chunk
	// upload this recent is never deleted, automatically or by hand.
	// <= 0 means the default (60).
	RecordDraftUploadGuardMinutes int
}

// Abandoned-draft defaults [SL-P5-4].
const (
	DefaultRecordDraftExpiryDays         = 14
	DefaultRecordDraftClosedLinkHours    = 24
	DefaultRecordDraftUploadGuardMinutes = 60
)

// Getenv reads P2PVIDEO_<name>, falling back to STUDIOLINK_<name> (the
// pre-rename prefix, still supported) [SL-REL-1].
func Getenv(name string) string {
	if v := os.Getenv("P2PVIDEO_" + name); v != "" {
		return v
	}
	return os.Getenv("STUDIOLINK_" + name)
}

// LoadOrCreateSecretKey returns nil on a read, entropy or persistence failure; callers must fail closed.
// It retrieves the secret key from environment, disk, or generates a new 32-byte key.
func LoadOrCreateSecretKey(dataDir string) []byte {
	if envKey := Getenv("SECRET_KEY"); len(envKey) >= 32 {
		return []byte(envKey)
	}

	keyPath := filepath.Join(dataDir, ".secret_key")
	if data, err := os.ReadFile(keyPath); err == nil {
		if len(data) < 32 {
			return nil
		}
		return data
	} else if !os.IsNotExist(err) {
		return nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil
	}
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		data, e := os.ReadFile(keyPath)
		if e == nil && len(data) >= 32 {
			return data
		}
		return nil
	}
	if err != nil {
		return nil
	}
	if _, err = f.Write(key); err != nil {
		_ = f.Close()
		return nil
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return nil
	}
	if err = f.Close(); err != nil {
		return nil
	}
	return key
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	dataDir := filepath.Join(home, "Movies", "StudioLinkData")

	return &Config{
		DataDir:        dataDir,
		HTTPPort:       7443,
		DevMode:        false,
		NoFunnel:       false,
		PublicURL:      "http://localhost:7443",
		TailnetDomain:  "localhost",
		SecretKey:      nil, // Load only after flags select the actual data directory.
		MaxChunkSizeMB: 50,
		PublicSTUN:     true,
		STUNURLs:       append([]string(nil), DefaultPublicSTUN...),
		UDPPort:        50000,
		TURNListen:     "127.0.0.1:3478",

		RecordDraftExpiryDays:         DefaultRecordDraftExpiryDays,
		RecordDraftClosedLinkHours:    DefaultRecordDraftClosedLinkHours,
		RecordDraftUploadGuardMinutes: DefaultRecordDraftUploadGuardMinutes,
	}
}
