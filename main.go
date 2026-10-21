package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"teleagent2api/internal/accounts"
	"teleagent2api/internal/apikeys"
	"teleagent2api/internal/authlogin"
	"teleagent2api/internal/checkin"
	"teleagent2api/internal/config"
	"teleagent2api/internal/handler"
	"teleagent2api/internal/middleware"
	"teleagent2api/internal/proxy"
	"teleagent2api/internal/webui"
)

var startedAt = time.Now()

func main() {
	cfg := config.Load()

	initLogger(cfg)

	// Web-login state file overrides environment credentials when present:
	// it is only written by a deliberate browser login, so it always carries
	// the freshest JWT. Delete the file (or clear TELEAGENT_STATE_FILE) to
	// fall back to the values from .env.
	if cfg.StateFilePath != "" {
		if st, ok := authlogin.LoadState(cfg.StateFilePath); ok {
			cfg.Token = st.Token
			if st.DeviceID != "" {
				cfg.DeviceID = st.DeviceID
			}
			if st.InstallID != "" {
				cfg.InstallID = st.InstallID
			}
			cfg.FromStateFile = true
			slog.Info("credentials loaded from web-login state file",
				slog.String("file", cfg.StateFilePath),
				slog.String("savedAt", st.SavedAt))
		}
	}

	live := config.NewLive(cfg)

	// Multi-account pool: seeded from the environment on first run so that
	// existing single-account deployments keep working unchanged.
	acctStore := accounts.NewStore(cfg.AccountsFilePath)
	acctStore.SeedFromEnv(cfg.Token, cfg.DeviceID, cfg.InstallID)
	if n := acctStore.Len(); n > 0 {
		slog.Info("account pool loaded", slog.Int("accounts", n), slog.String("file", cfg.AccountsFilePath))
	}

	if acctStore.Len() == 0 {
		// Degraded start: an empty account pool is NOT fatal, otherwise the
		// panel referenced in the message below could never be reached (the
		// process would exit before the HTTP server ever listens, and
		// `restart: unless-stopped` would turn that into a crash loop).
		// The console stays reachable so the operator can add an account;
		// the data plane answers 503 until one exists.
		slog.Warn("no account configured — starting in degraded mode (console reachable, data plane disabled); add one in the web panel (/ui → 账号) or set TELEAGENT_TOKEN/TELEAGENT_INSTALL_ID/TELEAGENT_DEVICE_ID")
	}

	if cfg.APIKey == "" {
		slog.Warn("API_KEY is empty — all requests will be accepted without authentication")
	}

	slog.Info("configuration loaded", slog.String("summary", live.Get().SafeSummary()))

	upProxy, err := proxy.NewUpstreamProxy(live)
	if err != nil {
		slog.Error("failed to initialize upstream proxy", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Egress transport: routes upstream traffic through the configured
	// proxy pool (Resin-style) when TELEAGENT_PROXY is set.
	var transport *http.Transport
	if live.Get().ProxyEnabled() {
		transport, err = proxy.BuildTransport(live.Get())
		if err != nil {
			slog.Error("failed to initialize upstream egress proxy", slog.String("error", err.Error()))
			os.Exit(1)
		}
		slog.Info("upstream egress proxy enabled", slog.String("proxy", live.Get().RedactedProxyURL()))
	}

	httpClient := &http.Client{
		Timeout: live.Get().Timeout,
	}
	if transport != nil {
		// Only set a custom transport when a proxy is configured; a typed-nil
		// *http.Transport would panic inside net/http.
		httpClient.Transport = transport
	}

	clientPool := proxy.NewClientPool(live)

	// Client API keys and the console admin credential are separate planes.
	keyStore := apikeys.NewStore(cfg.KeysFilePath)
	keyStore.EnsureLegacyKey(cfg.APIKey)
	adminPassword := cfg.AdminPassword

	if adminPassword == "" && cfg.APIKey == "" {
		// Neither console credential is configured. /v1/admin/* rejects every
		// request without one, so the documented bootstrap path ("add an account
		// in the panel") would be unreachable: the page loads but no login can
		// ever succeed. Generate a per-run console password and print it once,
		// instead of leaving the management API open to the network.
		adminPassword = middleware.GenerateAdminPassword()
		slog.Warn("ADMIN_PASSWORD/API_KEY 均未设置 — 已生成临时控制台密码（仅本次运行有效），请尽快设置 ADMIN_PASSWORD",
			slog.String("consolePassword", adminPassword), slog.String("login", "/ui"))
	}
	if adminPassword == "" {
		slog.Warn("ADMIN_PASSWORD 未设置 — 控制台暂用 API_KEY 登录，建议在 .env 中设置独立的 ADMIN_PASSWORD")
	}
	auth := &middleware.Authorizer{
		AdminPassword: adminPassword,
		LegacyAPIKey:  cfg.APIKey,
		Keys:          keyStore,
	}
	slog.Info("client keys loaded", slog.Int("keys", keyStore.Len()), slog.String("file", cfg.KeysFilePath))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.Health())

	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/v1/models", handler.Models(live.Get()))
	apiMux.HandleFunc("/v1/chat/completions", handler.ChatCompletions(upProxy, clientPool, acctStore, auth, live, live.Get().RetryCount))

	// Auto check-in (TeleAgent Star Welfare Club / 积分福利社)
	checkinSvc := checkin.NewService(live, acctStore, clientPool)
	if live.Get().CheckInEnabled {
		checkinSvc.Start()
		defer checkinSvc.Stop()
		slog.Info("auto check-in scheduled",
			slog.String("time", live.Get().CheckInTime),
			slog.String("tz", "Asia/Shanghai"),
			slog.String("type", live.Get().CheckInType))
	}
	apiMux.HandleFunc("/v1/checkin", checkinHandler(checkinSvc))

	// Web login (token renewal through the TeleAgent web account).
	authSvc := authlogin.NewService(live, httpClient, acctStore)
	mux.HandleFunc("/auth/login", authLoginHandler(authSvc, live))
	mux.HandleFunc("/auth/callback", authCallbackHandler(authSvc))

	// Control panel: page is public (login happens client-side against the
	// API_KEY auth); data endpoints live under the authenticated apiMux.
	panel := webui.NewHandler(live, checkinSvc, acctStore, keyStore, clientPool, httpClient, startedAt)
	mux.HandleFunc("/ui", panel.ServePage)
	mux.HandleFunc("/ui/", panel.ServePage)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
	// Console APIs live on a separate mux so they can be guarded by the admin
	// credential while the data plane accepts client API keys.
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/v1/admin/info", panel.ServeInfo)
	adminMux.HandleFunc("/v1/admin/proxy-test", panel.ServeProxyTest)
	adminMux.HandleFunc("/v1/admin/accounts", panel.ServeAccounts)
	adminMux.HandleFunc("/v1/admin/accounts/action", panel.ServeAccountAction)
	adminMux.HandleFunc("/v1/admin/logs", panel.ServeLogs)
	adminMux.HandleFunc("/v1/admin/keys", panel.ServeKeys)
	adminMux.HandleFunc("/v1/admin/models", panel.ServeModels)
	mux.Handle("/v1/admin/", auth.Admin(adminMux))

	// Data plane: client API keys + rate limit. The rate limiter keys on the
	// Authorization header even when invalid, so credential stuffing is
	// throttled too.
	var v1Handler http.Handler = auth.Client(apiMux)
	if limiter := middleware.NewRateLimiter(cfg.RateLimitRPM); limiter != nil {
		v1Handler = middleware.RateLimit(limiter)(v1Handler)
		if cfg.RateLimitRPM > 0 {
			slog.Info("rate limit enabled", slog.Int("rpm", cfg.RateLimitRPM))
		}
	}
	mux.Handle("/v1/", v1Handler)

	// Legacy endpoints
	mux.Handle("/models", auth.Client(http.HandlerFunc(handler.Models(cfg))))
	mux.Handle("/chat/completions", auth.Client(http.HandlerFunc(handler.ChatCompletions(upProxy, clientPool, acctStore, auth, live, cfg.RetryCount))))

	// Stack: RequestID → MaxBodySize → AccessLog → mux
	var h http.Handler = mux
	h = middleware.AccessLog(h)
	h = middleware.MaxBodySize(10 << 20)(h) // 10 MB
	h = middleware.RequestID(h)

	server := &http.Server{
		Addr:         cfg.Listen,
		Handler:      h,
		ReadTimeout:  cfg.Timeout / 4,
		WriteTimeout: cfg.Timeout * 2, // long enough for streaming
		IdleTimeout:  cfg.Timeout,
	}

	slog.Info("starting server",
		slog.String("listen", cfg.Listen),
		slog.String("upstream", cfg.BaseURL),
	)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down gracefully...")
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout/4)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", slog.String("error", err.Error()))
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server error", slog.String("error", err.Error()))
		os.Exit(1)
	}
	slog.Info("server stopped")
}

// checkinHandler exposes manual control of the TeleAgent daily check-in:
//
//	GET  /v1/checkin  → scheduler status + last run result
//	POST /v1/checkin  → trigger a check-in run right now
func checkinHandler(svc *checkin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, svc.Status())
		case http.MethodPost:
			ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
			defer cancel()
			res := svc.RunAll(ctx, r.URL.Query().Get("account"))
			status := http.StatusOK
			if !res.OK {
				status = http.StatusBadGateway
			}
			writeJSON(w, status, res)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// authLoginHandler redirects the browser to the TeleAgent UPMS login page
// with a redirect_uri pointing back at this gateway.
func authLoginHandler(svc *authlogin.Service, live *config.Live) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		redirectURI := publicBase(r, live.Get().PublicURL) + authlogin.CallbackPath
		loginURL, _ := svc.StartLogin(redirectURI)
		http.Redirect(w, r, loginURL, http.StatusFound)
	}
}

// authCallbackHandler receives the UPMS redirect, exchanges the code for a
// fresh JWT, persists it and shows the outcome.
func authCallbackHandler(svc *authlogin.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			writeLoginResult(w, false, "登录被拒绝："+errCode+"（"+q.Get("error_description")+"）")
			return
		}
		updated, err := svc.HandleCallback(r.Context(), q.Get("code"), q.Get("state"))
		if err != nil {
			writeLoginResult(w, false, "令牌兑换失败："+err.Error())
			return
		}
		exp := ""
		if t, ok := webui.JWTExpiry(updated.Token); ok {
			exp = t.Local().Format("2006-01-02 15:04")
		}
		writeLoginResult(w, true, "登录成功，新令牌已生效并持久化。到期时间："+exp+"。现在可以关闭本页面返回控制面板。")
	}
}

// publicBase derives the gateway's externally reachable base URL: the
// configured TELEAGENT_PUBLIC_URL when set, otherwise the request's host.
func publicBase(r *http.Request, configured string) string {
	if configured != "" {
		return strings.TrimRight(configured, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = fwd
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// writeLoginResult renders a minimal standalone result page.
func writeLoginResult(w http.ResponseWriter, ok bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	color := "#16a34a"
	icon := "✅"
	if !ok {
		color = "#dc2626"
		icon = "❌"
	}
	fmt.Fprintf(w, `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="UTF-8"><title>TeleAgent 登录结果</title></head>
<body style="font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;background:#f3f5f9;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0">
<div style="background:#fff;border:1px solid #e5e9f0;border-radius:14px;padding:36px 40px;max-width:520px;box-shadow:0 8px 30px rgba(15,23,42,.06)">
<h2 style="margin:0 0 12px">%s TeleAgent 网页登录</h2>
<p style="color:%s;font-size:14px;line-height:1.7;margin:0 0 18px">%s</p>
<a href="/ui" style="display:inline-block;padding:8px 18px;background:#2563eb;color:#fff;border-radius:8px;text-decoration:none;font-size:13px">返回控制面板</a>
</div></body></html>`, icon, color, message)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func initLogger(cfg config.Config) {
	opts := &slog.HandlerOptions{Level: cfg.SlogLevel()}

	var handler slog.Handler
	switch strings.ToLower(cfg.LogFormat) {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	default:
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}
