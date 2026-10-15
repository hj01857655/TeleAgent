package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"teleagent2api/internal/accounts"
	"teleagent2api/internal/middleware"
)

// accountsResponse is the console's account-pool payload.
type accountsResponse struct {
	Accounts []accounts.Snapshot `json:"accounts"`
	Total    int                 `json:"total"`
	Enabled  int                 `json:"enabled"`
	Healthy  int                 `json:"healthy"`
}

// ServeAccounts handles the account pool CRUD API.
//
//	GET    /v1/admin/accounts            列表
//	POST   /v1/admin/accounts            新增/更新（body 为账号对象）
//	DELETE /v1/admin/accounts?id=xxx     删除
func (h *Handler) ServeAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snaps := h.store.Snapshots()
		out := accountsResponse{Accounts: snaps, Total: len(snaps)}
		for _, s := range snaps {
			if s.Enabled {
				out.Enabled++
			}
			if s.Enabled && s.Status == accounts.StatusReady && !s.Expired {
				out.Healthy++
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var in struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Token         string `json:"token"`
			DeviceID      string `json:"deviceId"`
			InstallID     string `json:"installId"`
			Enabled       *bool  `json:"enabled"`
			Priority      int    `json:"priority"`
			ProxyURL      string `json:"proxyUrl"`
			ProxyPlatform string `json:"proxyPlatform"`
			ProxyAccount  string `json:"proxyAccount"`
			ProxyToken    string `json:"proxyToken"`
			AutoCheckin   bool   `json:"autoCheckin"`
			CheckinTime   string `json:"checkinTime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		if strings.TrimSpace(in.Token) == "" && in.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token 不能为空"})
			return
		}

		acc := accounts.Account{
			ID:            in.ID,
			Name:          strings.TrimSpace(in.Name),
			Token:         strings.TrimSpace(in.Token),
			DeviceID:      strings.TrimSpace(in.DeviceID),
			InstallID:     strings.TrimSpace(in.InstallID),
			Enabled:       true,
			Priority:      in.Priority,
			ProxyURL:      strings.TrimSpace(in.ProxyURL),
			ProxyPlatform: strings.TrimSpace(in.ProxyPlatform),
			ProxyAccount:  strings.TrimSpace(in.ProxyAccount),
			ProxyToken:    strings.TrimSpace(in.ProxyToken),
			AutoCheckin:   in.AutoCheckin,
			CheckinTime:   strings.TrimSpace(in.CheckinTime),
		}
		if in.Enabled != nil {
			acc.Enabled = *in.Enabled
		}
		// Editing without re-typing the token keeps the stored one.
		if acc.ID != "" && acc.Token == "" {
			if existing, ok := h.store.Get(acc.ID); ok {
				acc.Token = existing.Token
				if acc.DeviceID == "" {
					acc.DeviceID = existing.DeviceID
				}
				if acc.InstallID == "" {
					acc.InstallID = existing.InstallID
				}
				if acc.ProxyToken == "" {
					acc.ProxyToken = existing.ProxyToken
				}
			}
		}
		saved, err := h.store.Upsert(acc)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h.pool.Invalidate()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": saved.ID, "name": saved.Name})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 id"})
			return
		}
		if err := h.store.Delete(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ServeAccountAction runs a per-account operation:
//
//	POST /v1/admin/accounts/action  {"id":"...","action":"toggle|checkin|test|reset"}
func (h *Handler) ServeAccountAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	switch in.Action {
	case "reset":
		h.store.ResetCooldowns()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已重置全部账号状态"})
		return
	}

	acc, ok := h.store.Get(in.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "账号不存在"})
		return
	}

	switch in.Action {
	case "toggle":
		acc.Enabled = !acc.Enabled
		if _, err := h.store.Upsert(acc); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h.pool.Invalidate()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": acc.Enabled})

	case "checkin":
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		res := h.checkin.RunAll(ctx, acc.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "message": res.Message, "accounts": res.Accounts})

	case "test":
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		ip, err := h.testEgress(ctx, acc)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ip": ip})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知操作: " + in.Action})
	}
}

// ServeLogs returns the recent request ring buffer.
func (h *Handler) ServeLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &limit); err != nil || n != 1 {
			limit = 100
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": middleware.RecentLogs(limit)})
}
