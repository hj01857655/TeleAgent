package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"teleagent2api/internal/accounts"
	"teleagent2api/internal/config"
)

// ClientPool hands out *http.Client instances keyed by egress proxy
// configuration, so each account can be routed through its own Resin-style
// proxy while sharing the process-wide defaults.
type ClientPool struct {
	live  *config.Live
	mu    sync.Mutex
	cache map[string]*http.Client
}

// NewClientPool creates an empty pool bound to the live configuration.
func NewClientPool(live *config.Live) *ClientPool {
	return &ClientPool{live: live, cache: make(map[string]*http.Client)}
}

// For returns the client to use for the given account: its own proxy when
// configured, otherwise the global proxy settings.
func (p *ClientPool) For(acc accounts.Account) *http.Client {
	cfg := p.live.Get()
	if acc.ProxyURL != "" {
		cfg.ProxyURL = acc.ProxyURL
		if acc.ProxyPlatform != "" {
			cfg.ProxyPlatform = acc.ProxyPlatform
		}
		if acc.ProxyAccount != "" {
			cfg.ProxyAccount = acc.ProxyAccount
		}
		if acc.ProxyToken != "" {
			cfg.ProxyToken = acc.ProxyToken
		}
	}

	key := cfg.ProxyURL + "|" + cfg.ProxyPlatform + "|" + cfg.ProxyAccount + "|" + cfg.ProxyToken
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[key]; ok {
		return c
	}

	client := &http.Client{Timeout: cfg.Timeout}
	if cfg.ProxyEnabled() {
		transport, err := BuildTransport(cfg)
		if err != nil {
			slog.Error("proxy: build transport failed, falling back to direct",
				slog.String("proxy", cfg.RedactedProxyURL()),
				slog.String("error", err.Error()))
		} else if transport != nil {
			client.Transport = transport
			slog.Info("proxy: account client created", slog.String("proxy", cfg.RedactedProxyURL()))
		}
	}
	p.cache[key] = client
	return client
}

// Invalidate drops cached clients (call after proxy settings change).
func (p *ClientPool) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache = make(map[string]*http.Client)
}

// TestEgress probes the public IP through the given account's egress path.
func (p *ClientPool) TestEgress(acc accounts.Account, url string, timeout time.Duration) (string, error) {
	client := p.For(acc)
	c := *client
	c.Timeout = timeout
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 128)
	n, _ := resp.Body.Read(buf)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(buf[:n]), nil
}
