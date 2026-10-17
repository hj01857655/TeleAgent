// Package accounts implements the TeleAgent account pool: multiple
// credentials with round-robin scheduling, automatic failover on auth
// errors, per-account health tracking and per-account check-in settings.
package accounts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status values surfaced in the console.
const (
	StatusReady      = "ready"       // 正常
	StatusCooldown   = "cooldown"    // 冷却中（刚失败）
	StatusAuthFailed = "auth_failed" // 凭证失效
	StatusDisabled   = "disabled"    // 已禁用
)

// cooldownAfterFailure is how long an account is skipped after an error.
const cooldownAfterFailure = 60 * time.Second

// Account is one TeleAgent credential set.
type Account struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Token     string `json:"token"`
	DeviceID  string `json:"deviceId"`
	InstallID string `json:"installId"`
	Enabled   bool   `json:"enabled"`
	Priority  int    `json:"priority"`

	// Per-account egress proxy (Resin style); empty = use global config.
	ProxyURL      string `json:"proxyUrl,omitempty"`
	ProxyPlatform string `json:"proxyPlatform,omitempty"`
	ProxyAccount  string `json:"proxyAccount,omitempty"`
	ProxyToken    string `json:"proxyToken,omitempty"`

	// Check-in
	AutoCheckin bool   `json:"autoCheckin"`
	CheckinTime string `json:"checkinTime,omitempty"` // HH:MM, empty = global

	// Runtime state (persisted so the console survives restarts)
	Status         string `json:"status,omitempty"`
	LastError      string `json:"lastError,omitempty"`
	LastUsedAt     string `json:"lastUsedAt,omitempty"`
	LastCheckinAt  string `json:"lastCheckinAt,omitempty"`
	LastCheckinMsg string `json:"lastCheckinMsg,omitempty"`
	LastCheckinOK  bool   `json:"lastCheckinOk,omitempty"`
	RequestCount   int64  `json:"requestCount,omitempty"`
	ErrorCount     int64  `json:"errorCount,omitempty"`

	// Runtime-only (not persisted)
	cooldownUntil time.Time `json:"-"`
}

// Snapshot is the API-facing view with secrets masked.
type Snapshot struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Enabled        bool   `json:"enabled"`
	Priority       int    `json:"priority"`
	Status         string `json:"status"`
	StatusText     string `json:"statusText"`
	TokenHint      string `json:"tokenHint"`
	TokenSet       bool   `json:"tokenSet"`
	DeviceID       string `json:"deviceId"`
	InstallID      string `json:"installId"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
	DaysLeft       int    `json:"daysLeft"`
	Expired        bool   `json:"expired"`
	ProxyURL       string `json:"proxyUrl,omitempty"`
	AutoCheckin    bool   `json:"autoCheckin"`
	CheckinTime    string `json:"checkinTime,omitempty"`
	LastError      string `json:"lastError,omitempty"`
	LastUsedAt     string `json:"lastUsedAt,omitempty"`
	LastCheckinAt  string `json:"lastCheckinAt,omitempty"`
	LastCheckinMsg string `json:"lastCheckinMsg,omitempty"`
	LastCheckinOK  bool   `json:"lastCheckinOk"`
	RequestCount   int64  `json:"requestCount"`
	ErrorCount     int64  `json:"errorCount"`
}

// Store persists the account pool to a JSON file.
type Store struct {
	mu       sync.RWMutex
	path     string
	accounts []*Account
	cursor   int
}

// NewStore loads (or creates) the account pool at path.
func NewStore(path string) *Store {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		slog.Warn("accounts: load failed, starting empty", slog.String("error", err.Error()))
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
		Accounts []*Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	s.mu.Lock()
	s.accounts = doc.Accounts
	for _, a := range s.accounts {
		if a.Status == "" {
			a.Status = StatusReady
		}
	}
	s.mu.Unlock()
	return nil
}

// Save writes the pool atomically.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(map[string]any{"accounts": s.accounts}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Len reports how many accounts exist.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts)
}

// List returns internal copies (callers must not mutate the originals).
func (s *Store) List() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, *a)
	}
	return out
}

// Get returns one account by ID.
func (s *Store) Get(id string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return *a, true
		}
	}
	return Account{}, false
}

// Upsert creates (empty ID) or updates an account, persisting the result.
func (s *Store) Upsert(a Account) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if a.ID == "" {
		a.ID = newID()
		if a.Name == "" {
			a.Name = "账号 " + a.ID[:4]
		}
		if a.Status == "" {
			a.Status = StatusReady
		}
		s.accounts = append(s.accounts, &a)
	} else {
		found := false
		for i, ex := range s.accounts {
			if ex.ID == a.ID {
				// Preserve runtime counters/state unless explicitly provided.
				if a.Status == "" {
					a.Status = ex.Status
				}
				a.RequestCount = ex.RequestCount
				a.ErrorCount = ex.ErrorCount
				if a.LastUsedAt == "" {
					a.LastUsedAt = ex.LastUsedAt
				}
				if a.LastCheckinAt == "" {
					a.LastCheckinAt = ex.LastCheckinAt
					a.LastCheckinMsg = ex.LastCheckinMsg
					a.LastCheckinOK = ex.LastCheckinOK
				}
				s.accounts[i] = &a
				found = true
				break
			}
		}
		if !found {
			return Account{}, fmt.Errorf("account %s not found", a.ID)
		}
	}
	if err := s.saveLocked(); err != nil {
		return Account{}, err
	}
	return a, nil
}

// Delete removes an account.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if a.ID == id {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			return s.saveLocked()
		}
	}
	return fmt.Errorf("account %s not found", id)
}

// SeedFromEnv inserts a default account when the pool is empty, so existing
// single-account deployments keep working unchanged.
func (s *Store) SeedFromEnv(token, deviceID, installID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.accounts) > 0 || token == "" {
		return
	}
	s.accounts = append(s.accounts, &Account{
		ID:        newID(),
		Name:      "默认账号",
		Token:     token,
		DeviceID:  deviceID,
		InstallID: installID,
		Enabled:   true,
		Status:    StatusReady,
	})
	if err := s.saveLocked(); err != nil {
		slog.Warn("accounts: seed persist failed", slog.String("error", err.Error()))
	}
	slog.Info("accounts: seeded default account from environment")
}

// UpsertFromLogin records credentials acquired through the web-login flow so
// that the account pool reflects them immediately. Without this the pool only
// ever learns about them from SeedFromEnv at process start, which means a
// successful web login leaves the console reporting "no usable account" until
// the process is restarted.
//
// Matching order: same installId, then same deviceId, then the first account in
// the pool (renewing the primary account); an empty pool creates a new entry.
// The second return value reports whether the change was persisted.
func (s *Store) UpsertFromLogin(token, deviceID, installID string) (Account, bool) {
	if strings.TrimSpace(token) == "" {
		return Account{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	find := func(match func(*Account) bool) *Account {
		for _, a := range s.accounts {
			if match(a) {
				return a
			}
		}
		return nil
	}

	var target *Account
	if installID != "" {
		target = find(func(a *Account) bool { return a.InstallID == installID })
	}
	if target == nil && deviceID != "" {
		target = find(func(a *Account) bool { return a.DeviceID == deviceID })
	}
	if target == nil && len(s.accounts) > 0 {
		target = s.accounts[0]
	}
	if target == nil {
		target = &Account{ID: newID(), Name: "网页登录"}
		s.accounts = append(s.accounts, target)
	}

	target.Token = token
	if deviceID != "" {
		target.DeviceID = deviceID
	}
	if installID != "" {
		target.InstallID = installID
	}
	target.Enabled = true
	target.Status = StatusReady
	target.LastError = ""
	target.cooldownUntil = time.Time{}

	if err := s.saveLocked(); err != nil {
		slog.Warn("accounts: persist web-login credentials failed", slog.String("error", err.Error()))
		return *target, false
	}
	return *target, true
}

// Pick selects the next usable account (round-robin over healthy, enabled
// accounts, honouring Priority as a tie-breaker via stable ordering).
func (s *Store) Pick() (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	type cand struct {
		a   *Account
		idx int
	}
	var cands []cand
	for i, a := range s.accounts {
		if !a.Enabled || a.Token == "" {
			continue
		}
		if a.Status == StatusAuthFailed {
			continue
		}
		if now.Before(a.cooldownUntil) {
			continue
		}
		cands = append(cands, cand{a: a, idx: i})
	}
	if len(cands) == 0 {
		return Account{}, errors.New("no available account (all disabled, cooling down or auth-failed)")
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return cands[i].a.Priority > cands[j].a.Priority
	})
	pick := cands[s.cursor%len(cands)]
	s.cursor = (s.cursor + 1) % 1_000_000
	return *pick.a, nil
}

// MarkSuccess records a successful upstream call for the account.
func (s *Store) MarkSuccess(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		a.Status = StatusReady
		a.LastError = ""
		a.LastUsedAt = time.Now().Format(time.RFC3339)
		a.RequestCount++
		a.cooldownUntil = time.Time{}
		return
	}
}

// MarkFailure records a failed call. authFailure marks the credential dead;
// other errors only trigger a short cooldown so traffic fails over.
func (s *Store) MarkFailure(id string, err error, authFailure bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		a.ErrorCount++
		a.LastError = err.Error()
		if authFailure {
			a.Status = StatusAuthFailed
		} else {
			a.Status = StatusCooldown
			a.cooldownUntil = time.Now().Add(cooldownAfterFailure)
		}
		return
	}
}

// MarkCheckin stores a check-in result for the account.
func (s *Store) MarkCheckin(id string, ok bool, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		a.LastCheckinAt = time.Now().Format("2006-01-02 15:04:05")
		a.LastCheckinMsg = msg
		a.LastCheckinOK = ok
		return
	}
	_ = s.saveLocked()
}

// SaveState persists runtime changes (counters, status, check-in results).
func (s *Store) SaveState() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.saveLocked(); err != nil {
		slog.Warn("accounts: save state failed", slog.String("error", err.Error()))
	}
}

// ResetCooldowns clears transient cooldown state (used by the console's
// "全部恢复" action).
func (s *Store) ResetCooldowns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		a.cooldownUntil = time.Time{}
		if a.Status == StatusCooldown || a.Status == StatusAuthFailed {
			a.Status = StatusReady
		}
	}
	_ = s.saveLocked()
}

// Snapshots returns the masked, UI-facing view of the pool.
func (s *Store) Snapshots() []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Snapshot, 0, len(s.accounts))
	now := time.Now()
	for _, a := range s.accounts {
		st := a.Status
		text := statusText(st)
		if now.Before(a.cooldownUntil) && st == StatusCooldown {
			text = fmt.Sprintf("冷却中（%ds）", int(time.Until(a.cooldownUntil).Seconds())+1)
		}
		if !a.Enabled {
			text = "已禁用"
		}
		snap := Snapshot{
			ID:             a.ID,
			Name:           a.Name,
			Enabled:        a.Enabled,
			Priority:       a.Priority,
			Status:         st,
			StatusText:     text,
			TokenHint:      maskHint(a.Token),
			TokenSet:       a.Token != "",
			DeviceID:       a.DeviceID,
			InstallID:      a.InstallID,
			ProxyURL:       redactURL(a.ProxyURL),
			AutoCheckin:    a.AutoCheckin,
			CheckinTime:    a.CheckinTime,
			LastError:      a.LastError,
			LastUsedAt:     a.LastUsedAt,
			LastCheckinAt:  a.LastCheckinAt,
			LastCheckinMsg: a.LastCheckinMsg,
			LastCheckinOK:  a.LastCheckinOK,
			RequestCount:   a.RequestCount,
			ErrorCount:     a.ErrorCount,
		}
		if exp, ok := JWTExpiry(a.Token); ok {
			snap.ExpiresAt = exp.Local().Format("2006-01-02 15:04")
			snap.DaysLeft = int(time.Until(exp).Hours() / 24)
			snap.Expired = now.After(exp)
			if snap.Expired && snap.Enabled {
				snap.Status = StatusAuthFailed
				snap.StatusText = "凭证已过期"
			} else if snap.Enabled && snap.DaysLeft <= 7 && snap.Status == StatusReady {
				snap.StatusText = fmt.Sprintf("即将过期（剩 %d 天）", snap.DaysLeft)
			}
		}
		out = append(out, snap)
	}
	return out
}

func statusText(s string) string {
	switch s {
	case StatusReady:
		return "正常"
	case StatusCooldown:
		return "冷却中"
	case StatusAuthFailed:
		return "凭证失效"
	case StatusDisabled:
		return "已禁用"
	default:
		return "正常"
	}
}

func maskHint(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return s[:2] + "……"
	}
	return s[:6] + "……" + s[len(s)-4:]
}

func redactURL(u string) string {
	if u == "" {
		return ""
	}
	if at := strings.Index(u, "@"); at >= 0 {
		if se := strings.Index(u, "://"); se >= 0 && se+3 <= at {
			return u[:se+3] + "***@" + u[at+1:]
		}
		return "***@" + u[at+1:]
	}
	return u
}

func newID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1e12)
	}
	return hex.EncodeToString(buf)
}

func base64URLDecode(s string) ([]byte, error) {
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// JWTExpiry decodes the exp claim without verifying the signature.
func JWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return time.Time{}, false
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	decoded, err := base64URLDecode(payload)
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
