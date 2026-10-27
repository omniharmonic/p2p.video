package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/tailscale"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "session":
		handleSession(os.Args[2:])
	case "postprocess":
		handlePostprocess(os.Args[2:])
	case "doctor":
		handleDoctor()
	case "dashboard":
		handleDashboard(os.Args[2:])
	case "synctest":
		handleSynctest()
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`p2pvideo - p2p.video CLI Operator Utility

Usage:
  p2pvideo session create [--title <title>]   Create a new recording room
  p2pvideo dashboard [--open] [--port N] [--data-dir D]
                                           Print (or open) the signed-in host dashboard URL
  p2pvideo postprocess <takeId>               Process take, conform CFR, and export NLE packages
  p2pvideo doctor                             Check Tailscale, FFmpeg, and environment
  p2pvideo synctest                           Run cross-correlation synchronization accuracy test`)
}

func handleSession(args []string) {
	if len(args) == 0 || args[0] != "create" {
		fmt.Println("Usage: p2pvideo session create [--title <title>]")
		return
	}

	title := "New Session"
	for i := 1; i < len(args); i++ {
		if args[i] == "--title" && i+1 < len(args) {
			title = args[i+1]
		}
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"title":     title,
		"autoAdmit": true,
	})

	// Management routes need the desktop token (or the dashboard cookie).
	token, err := config.LoadOrCreateDesktopToken(dataDirFromEnv())
	if err != nil {
		fmt.Printf("Cannot read the desktop token: %v\n", err)
		return
	}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:7443/api/v1/sessions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("Error contacting p2pvideod daemon (is it running?): %v\n", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Session Created:\n%s\n", string(body))
}

// dataDirFromEnv is the daemon's data directory: P2PVIDEO_DATA_DIR / STUDIOLINK_DATA_DIR or the default.
func dataDirFromEnv() string {
	if d := config.Getenv("DATA_DIR"); d != "" {
		return d
	}
	return config.DefaultConfig().DataDir
}

// handleDashboard prints (or opens) the dashboard launch URL, which signs
// this browser in as the host for the daemon's current run [SL-SEC-1]. The
// URL contains a secret: it is printed for the user, never logged.
func handleDashboard(args []string) {
	dataDir, port, open := dataDirFromEnv(), 7443, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--open":
			open = true
		case "--data-dir":
			if i+1 < len(args) {
				i++
				dataDir = args[i]
			}
		case "--port":
			if i+1 < len(args) {
				i++
				if _, err := fmt.Sscanf(args[i], "%d", &port); err != nil {
					fmt.Println("--port needs a number")
					os.Exit(2)
				}
			}
		}
	}
	token, err := config.ReadLaunchToken(dataDir)
	if err != nil {
		fmt.Printf("No dashboard launch token in %s (is p2pvideod running with this --data-dir?): %v\n", dataDir, err)
		os.Exit(1)
	}
	u := fmt.Sprintf("http://127.0.0.1:%d/?launch=%s", port, token)
	if open {
		if err := exec.Command("open", u).Run(); err != nil {
			fmt.Printf("Cannot open the browser: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println(u)
}

func handlePostprocess(args []string) {
	if len(args) < 1 {
		fmt.Println("Usage: p2pvideo postprocess <takeId>")
		return
	}
	takeID := args[0]

	cfg := config.DefaultConfig()
	dbPath := filepath.Join(cfg.DataDir, "studiolink.db")
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Printf("Error opening database at %s: %v\n", dbPath, err)
		return
	}
	defer st.Close()

	ff, _ := ffmpeg.NewRunner()
	pipe := postproc.NewPipeline(st, cfg, ff)

	fmt.Printf("Processing take %s...\n", takeID)
	if err := pipe.ProcessTake(takeID); err != nil {
		fmt.Printf("Post-processing failed: %v\n", err)
		return
	}
	fmt.Printf("Take %s successfully processed! Exports are under %s\n", takeID, filepath.Join(cfg.DataDir, "exports"))
}

func handleDoctor() {
	fmt.Println("--- p2p.video Environment Doctor ---")

	// 1. FFmpeg
	ff, err := ffmpeg.NewRunner()
	if err != nil {
		fmt.Printf("❌ FFmpeg: NOT FOUND (%v)\n", err)
	} else {
		fmt.Printf("✅ FFmpeg: %s (VideoToolbox HW Acceleration: %t)\n", ff.FFmpegPath, ff.HasVideoToolbox)
	}

	// 2. Tailscale
	ts := tailscale.NewClient()
	status, err := ts.CheckStatus()
	if err != nil {
		fmt.Printf("⚠️ Tailscale: %v (local dev fallback available)\n", err)
	} else {
		fmt.Printf("✅ Tailscale: Online=%t DNSName=%s\n", status.Self.Online, status.Self.DNSName)
	}

	// 3. p2p.video Daemon
	resp, err := http.Get("http://localhost:7443/health")
	if err != nil {
		fmt.Println("ℹ️  p2pvideod daemon: Not running on localhost:7443 (start with `p2pvideod --dev`)")
	} else {
		defer resp.Body.Close()
		fmt.Println("✅ p2pvideod daemon: Active on http://localhost:7443")
	}
}

func handleSynctest() {
	fmt.Println("Running Audio Alignment GCC-PHAT Synthetic Sync Benchmark...")

	sampleRate := 48000
	durationSec := 1.0
	numSamples := int(float64(sampleRate) * durationSec)

	// Create reference signal (440Hz sine wave + 880Hz harmonic + bursts)
	refSig := make([]float64, numSamples)
	for i := 0; i < numSamples; i++ {
		t := float64(i) / float64(sampleRate)
		refSig[i] = math.Sin(2*math.Pi*440*t) + 0.5*math.Sin(2*math.Pi*880*t)
		if i%4800 < 240 { // periodic claps
			refSig[i] += 1.5
		}
	}

	// Target lag: 100 ms delay (0.100 * 48000 = 4800 samples)
	targetDelaySamples := 4800
	targetDelaySec := float64(targetDelaySamples) / float64(sampleRate)

	locSig := make([]float64, numSamples)
	for i := targetDelaySamples; i < numSamples; i++ {
		// simulate slight drift and noise
		locSig[i] = refSig[i-targetDelaySamples] + 0.01*(math.Sin(float64(i)))
	}

	// Run cross-correlation (reference vs local)
	bestLag, peak := align.CrossCorrelate(refSig, locSig, sampleRate/2)
	measuredDelaySec := float64(bestLag) / float64(sampleRate)
	residualMs := math.Abs(measuredDelaySec-targetDelaySec) * 1000.0

	fmt.Printf("  Target Delay:    %.4f s (%d samples)\n", targetDelaySec, targetDelaySamples)
	fmt.Printf("  Measured Delay:  %.4f s (%d samples)\n", measuredDelaySec, bestLag)
	fmt.Printf("  Peak Correlation: %.4f\n", peak)
	fmt.Printf("  Sync Residual:   %.3f ms\n", residualMs)

	if residualMs < 1.0 {
		fmt.Println("✅ PASSED: Residual < 1.0 ms! Sub-millisecond multicam alignment verified.")
	} else {
		fmt.Println("❌ FAILED: Residual exceeded threshold.")
	}
}
