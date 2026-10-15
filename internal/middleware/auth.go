package middleware

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"teleagent2api/internal/apikeys"
)

// Authorizer separates the two credential planes:
//
//   - Client plane (/v1/models, /v1/chat/completions): any enabled API key
//     from the key store, plus the legacy single API_KEY for compatibility.
//   - Admin plane (/v1/admin/*, the console): the admin password only, so a
//     leaked client key cannot reach account management.
//
// When no admin password is configured the legacy API_KEY doubles as the
// console password (with a startup warning) to keep upgrades frictionless.
type Authorizer struct {
	AdminPassword string
	LegacyAPIKey  string
	Keys          *apikeys.Store
}

// GenerateAdminPassword returns a random console password.
func GenerateAdminPassword() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "admin-fallback"
	}
	return "admin-" + hex.EncodeToString(buf)
}

// Client guards the data plane.
func (a *Authorizer) Client(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			// No client keys configured at all → open gateway (dev mode).
			if a.Keys.Len() == 0 && a.LegacyAPIKey == "" {
				next.ServeHTTP(w, r)
				return
			}
			reject(w, r, "missing api key")
			return
		}
		// Key store first, then the legacy single key.
		if key, ok := a.Keys.Verify(token); ok {
			ctx := WithKeyInfo(r.Context(), key.ID, key.Name)
			a.Keys.MarkUsed(key.ID, true)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if a.LegacyAPIKey != "" && constEq(token, a.LegacyAPIKey) {
			next.ServeHTTP(w, r)
			return
		}
		reject(w, r, "invalid api key")
	})
}

// Admin guards the console APIs.
func (a *Authorizer) Admin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			reject(w, r, "missing admin credential")
			return
		}
		if a.AdminPassword != "" && constEq(token, a.AdminPassword) {
			next.ServeHTTP(w, r)
			return
		}
		// Fallback: legacy API_KEY acts as the console password until the
		// operator sets ADMIN_PASSWORD.
		if a.AdminPassword == "" && a.LegacyAPIKey != "" && constEq(token, a.LegacyAPIKey) {
			next.ServeHTTP(w, r)
			return
		}
		reject(w, r, "invalid admin credential")
	})
}

// CheckModelScope reports an error message when the presented client key is
// not allowed to call the given model.
func (a *Authorizer) CheckModelScope(r *http.Request, model string) string {
	token := bearer(r)
	if token == "" || model == "" {
		return ""
	}
	if key, ok := a.Keys.Verify(token); ok {
		if !key.AllowedModel(model) {
			return "api key is not allowed to use model " + model
		}
	}
	return ""
}

// MarkKeyResult records the outcome for the calling key (if any).
func (a *Authorizer) MarkKeyResult(r *http.Request, ok bool) {
	if id := KeyIDFromContext(r.Context()); id != "" {
		a.Keys.MarkUsed(id, ok)
	}
}

// ---- key identity in context ----

type keyInfoKey struct{}

type keyInfo struct {
	ID   string
	Name string
}

// WithKeyInfo attaches the calling key's identity to the context.
func WithKeyInfo(ctx context.Context, id, name string) context.Context {
	return context.WithValue(ctx, keyInfoKey{}, keyInfo{ID: id, Name: name})
}

// KeyIDFromContext returns the calling key's ID ("" for the legacy key).
func KeyIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyInfoKey{}).(keyInfo); ok {
		return v.ID
	}
	return ""
}

// KeyNameFromContext returns the calling key's display name.
func KeyNameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(keyInfoKey{}).(keyInfo); ok {
		return v.Name
	}
	return ""
}

func bearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func reject(w http.ResponseWriter, r *http.Request, reason string) {
	slog.WarnContext(r.Context(), "auth: rejected",
		slog.String("reason", reason),
		slog.String("remote", r.RemoteAddr),
		slog.String("path", r.URL.Path),
	)
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{"error": "invalid credentials"})
}

// Auth keeps the original single-key middleware for backward compatibility.
func Auth(expectedKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expectedKey == "" {
				slog.WarnContext(r.Context(), "auth: no API_KEY configured, allowing all requests",
					slog.String("remote", r.RemoteAddr),
				)
				next.ServeHTTP(w, r)
				return
			}
			if constEq(bearer(r), expectedKey) {
				next.ServeHTTP(w, r)
				return
			}
			reject(w, r, "invalid api key")
		})
	}
}
