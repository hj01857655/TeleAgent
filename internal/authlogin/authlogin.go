// Package authlogin implements passwordless token renewal through the
// TeleAgent (TeleAI UPMS) web login. Instead of decrypting the desktop
// client's token file every month, the user logs in once in a browser;
// the gateway captures the OAuth-style authorization code and exchanges
// it for a fresh JWT automatically.
//
// Flow:
//
//  1. GET /auth/login            → 302 to the UPMS authorize page, carrying
//     redirect_uri = http(s)://<gateway>/auth/callback
//  2. user authenticates         (phone + SMS, or WeChat QR on teleai.com.cn)
//  3. UPMS redirects to our      /auth/callback?code=...&state=...
//  4. gateway exchanges code →   GET https://agent.teleai.com.cn/superCowork/sapi/cowork/auth?code=...
//     fresh JWT, updates live credentials and persists them to a state file.
package authlogin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"teleagent2api/internal/config"
)

const (
	upmsAuthorizeBase = "https://www.teleai.com.cn/teleai-api/upms/public/auth/authorize"
	oauthClientID     = "Z2veLLEHhZfq3hb5hWIwqJuPajIuasYi"
	exchangePath      = "/superCowork/sapi/cowork/auth"
	stateTTL          = 15 * time.Minute
)

// jwtRe matches a JWT anywhere in a response body or URL.
var jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)

// Service drives the web-login flow and persists the resulting credentials.
type Service struct {
	live    *config.Live
	client  *http.Client
	mu      sync.Mutex
	pending map[string]time.Time // state → expiry
}

// NewService wires the login flow to the live configuration.
func NewService(live *config.Live, client *http.Client) *Service {
	return &Service{live: live, client: client, pending: make(map[string]time.Time)}
}

// CallbackPath is the public redirect target suffix (combined with the
// gateway's public URL or the request Host).
const CallbackPath = "/auth/callback"

// AuthorizeURL builds the UPMS login URL for the given absolute callback URI.
func AuthorizeURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("client_id", oauthClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("response_type", "code")
	return upmsAuthorizeBase + "?" + q.Encode()
}

// StartLogin registers a CSRF state token and returns the login URL plus state.
func (s *Service) StartLogin(redirectURI string) (loginURL, state string) {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	state = hex.EncodeToString(buf)

	s.mu.Lock()
	// GC expired states.
	now := time.Now()
	for k, exp := range s.pending {
		if now.After(exp) {
			delete(s.pending, k)
		}
	}
	s.pending[state] = now.Add(stateTTL)
	s.mu.Unlock()

	return AuthorizeURL(redirectURI, state), state
}

// consumeState validates and burns a CSRF state token.
func (s *Service) consumeState(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.pending[state]
	if !ok || time.Now().After(exp) {
		return false
	}
	delete(s.pending, state)
	return true
}

// HandleCallback validates state, exchanges the code, updates live credentials
// and persists them. It returns the acquired credential snapshot.
func (s *Service) HandleCallback(ctx context.Context, code, state string) (config.Config, error) {
	if code == "" {
		return config.Config{}, fmt.Errorf("missing code parameter")
	}
	if !s.consumeState(state) {
		return config.Config{}, fmt.Errorf("invalid or expired state (possible CSRF); please restart the login")
	}

	cfg := s.live.Get()
	token, extra, err := exchangeCode(ctx, s.client, cfg.BaseURL, code)
	if err != nil {
		return config.Config{}, err
	}

	// Device identifiers: prefer values returned by the exchange, fall back
	// to whatever is already configured.
	deviceID := firstNonEmpty(extra["deviceId"], cfg.DeviceID)
	installID := firstNonEmpty(extra["installId"], cfg.InstallID)

	s.live.UpdateCreds(token, deviceID, installID)
	updated := s.live.Get()

	if cfg.StateFilePath != "" {
		if err := SaveState(cfg.StateFilePath, token, deviceID, installID); err != nil {
			// Non-fatal: credentials are live, persistence is best-effort.
			slog.Error("authlogin: persist state", slog.String("error", err.Error()))
		} else {
			slog.Info("authlogin: credentials persisted", slog.String("file", cfg.StateFilePath))
		}
	}
	return updated, nil
}

// exchangeCode performs the code→JWT exchange at the superCowork gateway.
func exchangeCode(ctx context.Context, client *http.Client, baseURL, code string) (token string, extra map[string]string, err error) {
	u := strings.TrimRight(baseURL, "/") + exchangePath + "?code=" + url.QueryEscape(code)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json, text/plain, */*")

	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("exchange request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Strategy 1: JSON payload with token fields.
	var envelope struct {
		Token       string          `json:"token"`
		AccessToken string          `json:"access_token"`
		Data        json.RawMessage `json:"data"`
		Error       string          `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Error != "" {
			return "", nil, fmt.Errorf("exchange rejected: %s", envelope.Error)
		}
		if envelope.Token != "" {
			return envelope.Token, nil, nil
		}
		if envelope.AccessToken != "" {
			return envelope.AccessToken, nil, nil
		}
		if len(envelope.Data) > 0 {
			var d map[string]any
			if json.Unmarshal(envelope.Data, &d) == nil {
				if t := findToken(d); t != "" {
					return t, flatStrings(d), nil
				}
			}
		}
	}

	// Strategy 2: JWT embedded anywhere in the body or redirect chain.
	if m := jwtRe.FindString(string(body)); m != "" {
		return m, nil, nil
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if m := jwtRe.FindString(loc); m != "" {
			return m, nil, nil
		}
	}

	return "", nil, fmt.Errorf("no token in exchange response (status %d, body %.120s)", resp.StatusCode, truncate120(body))
}

// findToken looks for common token keys in a decoded object.
func findToken(m map[string]any) string {
	for _, k := range []string{"token", "access_token", "accessToken", "jwt", "id_token"} {
		if v, ok := m[k].(string); ok && strings.HasPrefix(v, "eyJ") {
			return v
		}
	}
	return ""
}

// flatStrings extracts string members of a map (for device identifiers).
func flatStrings(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok && s != "" {
			out[k] = s
		}
	}
	return out
}

// StateData is the persisted credential file.
type StateData struct {
	Token     string `json:"token"`
	DeviceID  string `json:"deviceId"`
	InstallID string `json:"installId"`
	SavedAt   string `json:"savedAt"`
	Source    string `json:"source"`
}

// SaveState atomically writes credentials to path.
func SaveState(path, token, deviceID, installID string) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(StateData{
		Token:     token,
		DeviceID:  deviceID,
		InstallID: installID,
		SavedAt:   time.Now().Format(time.RFC3339),
		Source:    "web-login",
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadState reads a previously persisted credential file.
func LoadState(path string) (StateData, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return StateData{}, false
	}
	var d StateData
	if err := json.Unmarshal(raw, &d); err != nil || d.Token == "" {
		return StateData{}, false
	}
	return d, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate120(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
