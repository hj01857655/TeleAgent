// Package apikeys manages the gateway's client API keys. Each key is an
// independent credential for calling the data plane (/v1/*), separate from
// the console's admin password.
package apikeys

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Key is one client credential.
type Key struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Secret       string   `json:"secret"` // sk-...
	Enabled      bool     `json:"enabled"`
	Models       []string `json:"models,omitempty"`       // empty = all models
	RateLimitRPM int      `json:"rateLimitRpm,omitempty"` // 0 = use global
	CreatedAt    string   `json:"createdAt"`
	LastUsedAt   string   `json:"lastUsedAt,omitempty"`
	RequestCount int64    `json:"requestCount,omitempty"`
	ErrorCount   int64    `json:"errorCount,omitempty"`
}

// Snapshot is the API-facing view with the secret masked.
type Snapshot struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Hint         string   `json:"hint"`
	Enabled      bool     `json:"enabled"`
	Models       []string `json:"models"`
	AllModels    bool     `json:"allModels"`
	RateLimitRPM int      `json:"rateLimitRpm"`
	CreatedAt    string   `json:"createdAt"`
	LastUsedAt   string   `json:"lastUsedAt,omitempty"`
	RequestCount int64    `json:"requestCount"`
	ErrorCount   int64    `json:"errorCount"`
}

// Store persists the key set.
type Store struct {
	mu   sync.RWMutex
	path string
	keys []*Key
}

// NewStore loads (or creates) the key set at path.
func NewStore(path string) *Store {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		slog.Warn("apikeys: load failed, starting empty", slog.String("error", err.Error()))
	}
	return s
}

func (s *Store) load() error {
	if s.path == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc struct {
		Keys []*Key `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	s.mu.Lock()
	s.keys = doc.Keys
	s.mu.Unlock()
	return nil
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(map[string]any{"keys": s.keys}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Len reports the number of stored keys.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Verify checks a presented bearer token against all enabled keys.
// Returns the matching key (for usage accounting) when valid.
func (s *Store) Verify(token string) (*Key, bool) {
	if token == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if !k.Enabled || k.Secret == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(k.Secret)) == 1 {
			cp := *k
			return &cp, true
		}
	}
	return nil, false
}

// Get returns a copy of one key by ID.
func (s *Store) Get(id string) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if k.ID == id {
			return *k, true
		}
	}
	return Key{}, false
}

// Create adds a new key. When name is empty a default is generated.
// The returned Key carries the full secret (shown to the user once).
func (s *Store) Create(name string, models []string, rpm int) (Key, error) {
	secret, err := generateSecret()
	if err != nil {
		return Key{}, err
	}
	k := Key{
		ID:           newID(),
		Name:         strings.TrimSpace(name),
		Secret:       secret,
		Enabled:      true,
		Models:       models,
		RateLimitRPM: rpm,
		CreatedAt:    time.Now().Format("2006-01-02 15:04:05"),
	}
	if k.Name == "" {
		k.Name = "密钥 " + k.ID[:4]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, &k)
	if err := s.saveLocked(); err != nil {
		return Key{}, err
	}
	return k, nil
}

// Update applies a partial update (name/enabled/models/rateLimitRPM).
func (s *Store) Update(id string, name *string, enabled *bool, models []string, modelsSet bool, rpm *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.ID != id {
			continue
		}
		if name != nil && strings.TrimSpace(*name) != "" {
			k.Name = strings.TrimSpace(*name)
		}
		if enabled != nil {
			k.Enabled = *enabled
		}
		if modelsSet {
			k.Models = models
		}
		if rpm != nil {
			k.RateLimitRPM = *rpm
		}
		return s.saveLocked()
	}
	return fmt.Errorf("key %s not found", id)
}

// Delete removes a key.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.keys {
		if k.ID == id {
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			return s.saveLocked()
		}
	}
	return fmt.Errorf("key %s not found", id)
}

// MarkUsed records usage for a key (best-effort, flushed periodically).
func (s *Store) MarkUsed(id string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.ID != id {
			continue
		}
		k.RequestCount++
		if !ok {
			k.ErrorCount++
		}
		k.LastUsedAt = time.Now().Format("2006-01-02 15:04:05")
		return
	}
}

// SaveState persists accumulated usage counters.
func (s *Store) SaveState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveLocked(); err != nil {
		slog.Warn("apikeys: save failed", slog.String("error", err.Error()))
	}
}

// Snapshots returns the masked, UI-facing list.
func (s *Store) Snapshots() []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Snapshot, 0, len(s.keys))
	for _, k := range s.keys {
		models := k.Models
		if models == nil {
			models = []string{}
		}
		out = append(out, Snapshot{
			ID:           k.ID,
			Name:         k.Name,
			Hint:         mask(k.Secret),
			Enabled:      k.Enabled,
			Models:       models,
			AllModels:    len(k.Models) == 0,
			RateLimitRPM: k.RateLimitRPM,
			CreatedAt:    k.CreatedAt,
			LastUsedAt:   k.LastUsedAt,
			RequestCount: k.RequestCount,
			ErrorCount:   k.ErrorCount,
		})
	}
	return out
}

// AllowedModel reports whether the key may call the given model.
func (k *Key) AllowedModel(model string) bool {
	if len(k.Models) == 0 || model == "" {
		return true
	}
	for _, m := range k.Models {
		if m == model {
			return true
		}
	}
	return false
}

// EnsureLegacyKey imports a pre-existing single API_KEY so upgraded
// deployments keep working, and returns it.
func (s *Store) EnsureLegacyKey(apiKey string) {
	if strings.TrimSpace(apiKey) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(k.Secret), []byte(apiKey)) == 1 {
			return // already present
		}
	}
	s.keys = append(s.keys, &Key{
		ID:        newID(),
		Name:      "默认密钥（来自 .env）",
		Secret:    apiKey,
		Enabled:   true,
		CreatedAt: time.Now().Format("2006-01-02 15:04:05"),
	})
	if err := s.saveLocked(); err != nil {
		slog.Warn("apikeys: import legacy key failed", slog.String("error", err.Error()))
		return
	}
	slog.Info("apikeys: imported legacy API_KEY from environment")
}

func generateSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.New("generate secret: " + err.Error())
	}
	return "sk-ta2api-" + hex.EncodeToString(buf), nil
}

func newID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1e12)
	}
	return hex.EncodeToString(buf)
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 14 {
		return s[:3] + "……"
	}
	return s[:10] + "……" + s[len(s)-4:]
}
