package handler

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"teleagent2api/internal/accounts"
	"teleagent2api/internal/config"
	"teleagent2api/internal/middleware"
	"teleagent2api/internal/proxy"
)

// modelCreated is a stable timestamp for the /v1/models response.
var modelCreated = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

func Health() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}
}

func Models(cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		data := make([]map[string]any, 0, len(cfg.Models))
		for _, m := range cfg.Models {
			data = append(data, map[string]any{
				"id":       m,
				"object":   "model",
				"created":  modelCreated,
				"owned_by": "TeleAgent",
				"name":     m,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   data,
		})
	}
}

// ChatCompletions proxies OpenAI chat requests through the account pool:
// it picks an account, forwards the call, and fails over to the next account
// when the upstream rejects the credential.
func ChatCompletions(up *proxy.UpstreamProxy, pool *proxy.ClientPool, store *accounts.Store, auth *middleware.Authorizer, retryCount int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			slog.ErrorContext(r.Context(), "failed to read request body",
				slog.String("error", err.Error()),
			)
			http.Error(w, "read body error", http.StatusBadRequest)
			return
		}

		// Surface the requested model to the log/analytics layer and enforce
		// per-key model scoping.
		var bodyMeta struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &bodyMeta)
		if meta := middleware.RequestMetaFrom(r.Context()); meta != nil {
			meta.Model = bodyMeta.Model
		}
		if auth != nil {
			if msg := auth.CheckModelScope(r, bodyMeta.Model); msg != "" {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","message":%q}}`, msg)
				return
			}
		}

		// Attempts: one per available account, plus configured transient retries.
		maxAttempts := retryCount + 1
		if store.Len() > maxAttempts {
			maxAttempts = store.Len()
		}
		if maxAttempts > 6 {
			maxAttempts = 6 // cap failover fan-out
		}

		var resp *http.Response
		var acc accounts.Account
		var upstreamReq *http.Request
		lastErr := "no account available"

		for attempt := 0; attempt < maxAttempts; attempt++ {
			picked, err := store.Pick()
			if err != nil {
				slog.ErrorContext(r.Context(), "no usable account", slog.String("error", err.Error()))
				http.Error(w, "no available account: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			acc = picked
			if meta := middleware.RequestMetaFrom(r.Context()); meta != nil {
				meta.Account = picked.Name
			}

			upstreamReq, err = up.BuildRequest(r, body, acc)
			if err != nil {
				slog.ErrorContext(r.Context(), "failed to build upstream request",
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			if attempt > 0 {
				slog.WarnContext(r.Context(), "failing over to another account",
					slog.Int("attempt", attempt+1),
					slog.String("account", acc.Name),
					slog.String("reason", lastErr),
				)
			}

			resp, err = pool.For(acc).Do(upstreamReq)
			if err != nil {
				lastErr = err.Error()
				store.MarkFailure(acc.ID, err, false)
				if attempt < maxAttempts-1 {
					continue
				}
				slog.ErrorContext(r.Context(), "upstream request failed after retries",
					slog.String("error", err.Error()),
					slog.Int("attempts", attempt+1),
				)
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}

			// Auth failure → the credential is dead: retire it and try the next
			// account instead of surfacing an opaque error to the client.
			if isAuthFailure(resp) {
				lastErr = "upstream rejected credentials (401/login page)"
				store.MarkFailure(acc.ID, errors.New(lastErr), true)
				resp.Body.Close()
				if attempt < maxAttempts-1 {
					continue
				}
				http.Error(w, `{"error":{"type":"authentication_error","message":"all accounts rejected: refresh credentials via the panel (/ui → 网页登录续期)"}}`, http.StatusBadGateway)
				return
			}

			// Transient server-side errors are retriable.
			if resp.StatusCode >= 500 && attempt < maxAttempts-1 {
				lastErr = fmt.Sprintf("upstream %d", resp.StatusCode)
				store.MarkFailure(acc.ID, errors.New(lastErr), false)
				resp.Body.Close()
				resp = nil
				continue
			}
			break
		}

		if resp == nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		store.MarkSuccess(acc.ID)
		if auth != nil {
			auth.MarkKeyResult(r, resp.StatusCode < 400)
		}

		slog.InfoContext(r.Context(), "upstream responded",
			slog.Int("upstream_status", resp.StatusCode),
			slog.String("account", acc.Name),
		)

		// The TeleAgent gateway answers 200 + text/html (login page) when the
		// JWT is missing/invalid/expired. Forwarding that to OpenAI clients
		// yields opaque parse errors and leaks upstream pages, so translate it
		// into an explicit 502.
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
			slog.WarnContext(r.Context(), "upstream returned an HTML page instead of API data — token likely invalid or expired",
				slog.Int("upstream_status", resp.StatusCode))
			http.Error(w, `{"error":{"type":"authentication_error","message":"upstream returned a login page: TELEAGENT_TOKEN is invalid or expired, refresh it from the TeleAgent desktop client"}}`, http.StatusBadGateway)
			return
		}

		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			flushCopy(w, r, resp.Body)
			return
		}
		io.Copy(w, resp.Body)
	}
}

// isAuthFailure reports whether the upstream response indicates the account
// credential is no longer valid (401, the "当前令牌是否存在" JSON error, or an
// HTML login page served with 200).
func isAuthFailure(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return true
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/html") {
		return true
	}
	return false
}

// proxyResponseHeaderBlacklist lists upstream headers that should never be
// forwarded to the client.
var proxyResponseHeaderBlacklist = map[string]struct{}{"connection": {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"set-cookie":          {},
	"x-frame-options":     {},
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if _, skip := proxyResponseHeaderBlacklist[strings.ToLower(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	dst.Set("Connection", "close")
}

func flushCopy(w http.ResponseWriter, r *http.Request, body io.Reader) {
	flusher, ok := w.(http.Flusher)
	reader := bufio.NewReader(body)
	ctx := r.Context()

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := w.Write(line); writeErr != nil {
				slog.WarnContext(ctx, "flushCopy: client disconnected",
					slog.String("error", writeErr.Error()),
					slog.String("request_id", middleware.GetRequestID(ctx)),
				)
				return
			}
			if ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.WarnContext(ctx, "flushCopy: upstream read error",
					slog.String("error", err.Error()),
				)
			}
			return
		}

		// Respect client disconnect.
		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "flushCopy: context cancelled, stopping copy")
			return
		default:
		}
	}
}
