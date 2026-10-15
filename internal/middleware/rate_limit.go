package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimiter is a minimal in-memory token-bucket rate limiter. It protects
// the TeleAgent account quota behind the gateway: if the gateway API_KEY
// leaks, an attacker can otherwise burn the account's credits via chat or
// check-in calls. Buckets are keyed by the caller identity (API key or
// remote IP) and refill continuously.
//
// Not distributed — a single gateway instance covers a single-user
// deployment, which is the intended use.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rpm     float64 // requests per minute allowed per identity
	burst   float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter builds a limiter allowing rpm requests per minute per
// identity, with a burst capacity of max(1, rpm/60) — i.e. about one second
// of traffic. An rpm <= 0 disables limiting (Allow always returns true).
func NewRateLimiter(rpm int) *RateLimiter {
	if rpm <= 0 {
		return nil
	}
	return &RateLimiter{
		buckets: make(map[string]*bucket),
		rpm:     float64(rpm),
		burst:   float64(rpm)/60.0 + 1,
	}
}

// Allow reports whether the identity may proceed, consuming a token.
func (rl *RateLimiter) Allow(identity string) bool {
	if rl == nil {
		return true
	}
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[identity]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[identity] = b
	}
	// Refill since last seen.
	elapsed := now.Sub(b.last).Minutes()
	b.tokens += elapsed * rl.rpm
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RateLimit returns a middleware enforcing the limiter per API key (or per
// remote address when unauthenticated). Rejected requests get 429 with a
// Retry-After hint.
//
// Admin/console endpoints (/v1/admin/*) are exempt: they are authenticated,
// do not consume upstream quota, and are polled by the dashboard — throttling
// them would break monitoring without protecting anything.
func RateLimit(rl *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rl == nil || strings.HasPrefix(r.URL.Path, "/v1/admin/") {
				next.ServeHTTP(w, r)
				return
			}
			identity := r.Header.Get("Authorization")
			if identity == "" {
				identity = r.RemoteAddr
			}
			if !rl.Allow(identity) {
				w.Header().Set("Retry-After", strconv.Itoa(int(60/rl.rpm)+1))
				http.Error(w, `{"error":{"type":"rate_limit_exceeded","message":"too many requests; the upstream TeleAgent account quota is protected by a rate limit"}}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
