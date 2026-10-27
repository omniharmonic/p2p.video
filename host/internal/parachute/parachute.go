// Package parachute saves p2p.video session notes into a Parachute vault
// through its REST API (<hub>/vault/<name>/api/notes), using the same note
// conventions as Minutes so session notes read as native meeting notes.
//
// Credentials: by default a short-lived JWT is minted at send time with the
// local `parachute auth mint-token --ephemeral` (argv only), so p2p.video
// stores no vault secret. A token file is an opt-in fallback and must be a
// 0600 regular file owned by the daemon's user. Tokens never appear in logs,
// errors or status responses.
package parachute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// TokenSource supplies a bearer token for one request batch.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Runner executes one program with arguments (argv only).
type Runner interface {
	Run(ctx context.Context, program string, args []string, env []string) (stdout, stderr []byte, err error)
}

// ExecRunner runs programs with os/exec.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, program string, args []string, env []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Env = &out, &errb, env
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// MintArgs is the exact argv used to mint a write token for one vault.
func MintArgs(vault string) []string {
	return []string{"auth", "mint-token", "--scope", "vault:" + vault + ":write", "--ephemeral", "--label", "studiolink"}
}

// CLITokenSource mints an ephemeral (1 h) token per send with the local
// parachute CLI, the same mechanism Minutes' CLI adapter uses.
type CLITokenSource struct {
	Path   string
	Vault  string
	Runner Runner
}

// Token implements TokenSource.
func (s CLITokenSource) Token(ctx context.Context) (string, error) {
	runner := s.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, _, err := runner.Run(ctx, s.Path, MintArgs(s.Vault), childEnv(s.Path))
	if err != nil {
		// stderr can echo arguments or partial output; never surface it.
		return "", fmt.Errorf("parachute auth mint-token failed: %w", err)
	}
	token := strings.TrimSpace(string(stdout))
	if i := strings.LastIndexByte(token, '\n'); i >= 0 {
		token = strings.TrimSpace(token[i+1:])
	}
	if !looksLikeJWT(token) {
		return "", errors.New("parachute auth mint-token returned no token")
	}
	return token, nil
}

func looksLikeJWT(s string) bool {
	return strings.Count(s, ".") == 2 && len(s) > 20 && !strings.ContainsAny(s, " \t\r\n")
}

// childEnv keeps the CLI's own directory on PATH (the bun-installed parachute
// CLI resolves `bun` beside itself) and drops p2p.video's secrets.
func childEnv(cli string) []string {
	env := []string{}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	path := filepath.Dir(cli)
	if p := os.Getenv("PATH"); p != "" {
		path += string(os.PathListSeparator) + p
	} else {
		path += ":/usr/bin:/bin"
	}
	return append(env, "PATH="+path)
}

// FileTokenSource reads a token from a file that only the daemon's user can read.
type FileTokenSource struct{ Path string }

// Token implements TokenSource.
func (s FileTokenSource) Token(context.Context) (string, error) {
	info, err := os.Lstat(s.Path)
	if err != nil {
		return "", fmt.Errorf("parachute token file unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("parachute token file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("parachute token file must not be readable by group or others (chmod 600)")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "", errors.New("parachute token file must be owned by the p2p.video user")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read parachute token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("parachute token file is empty or malformed")
	}
	return token, nil
}

// DiscoverCLI finds the parachute CLI: configured path, $PATH, then common
// install locations (bun global install first, as the hub documents).
func DiscoverCLI(configured string, lookPath func(string) (string, error), isExec func(string) bool, home string) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) || !isExec(configured) {
			return "", fmt.Errorf("parachute CLI not found at configured path %s", configured)
		}
		return configured, nil
	}
	if lookPath != nil {
		if p, err := lookPath("parachute"); err == nil && filepath.IsAbs(p) {
			return p, nil
		}
	}
	candidates := []string{"/opt/homebrew/bin/parachute", "/usr/local/bin/parachute"}
	if home != "" {
		candidates = append([]string{filepath.Join(home, ".bun/bin/parachute")}, candidates...)
	}
	for _, c := range candidates {
		if isExec(c) {
			return c, nil
		}
	}
	return "", errors.New("parachute CLI not found")
}

// Client talks to one vault.
type Client struct {
	// Origin is the hub origin, e.g. https://hub.example.com or http://127.0.0.1:1940.
	Origin string
	Vault  string
	Tokens TokenSource
	HTTP   *http.Client
}

// Note is one vault note. Metadata values are strings (Minutes convention).
type Note struct {
	Path     string            `json:"path"`
	Content  string            `json:"content"`
	Tags     []string          `json:"tags"`
	Metadata map[string]string `json:"metadata"`
}

// Receipt identifies a saved note so a re-send updates it in place.
type Receipt struct {
	ID        string `json:"id"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Path      string `json:"path,omitempty"`
}

// ErrEditedInVault means the note changed in the vault since the last send;
// p2p.video leaves it unchanged (Minutes' rule).
var ErrEditedInVault = errors.New("the session note was edited in the vault since the last send, so p2p.video left it unchanged")

// HTTPError is a non-success vault response. Message is the server's own
// short message, never request headers.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("parachute request failed (HTTP %d)", e.Status)
	}
	return fmt.Sprintf("parachute request failed (HTTP %d): %s", e.Status, e.Message)
}

// ValidateOrigin accepts https origins, or http only on loopback (a local hub).
func ValidateOrigin(origin string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(origin), "/"))
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("parachute server must be an origin such as https://hub.example.com")
	}
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, errors.New("parachute server must use https (http is allowed only on loopback)")
	}
	u.Path = ""
	return u, nil
}

// ValidVault accepts the vault names Parachute uses in URLs and scopes.
func ValidVault(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// VaultURL is the browsable vault base (no credentials).
func (c *Client) VaultURL() string {
	u, err := ValidateOrigin(c.Origin)
	if err != nil {
		return ""
	}
	return u.String() + "/vault/" + url.PathEscape(c.Vault)
}

func (c *Client) notesURL() (string, error) {
	u, err := ValidateOrigin(c.Origin)
	if err != nil {
		return "", err
	}
	if !ValidVault(c.Vault) {
		return "", errors.New("invalid parachute vault name")
	}
	return u.String() + "/vault/" + c.Vault + "/api/notes", nil
}

// SaveNote creates the note, or updates the previously saved one. Like
// Minutes, an update carries if_updated_at so edits made in the vault are
// never overwritten; a note deleted in the vault is created again; a path
// collision on create keeps both notes.
func (c *Client) SaveNote(ctx context.Context, note Note, previous *Receipt) (Receipt, error) {
	base, err := c.notesURL()
	if err != nil {
		return Receipt{}, err
	}
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return Receipt{}, err
	}
	if previous != nil && previous.ID != "" {
		body := map[string]any{"content": note.Content, "metadata": note.Metadata, "path": note.Path}
		if previous.UpdatedAt != "" {
			body["if_updated_at"] = previous.UpdatedAt
		} else {
			body["force"] = true
		}
		r, err := c.send(ctx, http.MethodPatch, base+"/"+url.PathEscape(previous.ID), token, body)
		var he *HTTPError
		switch {
		case err == nil:
			if r.Path == "" {
				r.Path = note.Path
			}
			return r, nil
		case errors.As(err, &he) && he.Status == http.StatusConflict:
			return Receipt{}, ErrEditedInVault
		case errors.As(err, &he) && he.Status == http.StatusNotFound:
			// Deleted in the vault: create it again below.
		default:
			return Receipt{}, err
		}
	}
	path := note.Path
	for attempt := 0; attempt < 2; attempt++ {
		body := map[string]any{"path": path, "content": note.Content, "tags": note.Tags, "metadata": note.Metadata}
		r, err := c.send(ctx, http.MethodPost, base, token, body)
		var he *HTTPError
		if err == nil {
			if r.Path == "" {
				r.Path = path
			}
			return r, nil
		}
		if attempt == 0 && errors.As(err, &he) && he.Status == http.StatusConflict {
			suffix := note.Metadata["studiolink_take_id"]
			if len(suffix) > 6 {
				suffix = suffix[:6]
			}
			if suffix == "" {
				suffix = "2"
			}
			path = note.Path + " (" + suffix + ")"
			continue
		}
		return Receipt{}, err
	}
	return Receipt{}, &HTTPError{Status: http.StatusConflict, Message: "a note already exists at " + note.Path}
}

func (c *Client) send(ctx context.Context, method, target, token string, body any) (Receipt, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return Receipt{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
	if err != nil {
		return Receipt{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// url.Error carries only method and URL (no headers); still scrub.
		return Receipt{}, errors.New(scrub(fmt.Sprintf("parachute request failed: %v", err), token))
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Receipt{}, &HTTPError{Status: resp.StatusCode, Message: "the vault refused the credential (check the vault name and that the token has vault:write)"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var msg struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(payload, &msg)
		text := msg.Message
		if text == "" {
			text = msg.Error
		}
		if r := []rune(text); len(r) > 200 {
			text = string(r[:200])
		}
		return Receipt{}, &HTTPError{Status: resp.StatusCode, Message: scrub(text, token)}
	}
	return parseReceipt(payload)
}

func scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[redacted]")
}

func parseReceipt(data []byte) (Receipt, error) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return Receipt{}, errors.New("parachute returned an unreadable response")
	}
	note := obj
	if inner, ok := obj["note"].(map[string]any); ok {
		note = inner
	}
	id, _ := note["id"].(string)
	if id == "" {
		return Receipt{}, errors.New("parachute response is missing the note id")
	}
	updated, _ := note["updatedAt"].(string)
	if updated == "" {
		updated, _ = note["updated_at"].(string)
	}
	path, _ := note["path"].(string)
	return Receipt{ID: id, UpdatedAt: updated, Path: path}, nil
}
