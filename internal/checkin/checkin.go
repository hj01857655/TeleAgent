// Package checkin implements the TeleAgent Star Welfare Club (积分福利社)
// daily check-in: it periodically calls the portal API
//
//	GET /superCowork/sapi/user/portal/companyStore/checkTask?type=daily_login
//
// behind the same superCowork/sapi gateway used for chat completions,
// authenticating with the same JWT + SuperAgent headers.
package checkin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"teleagent2api/internal/accounts"
	"teleagent2api/internal/config"
	"teleagent2api/internal/proxy"
)

const (
	// Task types understood by the welfare portal.
	TypeDailyLogin = "daily_login"   // 每日签到
	TypeFirstLogin = "first_login"   // 首登奖励
	TypeMonthly    = "monthly_basic" // 每月登录基础奖励

	checkTaskPath = "/user/portal/companyStore/checkTask"
	dayTaskPath   = "/user/portal/companyStore/v2/checkDayTask"
)

// Service runs scheduled check-ins and exposes a manual trigger.
type Service struct {
	live  *config.Live
	store *accounts.Store
	pool  *proxy.ClientPool
	loc   *time.Location

	mu         sync.Mutex
	lastDate   string // last date (YYYY-MM-DDD in loc) a run was attempted
	lastResult RunResult

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// RunResult records the outcome of the most recent run.
type RunResult struct {
	Time     time.Time       `json:"time"`
	OK       bool            `json:"ok"`
	Claimed  bool            `json:"claimed"` // true when at least one account claimed now
	Message  string          `json:"message"` // human-readable outcome
	Accounts []AccountResult `json:"accounts,omitempty"`
}

// AccountResult is one account's check-in outcome.
type AccountResult struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	OK          bool   `json:"ok"`
	Claimed     bool   `json:"claimed"`
	Message     string `json:"message"`
}

// NewService creates the check-in service. The scheduler only starts when
// cfg.CheckInEnabled is true; manual runs are always available.
func NewService(live *config.Live, store *accounts.Store, pool *proxy.ClientPool) *Service {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	return &Service{
		live:   live,
		store:  store,
		pool:   pool,
		loc:    loc,
		stopCh: make(chan struct{}),
	}
}

// cfg returns the current live configuration snapshot.
func (s *Service) cfg() config.Config {
	return s.live.Get()
}

// Start launches the daily scheduler in the background.
func (s *Service) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		slog.Info("checkin scheduler started",
			slog.String("time", s.cfg().CheckInTime),
			slog.String("tz", "Asia/Shanghai"),
			slog.String("type", s.cfg().CheckInType))
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				if s.due() {
					ctx, cancel := context.WithTimeout(context.Background(), s.cfg().Timeout)
					res := s.RunAll(ctx, "")
					cancel()
					slog.Info("scheduled checkin finished",
						slog.Bool("ok", res.OK),
						slog.Bool("claimed", res.Claimed),
						slog.String("message", res.Message))
				}
			}
		}
	}()
}

// Stop terminates the scheduler.
func (s *Service) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.wg.Wait()
	})
}

// LastResult returns the outcome of the most recent run (if any).
func (s *Service) LastResult() RunResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastResult
}

// due reports whether it is time to run today's check-in (and it hasn't run yet).
func (s *Service) due() bool {
	now := time.Now().In(s.loc)
	today := now.Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastDate == today {
		return false
	}
	return now.Format("15:04") >= s.cfg().CheckInTime
}

// RunAll performs the check-in for every enabled account (or a single
// account when onlyID is set), recording each result on the account pool.
func (s *Service) RunAll(ctx context.Context, onlyID string) RunResult {
	res := RunResult{Time: time.Now().In(s.loc)}

	s.mu.Lock()
	s.lastDate = time.Now().In(s.loc).Format("2006-01-02")
	s.mu.Unlock()

	var targets []accounts.Account
	for _, a := range s.store.List() {
		if !a.Enabled || a.Token == "" {
			continue
		}
		if onlyID != "" && a.ID != onlyID {
			continue
		}
		// When onlyID is empty (scheduled run) honour the per-account switch;
		// an explicit single-account run ignores it.
		if onlyID == "" && !a.AutoCheckin {
			continue
		}
		targets = append(targets, a)
	}
	if len(targets) == 0 {
		res.Message = "no account eligible for check-in (enable 自动签到 on an account, or pass ?account=<id>)"
		s.record(res)
		return res
	}

	okCount, claimCount := 0, 0
	for _, acc := range targets {
		r := s.runFor(ctx, acc)
		res.Accounts = append(res.Accounts, r)
		s.store.MarkCheckin(acc.ID, r.OK, r.Message)
		if r.OK {
			okCount++
		}
		if r.Claimed {
			claimCount++
		}
	}
	s.store.SaveState()

	res.OK = okCount == len(targets)
	res.Claimed = claimCount > 0
	res.Message = fmt.Sprintf("%d/%d 个账号签到成功，本次新领取 %d 个", okCount, len(targets), claimCount)
	s.record(res)
	return res
}

// RunOnce keeps the single-account behaviour for the scheduled path.
func (s *Service) RunOnce(ctx context.Context) RunResult {
	return s.RunAll(ctx, "")
}

// runFor checks in one account.
func (s *Service) runFor(ctx context.Context, acc accounts.Account) AccountResult {
	out := AccountResult{AccountID: acc.ID, AccountName: acc.Name}

	status, err := s.fetchDayTask(ctx, acc)
	if err != nil {
		out.Message = fmt.Sprintf("查询任务状态失败: %v", err)
		return out
	}
	if status.ClaimedToday {
		out.OK = true
		out.Message = fmt.Sprintf("今日已签到（积分 %s，连续 %d 天）", orDash(status.Points), status.StreakDays)
		return out
	}
	if err := s.claim(ctx, acc); err != nil {
		out.Message = fmt.Sprintf("签到失败: %v", err)
		return out
	}
	out.OK = true
	out.Claimed = true
	out.Message = "签到成功，积分已到账"
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Status is the response payload for GET /checkin.
type Status struct {
	Enabled  bool       `json:"enabled"`
	Schedule string     `json:"schedule"` // "HH:MM Asia/Shanghai"
	Type     string     `json:"type"`
	Last     *RunResult `json:"last,omitempty"`
}

func (s *Service) Status() Status {
	last := s.LastResult()
	return Status{
		Enabled:  s.cfg().CheckInEnabled,
		Schedule: s.cfg().CheckInTime + " Asia/Shanghai",
		Type:     s.cfg().CheckInType,
		Last:     &last,
	}
}

func (s *Service) record(res RunResult) {
	s.mu.Lock()
	s.lastResult = res
	s.mu.Unlock()
}

// dayTaskStatus is the parsed daily-task payload.
type dayTaskStatus struct {
	ClaimedToday bool
	Points       string
	StreakDays   int
}

// fetchDayTask queries the portal for today's task state.
func (s *Service) fetchDayTask(ctx context.Context, acc accounts.Account) (dayTaskStatus, error) {
	var out dayTaskStatus
	body, status, err := s.portalGet(ctx, acc, dayTaskPath, "")
	if err != nil {
		return out, err
	}
	if status != http.StatusOK {
		return out, fmt.Errorf("upstream status %d for %s", status, dayTaskPath)
	}
	// The payload nests under data; claimed_today may also appear as
	// daily_task.claimed_today depending on the API version. Scan defensively.
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	claimed, streak, points := scanDayTask(body)
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Data != nil {
		if v, ok := envelope.Data["daily_task"]; ok {
			if c, st, p, ok2 := parseDailyTask(v); ok2 {
				claimed, streak, points = c, st, p
			}
		}
	}
	out.ClaimedToday = claimed
	out.StreakDays = streak
	out.Points = points
	return out, nil
}

func parseDailyTask(raw json.RawMessage) (claimed bool, streak int, points string, ok bool) {
	var t struct {
		ClaimedToday   *bool           `json:"claimed_today"`
		ContinuousDays *int            `json:"continuous_days"`
		Days           *int            `json:"days"`
		Points         json.RawMessage `json:"points"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return false, 0, "", false
	}
	ok = true
	if t.ClaimedToday != nil {
		claimed = *t.ClaimedToday
	}
	if t.ContinuousDays != nil {
		streak = *t.ContinuousDays
	} else if t.Days != nil {
		streak = *t.Days
	}
	if len(t.Points) > 0 {
		var s string
		if json.Unmarshal(t.Points, &s) == nil {
			points = s
		} else {
			points = string(t.Points)
		}
	}
	return claimed, streak, points, ok
}

// scanDayTask walks arbitrary JSON looking for claimed_today / points / days.
func scanDayTask(body []byte) (claimed bool, streak int, points string) {
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if b, ok := t["claimed_today"].(bool); ok && b {
				claimed = true
			}
			if f, ok := toFloat(t["points"]); ok {
				points = fmt.Sprintf("%v", f)
			} else if str, ok := t["points"].(string); ok {
				points = str
			}
			if f, ok := toFloat(t["continuous_days"]); ok {
				streak = int(f)
			} else if f, ok := toFloat(t["days"]); ok {
				streak = int(f)
			}
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err == nil {
		walk(doc)
	}
	return claimed, streak, points
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// claim performs the actual check-in request.
func (s *Service) claim(ctx context.Context, acc accounts.Account) error {
	taskType := s.cfg().CheckInType
	if taskType == "" {
		taskType = TypeDailyLogin
	}
	body, status, err := s.portalGet(ctx, acc, checkTaskPath, "?type="+taskType)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("upstream status %d for %s", status, checkTaskPath)
	}
	// The portal answers {"code":"...","message":"...","data":...}.
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("unparseable portal response for %s (status %d)", checkTaskPath, status)
	}
	codeStr := strings.Trim(string(envelope.Code), `"`)
	if codeStr != "" && codeStr != "0" && codeStr != "200" {
		return fmt.Errorf("portal code %s: %s", codeStr, envelope.Message)
	}
	slog.Info("checkin portal response", slog.String("code", codeStr), slog.String("message", envelope.Message))
	return nil
}

// portalGet issues a GET against the portal API with the same auth headers
// used for chat completions.
func (s *Service) portalGet(ctx context.Context, acc accounts.Account, path, query string) ([]byte, int, error) {
	u := s.cfg().PortalBaseURL() + path + query
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	s.applyAuthHeaders(req, acc)

	resp, err := s.pool.For(acc).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}

	// The portal answers JSON ({"code":...,"message":...,"data":...}).
	// When the JWT is missing/invalid/expired the gateway answers 200 with
	// an HTML login page instead of a JSON error — never treat that as
	// success, otherwise the check-in would silently no-op forever.
	// The error message intentionally omits the upstream body: it may be a
	// full HTML page, and echoing it would pollute logs and the API reply.
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	trimmed := strings.TrimSpace(string(body))
	if !strings.Contains(ct, "json") || strings.HasPrefix(trimmed, "<") {
		return body, resp.StatusCode, fmt.Errorf(
			"non-JSON response (status=%d, content-type=%q): token is likely invalid or expired (server returned a login page); refresh TELEAGENT_TOKEN from the desktop client",
			resp.StatusCode, ct)
	}
	return body, resp.StatusCode, nil
}

// applyAuthHeaders mirrors the desktop client's portal request headers:
// X-Token (JWT) plus the SuperAgent device attestation headers.
func (s *Service) applyAuthHeaders(req *http.Request, acc accounts.Account) {
	if key := s.cfg().UpstreamAPIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if s.cfg().RouteTarget != "" {
		req.Header.Set("X-Route-Target", s.cfg().RouteTarget)
	}
	req.Header.Set("X-Token", acc.Token)
	req.Header.Set("X-SuperAgent-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("X-SuperAgent-Nonce", randomHexUUID())
	req.Header.Set("X-SuperAgent-Device-Id", acc.DeviceID)
	req.Header.Set("X-SuperAgent-Install-Id", acc.InstallID)
	req.Header.Set("X-App-Version", s.cfg().AppVersion)
	req.Header.Set("User-Agent", s.cfg().UserAgent)
	req.Header.Set("Accept", "application/json")
	if s.cfg().CheckInUserID != "" {
		// The welfare portal also accepts a userId header.
		req.Header.Set("userId", s.cfg().CheckInUserID)
	}
}

// randomHexUUID returns a v4 UUID in dashed hex form.
func randomHexUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand should never fail; fall back to time-based entropy.
		for i := range buf {
			buf[i] = byte(time.Now().UnixNano() >> (uint(i%8) * 8))
		}
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return hex.EncodeToString(buf[0:4]) + "-" +
		hex.EncodeToString(buf[4:6]) + "-" +
		hex.EncodeToString(buf[6:8]) + "-" +
		hex.EncodeToString(buf[8:10]) + "-" +
		hex.EncodeToString(buf[10:16])
}
