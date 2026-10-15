// Package webui serves a lightweight single-page control panel for
// teleagent2api. The page itself is embedded in the binary and carries no
// secrets; all data is fetched from authenticated admin endpoints that
// return masked values only.
package webui

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"teleagent2api/internal/accounts"
	"teleagent2api/internal/apikeys"
	"teleagent2api/internal/checkin"
	"teleagent2api/internal/config"
	"teleagent2api/internal/proxy"
)

//go:embed static/index.html
var pageHTML []byte

// Handler serves the panel page and its admin APIs.
type Handler struct {
	live      *config.Live
	checkin   *checkin.Service
	store     *accounts.Store
	keys      *apikeys.Store
	pool      *proxy.ClientPool
	client    *http.Client
	startedAt time.Time
}

// NewHandler wires the panel to the live service.
func NewHandler(live *config.Live, ci *checkin.Service, store *accounts.Store, keys *apikeys.Store, pool *proxy.ClientPool, client *http.Client, startedAt time.Time) *Handler {
	return &Handler{live: live, checkin: ci, store: store, keys: keys, pool: pool, client: client, startedAt: startedAt}
}

// testEgress probes the public IP through an account's egress path.
func (h *Handler) testEgress(ctx context.Context, acc accounts.Account) (string, error) {
	var lastErr error
	for _, u := range egressEchoURLs {
		ip, err := h.pool.TestEgress(acc, u, 15*time.Second)
		if err != nil {
			lastErr = err
			continue
		}
		ip, _ = parseEcho(ip)
		if ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("所有出口探测均失败（最后错误：%v）", lastErr)
}

// cfg returns the current live configuration snapshot.
func (h *Handler) cfg() config.Config {
	return h.live.Get()
}

// ServePage renders the embedded single-file panel (login happens client-side
// against the regular API_KEY auth).
func (h *Handler) ServePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Conservative CSP: the page is fully self-contained (no external
	// scripts, styles or fonts).
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")
	_, _ = w.Write(pageHTML)
}

// infoResponse is the masked runtime snapshot shown on the dashboard.
type infoResponse struct {
	Service      serviceInfo  `json:"service"`
	Token        tokenInfo    `json:"token"`
	CheckIn      checkinInfo  `json:"checkin"`
	Proxy        proxyInfo    `json:"proxy"`
	WebLogin     webLoginInfo `json:"webLogin"`
	Models       []string     `json:"models"`
	AccountsFile string       `json:"accountsFile"`
}

type webLoginInfo struct {
	// LoginURL is the gateway entry the user opens to renew via browser.
	LoginURL string `json:"loginUrl"`
	// CallbackHint explains how the redirect_uri is derived.
	CallbackHint string `json:"callbackHint"`
	// StateFile is where renewed credentials are persisted (empty = none).
	StateFile string `json:"stateFile"`
	// FromStateFile reports whether the active token came from a web login.
	FromStateFile bool `json:"fromStateFile"`
}

type serviceInfo struct {
	Listen      string `json:"listen"`
	Upstream    string `json:"upstream"`
	AppVersion  string `json:"appVersion"`
	Uptime      string `json:"uptime"`
	RateLimit   int    `json:"rateLimitRpm"`
	SignEnabled bool   `json:"signEnabled"`
}

type tokenInfo struct {
	Hint       string `json:"hint"`
	Set        bool   `json:"set"`
	ExpiresAt  string `json:"expiresAt,omitempty"`
	DaysLeft   int    `json:"daysLeft"`
	Expired    bool   `json:"expired"`
	UnknownExp bool   `json:"unknownExp"`
}

type checkinInfo struct {
	Enabled  bool               `json:"enabled"`
	Schedule string             `json:"schedule"`
	Type     string             `json:"type"`
	Last     *checkin.RunResult `json:"last,omitempty"`
}

type proxyInfo struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	Platform string `json:"platform"`
	Account  string `json:"account"`
	TokenSet bool   `json:"tokenSet"`
}

// ServeInfo returns the masked runtime configuration. Sensitive values are
// never returned in full — only hints and booleans.
func (h *Handler) ServeInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tok := tokenInfo{Hint: maskHint(h.cfg().Token), Set: h.cfg().Token != ""}
	if h.cfg().Token != "" {
		if exp, ok := JWTExpiry(h.cfg().Token); ok {
			tok.ExpiresAt = exp.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04")
			tok.DaysLeft = int(time.Until(exp).Hours() / 24)
			tok.Expired = time.Now().After(exp)
		} else {
			tok.UnknownExp = true
		}
	}

	last := h.checkin.LastResult()
	resp := infoResponse{
		Service: serviceInfo{
			Listen:      h.cfg().Listen,
			Upstream:    h.cfg().BaseURL,
			AppVersion:  h.cfg().AppVersion,
			Uptime:      humanDuration(time.Since(h.startedAt)),
			RateLimit:   h.cfg().RateLimitRPM,
			SignEnabled: h.cfg().SignEnabled,
		},
		Token: tok,
		CheckIn: checkinInfo{
			Enabled:  h.cfg().CheckInEnabled,
			Schedule: h.cfg().CheckInTime + " Asia/Shanghai",
			Type:     h.cfg().CheckInType,
			Last:     &last,
		},
		Proxy: proxyInfo{
			Enabled:  h.cfg().ProxyEnabled(),
			URL:      h.cfg().RedactedProxyURL(),
			Platform: h.cfg().ProxyPlatform,
			Account:  h.cfg().ProxyAccount,
			TokenSet: h.cfg().ProxyToken != "",
		},
		WebLogin: webLoginInfo{
			LoginURL:      "/auth/login",
			CallbackHint:  "登录成功后浏览器会跳转回本网关完成兑换；如网关在反向代理后，请设置 TELEAGENT_PUBLIC_URL",
			StateFile:     h.cfg().StateFilePath,
			FromStateFile: h.cfg().FromStateFile,
		},
		Models:       h.cfg().Models,
		AccountsFile: h.cfg().AccountsFilePath,
	}
	writeJSON(w, http.StatusOK, resp)
}

// egressEchoURLs are tried in order for the proxy connectivity test. The
// first two return a bare IP; ipip.net additionally returns the Chinese
// geographic attribution. Multiple providers keep the test working behind
// restrictive networks where one of them is blocked.
var egressEchoURLs = []string{
	"https://ifconfig.me/ip",
	"https://ipinfo.io/ip",
	"https://myip.ipip.net",
	"https://api.ip.sb/ip",
	"https://v4.ident.me",
}

// ServeProxyTest performs a live egress check: it fetches the public IP
// through the gateway's own transport, proving whether the configured
// Resin-style proxy is actually in effect.
func (h *Handler) ServeProxyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var lastErr error
	for _, url := range egressEchoURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := h.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
			continue
		}
		ip, geo := parseEcho(string(body))
		if ip == "" {
			lastErr = fmt.Errorf("%s returned unparseable body", url)
			continue
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":   true,
			"ip":   ip,
			"geo":  geo,
			"via":  h.viaLabel(),
			"note": "若出口 IP 与服务器本身不同，说明流量正通过代理池发出",
		})
		return
	}

	writeJSON(w, http.StatusBadGateway, map[string]any{
		"ok":    false,
		"error": fmt.Sprintf("all egress probes failed (last: %v); is upstream/proxy reachable?", lastErr),
		"via":   h.viaLabel(),
	})
}

// parseEcho extracts the IP (and optional geo line) from an echo body.
func parseEcho(body string) (ip, geo string) {
	s := strings.TrimSpace(body)
	if strings.HasPrefix(s, "当前 IP：") || strings.HasPrefix(s, "当前IP:") {
		// "当前 IP：1.2.3.4  来自于：中国 福建 厦门  电信"
		rest := strings.TrimPrefix(strings.TrimPrefix(s, "当前 IP："), "当前IP:")
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			ip = fields[0]
		}
		if idx := strings.Index(rest, "来自于："); idx >= 0 {
			geo = strings.TrimSpace(rest[idx+len("来自于："):])
		}
		return ip, geo
	}
	// Bare IP (possibly with trailing newline / whitespace).
	return strings.Fields(s)[0], ""
}

func (h *Handler) viaLabel() string {
	if h.cfg().ProxyEnabled() {
		return "proxy (" + h.cfg().RedactedProxyURL() + ")"
	}
	return "direct"
}

// JWTExpiry decodes the exp claim of a JWT without verifying the signature
// (the token is validated by the upstream anyway; we only display metadata).
func JWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return time.Time{}, false
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		// Some encoders omit padding handling differences; try raw.
		decoded, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "="))
		if err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func maskHint(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return s[:2] + "……"
	}
	return s[:6] + "……" + s[len(s)-4:]
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%d 天 %d 小时 %d 分", days, hours, mins)
	}
	if hours > 0 {
		return fmt.Sprintf("%d 小时 %d 分", hours, mins)
	}
	return fmt.Sprintf("%d 分钟", mins)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("webui: write json", slog.String("error", err.Error()))
	}
}
