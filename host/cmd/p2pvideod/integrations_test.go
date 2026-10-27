package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/integrations"
	"github.com/omniharmonic/studiolink/host/internal/parachute"
)

func TestParachuteClientConfiguration(t *testing.T) {
	if c, reason := parachuteClient(&config.Config{}); c != nil || !strings.Contains(reason, "--parachute-url") {
		t.Fatalf("unconfigured: %v %q", c, reason)
	}
	if c, _ := parachuteClient(&config.Config{ParachuteURL: "http://hub.example.com"}); c != nil {
		t.Fatal("plain http to a remote hub accepted")
	}
	if c, _ := parachuteClient(&config.Config{ParachuteURL: "https://hub.example.com", ParachuteVault: "../x"}); c != nil {
		t.Fatal("invalid vault accepted")
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("a.b.c"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, reason := parachuteClient(&config.Config{ParachuteURL: "https://hub.example.com/", ParachuteVault: "pods", ParachuteTokenFile: token})
	if c == nil {
		t.Fatalf("token-file config rejected: %s", reason)
	}
	if c.VaultURL() != "https://hub.example.com/vault/pods" {
		t.Fatalf("vault URL %s", c.VaultURL())
	}
	if _, ok := c.Tokens.(parachute.FileTokenSource); !ok {
		t.Fatalf("token source %T", c.Tokens)
	}
	if c, reason := parachuteClient(&config.Config{ParachuteURL: "https://hub.example.com", ParachuteCLI: "/nonexistent/parachute"}); c != nil || !strings.Contains(reason, "--parachute-token-file") {
		t.Fatalf("missing CLI: %v %q", c, reason)
	}
}

func TestBuildIntegrationsWithNothingInstalled(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir(), MinutesCLI: "/nonexistent/minutes-cli"}
	svc := buildIntegrations(cfg, nil, nil, nil)
	defer svc.Close()
	if svc.Minutes != nil || !strings.Contains(svc.MinutesReason, "minutes-cli not found") || svc.Parachute != nil || svc.Mixer != nil {
		t.Fatalf("service %+v", svc)
	}
	// The built-in engine is always wired and is the default; it waits for the Mac app.
	if svc.Builtin == nil || svc.TranscriptionEngine() != integrations.EngineBuiltin {
		t.Fatalf("builtin %v %q", svc.Builtin, svc.TranscriptionEngine())
	}
	if ok, code, _ := svc.Builtin.Status().Available(); ok || code != "no_provider" {
		t.Fatalf("builtin before the app connects: %v %s", ok, code)
	}
}
