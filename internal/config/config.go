package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token          string `json:"-"`
	DeviceID       string `json:"deviceId"`
	InstallID      string `json:"-"`
	APIKey         string `json:"-"`
	UpstreamAPIKey string `json:"-"`
	RouteTarget    string `json:"-"`
	SignEnabled    bool   `json:"-"`
	// Auto check-in (TeleAgent Star Welfare Club / 积分福利社)
	CheckInEnabled bool   `json:"-"`
	CheckInTime    string `json:"-"`
	CheckInType    string `json:"-"`
	CheckInUserID  string `json:"-"`
	// Upstream egress proxy (Resin-style proxy pool gateway)
	ProxyURL      string `json:"-"`
	ProxyPlatform string `json:"-"`
	ProxyAccount  string `json:"-"`
	ProxyToken    string `json:"-"`
	// RateLimitRPM caps requests per minute per API key (0 = disabled).
	RateLimitRPM int `json:"-"`
	// StateFilePath persists web-login credential updates across restarts.
	StateFilePath string `json:"-"`
	// AccountsFilePath persists the multi-account pool.
	AccountsFilePath string `json:"-"`
	// KeysFilePath persists the client API key set.
	KeysFilePath string `json:"-"`
	// AdminPassword guards the console; separate from client API keys.
	AdminPassword string `json:"-"`
	// FromStateFile reports the active credentials came from the state file
	// (written by web login) rather than environment variables.
	FromStateFile bool `json:"-"`
	// PublicURL is the gateway's externally reachable base (used to build
	// the web-login callback URL when behind a reverse proxy).
	PublicURL  string        `json:"-"`
	BaseURL    string        `json:"baseURL,omitempty"`
	AppVersion string        `json:"appVersion,omitempty"`
	UserAgent  string        `json:"userAgent,omitempty"`
	Listen     string        `json:"listen,omitempty"`
	Models     []string      `json:"models,omitempty"`
	Timeout    time.Duration `json:"timeout,omitempty"`
	LogLevel   string        `json:"logLevel,omitempty"`
	LogFormat  string        `json:"logFormat,omitempty"`
	RetryCount int           `json:"retryCount,omitempty"`
	// ReasoningMode decides where the model's <think> block is delivered. See
	// adapter.NormalizeMode for the accepted values.
	ReasoningMode string `json:"reasoningMode,omitempty"`
	// ExtraFields extends the request allowlist beyond the built-in set, for
	// clients whose parameters the upstream does accept.
	ExtraFields []string `json:"extraFields,omitempty"`
	// ModelLimits caps max_tokens per model id; an absent entry means no cap.
	ModelLimits map[string]int `json:"modelLimits,omitempty"`
}

// fileConfig mirrors Config but allows deserializing sensitive fields from file.
type fileConfig struct {
	Token          string         `json:"token"`
	DeviceID       string         `json:"deviceId"`
	InstallID      string         `json:"installId"`
	APIKey         string         `json:"apiKey"`
	UpstreamAPIKey string         `json:"upstreamApiKey"`
	BaseURL        string         `json:"baseURL"`
	AppVersion     string         `json:"appVersion"`
	UserAgent      string         `json:"userAgent"`
	Listen         string         `json:"listen"`
	Models         []string       `json:"models"`
	Timeout        string         `json:"timeout"`
	LogLevel       string         `json:"logLevel"`
	LogFormat      string         `json:"logFormat"`
	RetryCount     int            `json:"retryCount"`
	ReasoningMode  string         `json:"reasoningMode"`
	ExtraFields    []string       `json:"extraFields"`
	ModelLimits    map[string]int `json:"modelLimits"`
}

func Load() Config {
	// Phase 1: defaults + config file
	c := Config{
		// No built-in default: this is the upstream's application identifier, and
		// shipping one in source means every deployment shares (and publishes) the
		// same credential-shaped string. Supply it via TELEAGENT_UPSTREAM_KEY or
		// config.json. An empty value omits the Authorization header entirely,
		// which the upstream treats the same as its own built-in value — see
		// proxy.BuildRequest.
		UpstreamAPIKey:   "",
		RouteTarget:      "ops-gateway",
		SignEnabled:      false,
		CheckInType:      "daily_login",
		CheckInTime:      "08:00",
		RateLimitRPM:     60,
		StateFilePath:    "teleagent-token-state.json",
		AccountsFilePath: "teleagent-accounts.json",
		KeysFilePath:     "teleagent-keys.json",
		BaseURL:          "https://agent.teleai.com.cn",
		AppVersion:       "2.7.0",
		UserAgent:        "opencode/1.2.27 ai-sdk/provider-utils/3.0.20 runtime/bun/1.3.10",
		Listen:           ":10000",
		Timeout:          120 * time.Second,
		LogLevel:         "info",
		LogFormat:        "text",
		RetryCount:       0,
		ReasoningMode:    "content",
		ModelLimits:      defaultModelLimits(),
	}

	configPath := getEnv("TELEAGENT_CONFIG", "config.json")
	if data, err := os.ReadFile(configPath); err == nil {
		var fc fileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			slog.Error("config file parse error, ignoring file",
				slog.String("path", configPath),
				slog.String("error", err.Error()),
			)
		} else {
			slog.Info("loaded config file", slog.String("path", configPath))
			if fc.Token != "" {
				c.Token = fc.Token
			}
			if fc.DeviceID != "" {
				c.DeviceID = fc.DeviceID
			}
			if fc.InstallID != "" {
				c.InstallID = fc.InstallID
			}
			if fc.APIKey != "" {
				c.APIKey = fc.APIKey
			}
			if fc.UpstreamAPIKey != "" {
				c.UpstreamAPIKey = fc.UpstreamAPIKey
			}
			if fc.BaseURL != "" {
				c.BaseURL = fc.BaseURL
			}
			if fc.AppVersion != "" {
				c.AppVersion = fc.AppVersion
			}
			if fc.UserAgent != "" {
				c.UserAgent = fc.UserAgent
			}
			if fc.Listen != "" {
				c.Listen = fc.Listen
			}
			if len(fc.Models) > 0 {
				c.Models = fc.Models
			}
			if fc.Timeout != "" {
				if d, err := time.ParseDuration(fc.Timeout); err == nil {
					c.Timeout = d
				}
			}
			if fc.LogLevel != "" {
				c.LogLevel = fc.LogLevel
			}
			if fc.LogFormat != "" {
				c.LogFormat = fc.LogFormat
			}
			if fc.RetryCount > 0 {
				c.RetryCount = fc.RetryCount
			}
			if fc.ReasoningMode != "" {
				c.ReasoningMode = fc.ReasoningMode
			}
			if len(fc.ExtraFields) > 0 {
				c.ExtraFields = fc.ExtraFields
			}
			for model, limit := range fc.ModelLimits {
				c.ModelLimits[model] = limit
			}
		}
	}

	// Phase 2: environment variables override file values (12-factor compliance)
	if v, ok := os.LookupEnv("TELEAGENT_TOKEN"); ok {
		c.Token = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_DEVICE_ID"); ok {
		c.DeviceID = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_INSTALL_ID"); ok {
		c.InstallID = v
	}
	if v, ok := os.LookupEnv("API_KEY"); ok {
		c.APIKey = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_UPSTREAM_KEY"); ok {
		c.UpstreamAPIKey = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_ROUTE_TARGET"); ok {
		c.RouteTarget = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_SIGN"); ok {
		// Legacy HMAC signing (superagent-auth-v1). The current TeleAgent
		// server no longer requires it; enabled only for backward compat.
		c.SignEnabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := os.LookupEnv("CHECKIN_ENABLED"); ok {
		c.CheckInEnabled = v == "1" || strings.EqualFold(v, "true")
	}
	if v, ok := os.LookupEnv("CHECKIN_TIME"); ok {
		if t, err := time.Parse("15:04", strings.TrimSpace(v)); err == nil {
			c.CheckInTime = t.Format("15:04")
		}
	}
	if v, ok := os.LookupEnv("CHECKIN_TYPE"); ok && strings.TrimSpace(v) != "" {
		c.CheckInType = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_USER_ID"); ok {
		c.CheckInUserID = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_PROXY"); ok {
		c.ProxyURL = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_PROXY_PLATFORM"); ok {
		c.ProxyPlatform = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_PROXY_ACCOUNT"); ok {
		c.ProxyAccount = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_PROXY_TOKEN"); ok {
		c.ProxyToken = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("RATE_LIMIT_RPM"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			c.RateLimitRPM = n
		}
	}
	if v, ok := os.LookupEnv("TELEAGENT_STATE_FILE"); ok && strings.TrimSpace(v) != "" {
		c.StateFilePath = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_PUBLIC_URL"); ok {
		c.PublicURL = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_ACCOUNTS_FILE"); ok && strings.TrimSpace(v) != "" {
		c.AccountsFilePath = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_KEYS_FILE"); ok && strings.TrimSpace(v) != "" {
		c.KeysFilePath = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("ADMIN_PASSWORD"); ok {
		c.AdminPassword = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_BASE_URL"); ok {
		c.BaseURL = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_APP_VERSION"); ok {
		c.AppVersion = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_USER_AGENT"); ok {
		c.UserAgent = v
	}
	if v, ok := os.LookupEnv("TELEAGENT2API_LISTEN"); ok {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_TIMEOUT"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			c.Timeout = d
		}
	}
	if v, ok := os.LookupEnv("TELEAGENT_LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_LOG_FORMAT"); ok {
		c.LogFormat = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_RETRY_COUNT"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.RetryCount = n
		}
	}
	if v, ok := os.LookupEnv("TELEAGENT_REASONING_MODE"); ok && strings.TrimSpace(v) != "" {
		c.ReasoningMode = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("TELEAGENT_EXTRA_FIELDS"); ok {
		for _, field := range strings.Split(v, ",") {
			if field = strings.TrimSpace(field); field != "" {
				c.ExtraFields = append(c.ExtraFields, field)
			}
		}
	}
	// JSON rather than a CSV: the value is a model→limit map, which has no
	// unambiguous flat spelling. Malformed input is ignored rather than fatal,
	// so a typo in one optional tuning knob cannot stop the gateway from booting.
	if v := strings.TrimSpace(getEnv("TELEAGENT_MODEL_LIMITS", "")); v != "" {
		var limits map[string]int
		if err := json.Unmarshal([]byte(v), &limits); err != nil {
			slog.Warn("TELEAGENT_MODEL_LIMITS is not valid JSON; using built-in limits",
				slog.String("value", v), slog.String("error", err.Error()))
		} else {
			for model, limit := range limits {
				c.ModelLimits[model] = limit
			}
		}
	}

	if len(c.Models) == 0 {
		modelsStr := getEnv("TELEAGENT_MODELS", "chat-nano,chat-lite,chat-pro,chat-flash")
		for _, m := range strings.Split(modelsStr, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.Models = append(c.Models, m)
			}
		}
	}

	return c
}

// RedactedProxyURL returns the proxy URL with any embedded credentials
// replaced, safe for logs and diagnostics.
func (c Config) RedactedProxyURL() string {
	u := c.ProxyURL
	if u == "" {
		return ""
	}
	if at := strings.Index(u, "@"); at >= 0 {
		if schemeEnd := strings.Index(u, "://"); schemeEnd >= 0 && schemeEnd+3 <= at {
			return u[:schemeEnd+3] + "***@" + u[at+1:]
		}
		return "***@" + u[at+1:]
	}
	return u
}

// SafeSummary returns a human-readable summary with sensitive fields redacted.
func (c Config) SafeSummary() string {
	tokenHint := ""
	if c.Token != "" {
		tokenHint = fmt.Sprintf("%s…%s", c.Token[:min(4, len(c.Token))], c.Token[max(0, len(c.Token)-4):])
	}
	apiKeyHint := ""
	if c.APIKey != "" {
		apiKeyHint = fmt.Sprintf("%s…", c.APIKey[:min(4, len(c.APIKey))])
	}
	return fmt.Sprintf("listen=%s upstream=%s token=%s apiKey=%s models=%v timeout=%s logLevel=%s retryCount=%d reasoningMode=%s checkin=%s@%s proxy=%s",
		c.Listen, c.BaseURL, tokenHint, apiKeyHint, c.Models, c.Timeout, c.LogLevel, c.RetryCount,
		c.ReasoningMode, c.CheckInType, c.CheckInTime, c.RedactedProxyURL())
}

// PortalBaseURL returns the TeleAgent user-portal (Star Welfare Club) API base,
// which sits behind the same superCowork/sapi gateway as the chat endpoint.
func (c Config) PortalBaseURL() string {
	return strings.TrimRight(c.BaseURL, "/") + "/superCowork/sapi"
}

// ProxyEnabled reports whether an upstream egress proxy is configured.
func (c Config) ProxyEnabled() bool {
	return strings.TrimSpace(c.ProxyURL) != ""
}

// SlogLevel converts the string log level to slog.Level.
func (c Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// defaultModelLimits is the max_tokens ceiling per model, taken from what each
// model reports as its own output cap. A model that is absent here is left
// uncapped: guessing a limit for an unknown model would silently truncate
// answers, which is worse than letting the upstream refuse the request.
func defaultModelLimits() map[string]int {
	return map[string]int{
		"chat-nano":  16384,
		"chat-lite":  32768,
		"chat-pro":   65536,
		"chat-flash": 65536,
	}
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
