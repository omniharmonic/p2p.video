package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status represents relevant fields from `tailscale status --json`.
type Status struct {
	Self struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Self"`
	MagicDNSSuffix string   `json:"MagicDNSSuffix"`
	CertDomains    []string `json:"CertDomains"`
}

// Client interacts with the local Tailscale CLI / daemon.
type Client struct{}

// NewClient creates a new Tailscale helper client.
func NewClient() *Client {
	return &Client{}
}

// command also supports the normal macOS application install, whose CLI is not
// necessarily on Finder's PATH. CLI mode avoids opening its GUI from the host.
func command(args ...string) ([]byte, error) {
	executable, err := findExecutable(exec.LookPath, os.Stat, runtime.GOOS, os.UserHomeDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Env = append(os.Environ(), "TAILSCALE_BE_CLI=1")
	return cmd.Output()
}

func findExecutable(lookPath func(string) (string, error), stat func(string) (os.FileInfo, error), platform string, home func() (string, error)) (string, error) {
	if path, err := lookPath("tailscale"); err == nil {
		return path, nil
	}
	if platform == "darwin" {
		candidates := []string{"/Applications/Tailscale.app/Contents/MacOS/Tailscale"}
		if dir, err := home(); err == nil {
			candidates = append(candidates, filepath.Join(dir, "Applications/Tailscale.app/Contents/MacOS/Tailscale"))
		}
		for _, path := range candidates {
			if info, err := stat(path); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("Tailscale is not installed; install and connect Tailscale, then recheck")
}

// CheckStatus retrieves status from the local Tailscale instance.
func (c *Client) CheckStatus() (*Status, error) {
	out, err := command("status", "--json")
	if err != nil {
		return nil, fmt.Errorf("tailscale status failed: %w", err)
	}

	var s Status
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	return &s, nil
}

// GetInviteURL builds the guest invite link for a session.
func (c *Client) GetInviteURL(baseURL string, sessionID string, inviteToken string) string {
	cleanBase := strings.TrimRight(baseURL, "/")
	v := url.Values{}
	v.Set("session", sessionID)
	v.Set("token", inviteToken)
	return fmt.Sprintf("%s/join?%s", cleanBase, v.Encode())
}

// DiscoverGuestURL returns only an existing HTTPS Serve route to this daemon.
// It never changes Serve/Funnel or manufactures Tailscale credentials.
func (c *Client) DiscoverGuestURL(port int) (string, error) {
	out, err := command("serve", "status", "--json")
	if err != nil {
		return "", fmt.Errorf("read Tailscale Serve: %w", err)
	}
	return guestURLFromServe(out, port)
}

func guestURLFromServe(data []byte, port int) (string, error) {
	var cfg struct {
		Web map[string]struct {
			Handlers map[string]struct{ Proxy string }
		}
		AllowFunnel map[string]bool
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse Serve: %w", err)
	}
	hosts := make([]string, 0, len(cfg.Web))
	for host := range cfg.Web {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		if cfg.AllowFunnel[host] {
			continue
		}
		handler, ok := cfg.Web[host].Handlers["/"]
		if !ok {
			continue
		}
		u, err := url.Parse(handler.Proxy)
		if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") || u.Port() != strconv.Itoa(port) {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			continue
		}
		name, p, err := net.SplitHostPort(host)
		if err != nil || !strings.HasSuffix(name, ".ts.net") {
			continue
		}
		if p == "443" {
			return "https://" + name, nil
		}
		return "https://" + host, nil
	}
	return "", fmt.Errorf("no private HTTPS Serve route to p2p.video port %d", port)
}
