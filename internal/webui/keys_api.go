package webui

import (
	"encoding/json"
	"net/http"
	"strings"

	"teleagent2api/internal/apikeys"
	"teleagent2api/internal/middleware"
)

// ServeKeys handles the client API key management API.
//
//	GET    /v1/admin/keys          列表（密钥掩码）
//	POST   /v1/admin/keys          新建（返回一次性完整密钥）
//	PATCH  /v1/admin/keys          更新（名称/启用/模型范围/限流）
//	DELETE /v1/admin/keys?id=xxx   删除
func (h *Handler) ServeKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		snaps := h.keys.Snapshots()
		active := 0
		for _, k := range snaps {
			if k.Enabled {
				active++
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"keys":    snaps,
			"total":   len(snaps),
			"enabled": active,
			"legacy":  h.cfg().APIKey != "",
		})

	case http.MethodPost:
		var in struct {
			Name         string   `json:"name"`
			Models       []string `json:"models"`
			RateLimitRPM int      `json:"rateLimitRpm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		k, err := h.keys.Create(in.Name, cleanModels(in.Models), in.RateLimitRPM)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// The full secret is returned exactly once, at creation time.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"id":     k.ID,
			"name":   k.Name,
			"secret": k.Secret,
			"hint":   "完整密钥仅此一次显示，请立即复制保存",
		})

	case http.MethodPatch:
		var in struct {
			ID           string    `json:"id"`
			Name         *string   `json:"name"`
			Enabled      *bool     `json:"enabled"`
			Models       *[]string `json:"models"`
			RateLimitRPM *int      `json:"rateLimitRpm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json or missing id"})
			return
		}
		var models []string
		modelsSet := in.Models != nil
		if modelsSet {
			models = cleanModels(*in.Models)
		}
		if err := h.keys.Update(in.ID, in.Name, in.Enabled, models, modelsSet, in.RateLimitRPM); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 id"})
			return
		}
		if err := h.keys.Delete(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ModelStat is one model row in the console's model page.
type ModelStat struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnedBy     string `json:"ownedBy"`
	Requests    int64  `json:"requests"`
	Errors      int64  `json:"errors"`
	LastUsedAt  string `json:"lastUsedAt,omitempty"`
	AllowedKeys int    `json:"allowedKeys"`
	TotalKeys   int    `json:"totalKeys"`
}

// ServeModels lists the gateway's models with usage statistics.
func (h *Handler) ServeModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logs := middleware.RecentLogs(200)
	stats := map[string]*ModelStat{}
	for _, m := range h.cfg().Models {
		stats[m] = &ModelStat{ID: m, Name: m, OwnedBy: "TeleAgent"}
	}
	for _, l := range logs {
		if l.Model == "" {
			continue
		}
		st, ok := stats[l.Model]
		if !ok {
			st = &ModelStat{ID: l.Model, Name: l.Model, OwnedBy: "TeleAgent"}
			stats[l.Model] = st
		}
		st.Requests++
		if l.Status >= 400 {
			st.Errors++
		}
		if l.Time > st.LastUsedAt {
			st.LastUsedAt = l.Time
		}
	}

	keys := h.keys.Snapshots()
	out := make([]ModelStat, 0, len(stats))
	for _, st := range stats {
		st.TotalKeys = len(keys)
		for _, k := range keys {
			if k.AllModels {
				st.AllowedKeys++
				continue
			}
			for _, m := range k.Models {
				if m == st.ID {
					st.AllowedKeys++
					break
				}
			}
		}
		out = append(out, *st)
	}
	// Stable order: configured models first (config order), then extras.
	ordered := make([]ModelStat, 0, len(out))
	seen := map[string]bool{}
	for _, m := range h.cfg().Models {
		for _, st := range out {
			if st.ID == m && !seen[st.ID] {
				ordered = append(ordered, st)
				seen[st.ID] = true
			}
		}
	}
	for _, st := range out {
		if !seen[st.ID] {
			ordered = append(ordered, st)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models":   ordered,
		"endpoint": "/v1/chat/completions",
		"note":     "模型由 TeleAgent 上游提供，可用性以实际调用为准",
	})
}

func cleanModels(in []string) []string {
	out := make([]string, 0, len(in))
	for _, m := range in {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ensure apikeys import is used even if the file is trimmed later.
var _ = apikeys.Key{}
