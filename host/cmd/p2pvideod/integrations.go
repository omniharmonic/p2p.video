package main

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/filespeech"
	"github.com/omniharmonic/studiolink/host/internal/integrations"
	"github.com/omniharmonic/studiolink/host/internal/minutes"
	"github.com/omniharmonic/studiolink/host/internal/parachute"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// buildIntegrations wires the post-take handoffs (SL-P4-1). Every part is
// optional: a missing minutes-cli, vault or ffmpeg only disables its action
// and the reason is shown in the host UI. Paths are logged; tokens never are.
func buildIntegrations(cfg *config.Config, st *store.Store, pipe *postproc.Pipeline, ff *ffmpeg.Runner) *integrations.Service {
	svc := &integrations.Service{
		Store: st, Package: pipe, ParachuteFolder: cfg.ParachuteFolder,
		StateDir: filepath.Join(cfg.DataDir, "integrations"),
	}
	if ff != nil {
		svc.Mixer = integrations.FFmpegMixer{Runner: ff}
	}
	// The built-in engine is always wired; it is available while the Mac app
	// (or `studiolink-captions file-provider`) is connected [SL-P4-3].
	svc.Builtin = filespeech.NewBroker()
	if path, err := minutes.Discover(cfg.MinutesCLI, minutes.DefaultFinder()); err != nil {
		svc.MinutesReason = err.Error()
		slog.Info("Minutes transcripts unavailable", "reason", err)
	} else {
		client := minutes.NewClient(path)
		client.TempDir = filepath.Join(cfg.DataDir, "integrations", "tmp")
		if err := os.MkdirAll(client.TempDir, 0o700); err != nil {
			svc.MinutesReason = "cannot create a private work folder: " + err.Error()
		} else {
			svc.Minutes = client
			slog.Info("Discovered minutes-cli", "path", path)
		}
	}
	client, reason := parachuteClient(cfg)
	if client != nil {
		svc.Parachute = client
		slog.Info("Parachute session notes enabled", "vault", client.VaultURL())
	} else {
		svc.ParachuteReason = reason
	}
	return svc
}

func parachuteClient(cfg *config.Config) (*parachute.Client, string) {
	if cfg.ParachuteURL == "" {
		return nil, "no Parachute vault configured (start p2pvideod with --parachute-url)"
	}
	origin, err := parachute.ValidateOrigin(cfg.ParachuteURL)
	if err != nil {
		return nil, err.Error()
	}
	vault := cfg.ParachuteVault
	if vault == "" {
		vault = "default"
	}
	if !parachute.ValidVault(vault) {
		return nil, "invalid --parachute-vault name"
	}
	var tokens parachute.TokenSource
	if cfg.ParachuteTokenFile != "" {
		tokens = parachute.FileTokenSource{Path: cfg.ParachuteTokenFile}
	} else {
		home, _ := os.UserHomeDir()
		cli, err := parachute.DiscoverCLI(cfg.ParachuteCLI, exec.LookPath, isExecutable, home)
		if err != nil {
			return nil, err.Error() + " (install it, or pass --parachute-token-file)"
		}
		tokens = parachute.CLITokenSource{Path: cli, Vault: vault}
	}
	return &parachute.Client{Origin: origin.String(), Vault: vault, Tokens: tokens}, ""
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}
