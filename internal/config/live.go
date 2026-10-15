package config

import (
	"sync"
)

// Live holds the working configuration behind an RWMutex so that runtime
// updates (notably a fresh JWT obtained through web login) take effect
// immediately without restarting the process. Readers get a snapshot
// copy; writers replace fields atomically.
type Live struct {
	mu  sync.RWMutex
	cfg Config
}

// NewLive wraps an initial configuration.
func NewLive(cfg Config) *Live {
	return &Live{cfg: cfg}
}

// Get returns a snapshot of the current configuration.
func (l *Live) Get() Config {
	if l == nil {
		return Config{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cfg
}

// Replace swaps the whole configuration.
func (l *Live) Replace(cfg Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = cfg
}

// UpdateCreds swaps the TeleAgent credentials (JWT + device identifiers),
// preserving every other field.
func (l *Live) UpdateCreds(token, deviceID, installID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg.Token = token
	l.cfg.DeviceID = deviceID
	l.cfg.InstallID = installID
}

// StateFilePath reports where runtime credential updates are persisted
// (empty when no state file was configured).
func (l *Live) StateFilePath() string {
	return l.Get().StateFilePath
}
