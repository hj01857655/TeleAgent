package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type contextKey string

const requestIDKey contextKey = "request_id"

// RequestID injects a unique request ID into context and response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetRequestID extracts the request ID from context.
func GetRequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// LogEntry is one captured request, surfaced in the console's log view.
type LogEntry struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"durationMs"`
	Remote     string `json:"remote"`
	RequestID  string `json:"requestId"`
	Model      string `json:"model,omitempty"`
	Account    string `json:"account,omitempty"`
	KeyName    string `json:"keyName,omitempty"`
}

// RequestMeta carries per-request dimensions (model / serving account) from
// the handler back to the access-log middleware.
type RequestMeta struct {
	Model   string
	Account string
}

type reqMetaKey struct{}

// WithRequestMeta attaches a mutable metadata holder to the context.
func WithRequestMeta(ctx context.Context) (*RequestMeta, context.Context) {
	m := &RequestMeta{}
	return m, context.WithValue(ctx, reqMetaKey{}, m)
}

// RequestMetaFrom returns the metadata holder, or nil when absent.
func RequestMetaFrom(ctx context.Context) *RequestMeta {
	if m, ok := ctx.Value(reqMetaKey{}).(*RequestMeta); ok {
		return m
	}
	return nil
}

var (
	recentMu   sync.Mutex
	recentLogs = make([]LogEntry, 0, recentLogCap)
)

const recentLogCap = 200

// RecentLogs returns the most recent requests (newest first).
func RecentLogs(limit int) []LogEntry {
	recentMu.Lock()
	defer recentMu.Unlock()
	if limit <= 0 || limit > len(recentLogs) {
		limit = len(recentLogs)
	}
	out := make([]LogEntry, 0, limit)
	for i := len(recentLogs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, recentLogs[i])
	}
	return out
}

func appendRecent(e LogEntry) {
	recentMu.Lock()
	defer recentMu.Unlock()
	if len(recentLogs) >= recentLogCap {
		copy(recentLogs, recentLogs[1:])
		recentLogs = recentLogs[:recentLogCap-1]
	}
	recentLogs = append(recentLogs, e)
}

// AccessLog logs every completed request with structured fields.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		meta, ctx := WithRequestMeta(r.Context())
		next.ServeHTTP(sw, r.WithContext(ctx))

		elapsed := time.Since(start)
		// The console's own polling would drown the log view; skip it.
		if !strings.HasPrefix(r.URL.Path, "/v1/admin") {
			appendRecent(LogEntry{
				Time:       start.Format("15:04:05"),
				Method:     r.Method,
				Path:       r.URL.Path,
				Status:     sw.status,
				Bytes:      int64(sw.bytes),
				DurationMS: elapsed.Milliseconds(),
				Remote:     r.RemoteAddr,
				RequestID:  GetRequestID(r.Context()),
				Model:      meta.Model,
				Account:    meta.Account,
				KeyName:    KeyNameFromContext(r.Context()),
			})
		}

		slog.InfoContext(r.Context(), "request completed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", sw.status),
			slog.Int("bytes", sw.bytes),
			slog.Duration("duration", elapsed),
			slog.String("remote", r.RemoteAddr),
			slog.String("request_id", GetRequestID(r.Context())),
		)
	})
}

// MaxBodySize rejects requests with body exceeding limit bytes.
func MaxBodySize(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// --- helpers ---

func newRequestID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("060102150405")))
	}
	return hex.EncodeToString(buf)
}

// statusWriter captures response status and bytes written.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
