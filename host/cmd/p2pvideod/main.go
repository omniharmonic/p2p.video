package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/captions"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/filespeech"
	"github.com/omniharmonic/studiolink/host/internal/httpserver"
	"github.com/omniharmonic/studiolink/host/internal/livestream"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/sfu"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
	"github.com/omniharmonic/studiolink/host/internal/webassets"
)

func main() {
	cfg := config.DefaultConfig()

	listenHost := "127.0.0.1"
	flag.StringVar(&listenHost, "listen-host", listenHost, "HTTP listen address; loopback is safe behind Tailscale Serve")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data storage directory")
	flag.IntVar(&cfg.HTTPPort, "port", cfg.HTTPPort, "HTTP server port")
	flag.BoolVar(&cfg.DevMode, "dev", false, "Developer mode")
	flag.BoolVar(&cfg.NoFunnel, "no-funnel", false, "Disable Tailscale Funnel exposure")
	flag.StringVar(&cfg.PublicURL, "public-url", "", "Public URL base")
	flag.StringVar(&cfg.ExportDir, "export-dir", "", "Optional editor export directory (default: data-dir/exports)")
	flag.StringVar(&cfg.OwnerTailscaleLogin, "owner-tailscale-login", "", "Optional exact Tailscale owner login for private Serve dashboard access")
	noPublicSTUN := flag.Bool("no-public-stun", false, "Do not use or advertise public STUN servers (ADR-005)")
	flag.IntVar(&cfg.UDPPort, "udp-port", cfg.UDPPort, "SFU single UDP mux port (0 = ephemeral ports; falls back to ephemeral if busy)")
	flag.StringVar(&cfg.TURNListen, "turn-listen", cfg.TURNListen, "Embedded TURN TCP listen address, loopback only (empty disables TURN)")
	turnURLs := flag.String("turn-url", "", "Comma-separated TURN URLs advertised to admitted participants, e.g. turn:127.0.0.1:3478?transport=tcp")
	funnelTURN := flag.Bool("funnel-turn", false, "Advertise turns:<public-url host>:8443?transport=tcp (Tailscale Funnel TLS-terminated TCP to --turn-listen)")
	flag.StringVar(&cfg.MinutesCLI, "minutes-cli", "", "Absolute path to minutes-cli for post-take transcripts (default: discover)")
	flag.StringVar(&cfg.ParachuteURL, "parachute-url", "", "Parachute hub origin for session notes, e.g. https://hub.example.com (empty disables)")
	flag.StringVar(&cfg.ParachuteVault, "parachute-vault", "default", "Parachute vault name for session notes")
	flag.StringVar(&cfg.ParachuteFolder, "parachute-folder", "Meetings", "Vault folder for session notes")
	flag.StringVar(&cfg.ParachuteCLI, "parachute-cli", "", "Absolute path to the parachute CLI used to mint ephemeral tokens (default: discover)")
	flag.StringVar(&cfg.ParachuteTokenFile, "parachute-token-file", "", "Optional 0600 file holding a vault:write token, used instead of minting")
	livestreamTestCA := flag.String("livestream-test-ca", "", "TESTING ONLY: PEM file of an extra CA trusted for rtmps:// livestream destinations, in addition to the system trust store")
	flag.IntVar(&cfg.RecordDraftExpiryDays, "record-draft-expiry-days", cfg.RecordDraftExpiryDays, "Delete record-link drafts idle this many days, on start and daily (0 turns automatic draft cleanup off)")
	flag.IntVar(&cfg.RecordDraftClosedLinkHours, "record-draft-closed-link-hours", cfg.RecordDraftClosedLinkHours, "Delete drafts on expired or turned-off record links once idle this many hours")
	flag.IntVar(&cfg.RecordDraftUploadGuardMinutes, "record-draft-upload-guard-minutes", cfg.RecordDraftUploadGuardMinutes, "Never delete a draft with a take start or upload in the last N minutes")
	flag.Parse()
	if cfg.PublicURL == "" {
		cfg.PublicURL = fmt.Sprintf("http://localhost:%d", cfg.HTTPPort)
	}
	if err := applyNetworkFlags(cfg, *noPublicSTUN, *turnURLs, *funnelTURN); err != nil {
		slog.Error("Invalid network configuration", "err", err)
		os.Exit(1)
	}

	// Ensure secret key is loaded from the actual configured DataDir
	cfg.SecretKey = config.LoadOrCreateSecretKey(cfg.DataDir)
	if len(cfg.SecretKey) < 32 {
		slog.Error("Cannot load or persist signing identity; check data-dir permissions and .secret_key")
		os.Exit(1)
	}

	slog.Info("Starting p2p.video daemon", "version", "0.1.0", "port", cfg.HTTPPort, "dataDir", cfg.DataDir)

	// Owner-only data directory: database, raw recordings, tokens [SL-SEC-1].
	// Exports the user picks with --export-dir keep their own permissions.
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		slog.Error("Failed to create data dir", "err", err)
		os.Exit(1)
	}
	if err := os.Chmod(cfg.DataDir, 0o700); err != nil {
		slog.Warn("Cannot restrict data dir to its owner", "err", err)
	}

	// 1. Open SQLite database
	dbPath := filepath.Join(cfg.DataDir, "studiolink.db")
	st, err := store.Open(dbPath)
	if err != nil {
		slog.Error("Failed to open SQLite database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// 2. Reference audio recorder
	refRec := refrec.NewManager()

	// 3. Pion SFU
	sfuEngine, err := sfu.NewSFU(refRec, sfu.WithNetwork(sfuNetworkOptions(cfg)))
	if err != nil {
		slog.Error("Failed to initialize Pion SFU", "err", err)
		os.Exit(1)
	}
	defer sfuEngine.Close()

	// 4. Signaling Hub
	hub := signaling.NewHub(st, cfg, sfuEngine)
	hub.References = refRec
	refRec.Allow = hub.RecordsReference
	defer hub.Close()

	// 4a. Live captions [SL-P4-2]: call audio from the host's browser to the
	// on-device caption provider. Never part of the recording pipeline.
	captionMgr := captions.NewManager(hub)
	defer captionMgr.Close()
	hub.Captions = captionMgr

	// 4b. Embedded TURN relay (public-guest fallback, ADR-004). Credentials are
	// only advertised once the relay is actually listening.
	if relay := startTURN(cfg, sfuEngine, hub); relay != nil {
		defer relay.Close()
	}

	// 5. FFmpeg
	ff, err := ffmpeg.NewRunner()
	if err != nil {
		slog.Warn("FFmpeg discovery notice", "err", err)
	} else {
		slog.Info("Discovered FFmpeg", "path", ff.FFmpegPath, "videotoolbox", ff.HasVideoToolbox)
	}

	// 6. Post-processing pipeline
	pipe := postproc.NewPipeline(st, cfg, ff)

	// 7. Upload Handler
	uploadHandler := &upload.Handler{
		Store:  st,
		Config: cfg,
		OnAssembleDone: func(takeID, trackID, assembledPath string) {
			slog.Info("Auto-assembled track", "takeId", takeID, "trackId", trackID)
		},
	}

	// 7b. Livestream publisher (independent of recording; ADR-014).
	ffmpegPath := "ffmpeg"
	if ff != nil {
		ffmpegPath = ff.FFmpegPath
	}
	liveTLS, err := livestreamTLS(*livestreamTestCA)
	if err != nil {
		slog.Error("Invalid --livestream-test-ca", "err", err)
		os.Exit(1)
	}
	live := livestream.NewManager(livestream.ExecLauncher(ffmpegPath), livestream.Options{Preflight: livestream.ExecPreflight(ff), TLS: liveTLS})

	// 8. HTTP Server. The desktop token gates the native app's loopback-only
	// state endpoint; without it that endpoint stays disabled.
	desktopToken, err := config.LoadOrCreateDesktopToken(cfg.DataDir)
	if err != nil {
		slog.Warn("Desktop app state endpoint disabled", "err", err)
	}
	// Per-boot dashboard launch token (admin.go). Never logged: launchers read
	// it from the 0600 file.
	launchToken, err := config.WriteLaunchToken(cfg.DataDir)
	if err != nil {
		slog.Warn("Dashboard launch sign-in disabled", "err", err)
	} else {
		slog.Info("Dashboard sign-in: open the launch URL from the p2p.video app or `p2pvideo dashboard`", "tokenFile", filepath.Join(cfg.DataDir, config.LaunchTokenFile))
	}
	server := &httpserver.Server{
		Config:     cfg,
		Store:      st,
		Hub:        hub,
		Upload:     uploadHandler,
		Pipeline:   pipe,
		WebFS:      webassets.FS(),
		Livestream: live,
		RuntimeCounts: func() map[string]int {
			counts := hub.Counts()
			for k, v := range sfuEngine.Counts() {
				counts[k] = v
			}
			for k, v := range captionMgr.Counts() {
				counts[k] = v
			}
			return counts
		},
		DesktopToken: desktopToken,
		LaunchToken:  launchToken,
		Integrations: buildIntegrations(cfg, st, pipe, ff),
		Captions:     captionMgr,
	}
	defer server.Integrations.Close()
	// Built-in post-take transcription [SL-P4-3]: the Mac app's on-device
	// recognizer connects to /api/v1/transcription/provider.
	server.Transcription, _ = server.Integrations.Builtin.(*filespeech.Broker)
	if server.Transcription != nil {
		defer server.Transcription.Close()
	}

	// Abandoned record-link drafts: cleaned on start and daily [SL-P5-4].
	stopDraftJanitor := server.StartDraftJanitor(24 * time.Hour)
	defer stopDraftJanitor()

	addr := fmt.Sprintf("%s:%d", listenHost, cfg.HTTPPort)
	srv := newHTTPServer(addr, server.Handler())

	// Graceful shutdown channel
	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		slog.Info("p2p.video HTTP listening", "addr", addr, "url", cfg.PublicURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server listen error", "err", err)
			os.Exit(1)
		}
	}()

	<-stopCh
	slog.Info("Shutting down p2p.video daemon...")
	_ = srv.Close()
	live.StopAll()
	refRec.StopAll()
}

// newHTTPServer bounds what a slow or idle client can hold [SL-SEC-1].
// Headers must arrive within 10 s. There is deliberately no whole-request
// ReadTimeout or WriteTimeout: chunk uploads at 256 kbit/s, WebSockets and
// large take downloads are long-lived. Request bodies get a per-request
// deadline in the handler (30 s, or size-based for chunk uploads).
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
