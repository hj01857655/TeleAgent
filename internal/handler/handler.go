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
	"teleagent2api/internal/adapter"
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
// it sanitises the body, picks an account, forwards the call, and fails over
// to the next account when the upstream rejects the credential or answers with
// nothing at all.
func ChatCompletions(up *proxy.UpstreamProxy, pool *proxy.ClientPool, store *accounts.Store, auth *middleware.Authorizer, live *config.Live, retryCount int) http.HandlerFunc {
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

		cfg := live.Get()

		// The upstream is the desktop app's own backend and rejects any field it
		// does not know, so the body is reduced to the fields it accepts before
		// anything is forwarded. This is what removes the "API 调用参数有误"
		// class of failure that clients like Claude Code provoke.
		original := body
		body = adapter.SanitizeRequest(body, cfg.ExtraFields, adapter.ModelLimits(cfg.ModelLimits))
		if len(body) != len(original) {
			slog.DebugContext(r.Context(), "request sanitized",
				slog.Int("bytes_in", len(original)), slog.Int("bytes_out", len(body)))
		}

		// Surface the requested model to the log/analytics layer and enforce
		// per-key model scoping.
		var bodyMeta struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &bodyMeta)
		if meta := middleware.RequestMetaFrom(r.Context()); meta != nil {
			meta.Model = bodyMeta.Model
		}
		if auth != nil {
			if msg := auth.CheckModelScope(r, bodyMeta.Model); msg != "" {
				writeError(w, http.StatusForbidden, "invalid_request_error", msg)
				return
			}
		}

		// Attempts: one per available account, plus configured transient retries,
		// plus one for the empty-response retry, which is a distinct failure the
		// retry count was never meant to cover.
		maxAttempts := retryCount + 2
		if store.Len() > maxAttempts {
			maxAttempts = store.Len()
		}
		if maxAttempts > 6 {
			maxAttempts = 6 // cap failover fan-out
		}

		var resp *http.Response
		var acc accounts.Account
		lastErr := "no account available"

		// Empty pool is a configuration state, not a transient failure: fail
		// closed with an actionable message instead of the generic
		// "no available account", which suggests a cooldown and hides the cause.
		if store.Len() == 0 {
			slog.ErrorContext(r.Context(), "no account configured; data plane disabled")
			writeError(w, http.StatusServiceUnavailable, "service_unavailable",
				"no account configured: add one in the control panel (/ui → 账号), or set TELEAGENT_TOKEN/TELEAGENT_INSTALL_ID/TELEAGENT_DEVICE_ID and restart")
			return
		}

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

			upstreamReq, err := up.BuildRequest(r, body, acc)
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
				writeError(w, http.StatusBadGateway, "authentication_error",
					"all accounts rejected: refresh credentials via the panel (/ui → 网页登录续期)")
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

			// Non-streaming replies are buffered so an empty one can be retried.
			// Streaming replies cannot: the headers are already on the wire.
			isStream := bodyMeta.Stream || isEventStream(resp)
			if !isStream && resp.StatusCode < 400 {
				// Take the headers before releasing the response: the buffered
				// branch below no longer has a resp to read them from.
				upstreamHeader := resp.Header.Clone()
				status := resp.StatusCode
				raw, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				resp = nil
				if readErr != nil {
					slog.ErrorContext(r.Context(), "failed to read upstream response",
						slog.String("error", readErr.Error()))
					http.Error(w, "bad gateway", http.StatusBadGateway)
					return
				}
				if adapter.IsEmptyResponse(raw) && attempt < maxAttempts-1 {
					// The upstream accepted the request and then said nothing. Seen
					// often enough that treating it as a landed answer would hand
					// clients an empty message they cannot diagnose.
					//
					// Deliberately not MarkFailure: that starts a 60s cooldown, which
					// with a single account makes the retry below impossible — Pick
					// would report "no available account" and the client would get a
					// 503 instead of the answer. Rotation between accounts comes from
					// Pick's cursor, not from the cooldown.
					lastErr = "upstream returned an empty response"
					slog.WarnContext(r.Context(), "upstream returned empty response, retrying",
						slog.Int("attempt", attempt+1),
						slog.Int("body_len", len(raw)),
					)
					continue
				}
				store.MarkSuccess(acc.ID)
				if auth != nil {
					auth.MarkKeyResult(r, true)
				}
				copyHeaders(w.Header(), upstreamHeader)
				writeNonStreaming(w, status, raw, cfg.ReasoningMode)
				return
			}

			if resp.StatusCode >= 400 {
				// Client errors are the client's to fix; pass them through rather
				// than burning the remaining accounts on them.
				break
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
		if isHTML(resp) {
			slog.WarnContext(r.Context(), "upstream returned an HTML page instead of API data — token likely invalid or expired",
				slog.Int("upstream_status", resp.StatusCode))
			writeError(w, http.StatusBadGateway, "authentication_error",
				"upstream returned a login page: TELEAGENT_TOKEN is invalid or expired, refresh it from the TeleAgent desktop client")
			return
		}

		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)

		if isEventStream(resp) {
			streamCopy(w, r, resp.Body, adapter.NewStreamProcessor(cfg.ReasoningMode))
			return
		}
		io.Copy(w, resp.Body)
	}
}

// writeNonStreaming emits a buffered body, rewriting the reasoning block if the
// mode asks for it. The status is echoed rather than assumed because the
// upstream may have answered 206 for some gateway-side reason.
func writeNonStreaming(w http.ResponseWriter, status int, raw []byte, mode string) {
	out := adapter.TransformNonStreaming(raw, mode)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// writeError emits an OpenAI-shaped error so clients can parse it.
func writeError(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"type":%q,"message":%q}}`, kind, message)
}

// isEventStream reports whether the upstream is streaming.
func isEventStream(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// isHTML reports whether the upstream answered with a page instead of JSON.
func isHTML(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html")
}

// isAuthFailure reports whether the upstream response indicates the account
// credential is no longer valid (401, the "当前令牌是否存在" JSON error, or an
// HTML login page served with 200).
func isAuthFailure(resp *http.Response) bool {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return true
	}
	return isHTML(resp)
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
	// The body is rewritten, so the upstream's length is no longer true.
	"content-length": {},
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

// sseWriter tracks the one piece of state line-by-line forwarding needs: a
// rewritten data line emits its own terminating blank line, so the blank line
// that followed it in the upstream stream has to be swallowed or the client
// sees an extra event boundary.
type sseWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	consumed bool // the previous line emitted its own blank terminator
}

func (s *sseWriter) write(data []byte) bool {
	if _, err := s.w.Write(data); err != nil {
		return false
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}

// streamCopy forwards an SSE stream, rewriting each event through proc.
//
// Lines are read and written one at a time so a client sees tokens as they
// arrive; the processor may turn one upstream event into several, which is why
// the write step loops.
func streamCopy(w http.ResponseWriter, r *http.Request, body io.Reader, proc *adapter.StreamProcessor) {
	flusher, _ := w.(http.Flusher)
	out := &sseWriter{w: w, flusher: flusher}
	reader := bufio.NewReader(body)
	ctx := r.Context()

	write := func(data []byte) bool {
		if !out.write(data) {
			slog.WarnContext(ctx, "streamCopy: client disconnected",
				slog.String("request_id", middleware.GetRequestID(ctx)),
			)
			return false
		}
		return true
	}

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !writeLine(line, proc, out) {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				slog.WarnContext(ctx, "streamCopy: upstream read error",
					slog.String("error", err.Error()),
				)
			}
			// Anything still buffered is text the upstream did not finish
			// delivering as a complete chunk; emit it rather than drop it.
			if tail := proc.Flush(); tail != nil {
				if !write([]byte("data: ")) || !write(tail) || !write([]byte("\n\n")) {
					return
				}
			}
			// A stream that opened, closed, and said nothing is the streaming
			// shape of the empty response the non-streaming path retries. It
			// cannot be retried here — the headers are long gone — but it is
			// worth a warning, because the client will just see nothing.
			if !proc.Passthrough() && !proc.HasContent() {
				slog.WarnContext(ctx, "stream ended without any content or reasoning",
					slog.String("request_id", middleware.GetRequestID(ctx)),
				)
			}
			return
		}

		select {
		case <-ctx.Done():
			slog.DebugContext(ctx, "streamCopy: context cancelled, stopping copy")
			return
		default:
		}
	}
}

// writeLine handles one SSE line, delegating data payloads to the processor.
func writeLine(line []byte, proc *adapter.StreamProcessor, out *sseWriter) bool {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		if out.consumed {
			// The rewritten line already wrote its terminator.
			out.consumed = false
			return true
		}
		return out.write(line)
	}
	out.consumed = false
	if !strings.HasPrefix(trimmed, "data:") {
		// Comments (": keep-alive") carry no payload but do keep the connection
		// visibly alive, so they pass through untouched.
		return out.write(line)
	}
	payload := []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
	events := proc.ProcessChunk(payload)
	if len(events) == 0 {
		// Entirely buffered — the client must see nothing for this line, not an
		// empty event, or it would receive a malformed data line. The blank line
		// that follows has to go with it, or the client sees a bare event.
		out.consumed = true
		return true
	}
	for _, event := range events {
		if len(event) == 0 {
			continue
		}
		if !out.write([]byte("data: ")) || !out.write(event) || !out.write([]byte("\n\n")) {
			return false
		}
	}
	out.consumed = true
	return true
}
