package app

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func handleGetState(state *AppState, rtCfg *RuntimeConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := rtCfg.Get()
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		state.mu.RLock()
		allKeys := make([]PublicKeyView, 0, len(state.Keys))
		for _, k := range state.Keys {
			allKeys = append(allKeys, publicView(k))
		}
		state.mu.RUnlock()
		activeKeys := make([]PublicKeyView, 0)
		cooldownKeys := make([]PublicKeyView, 0)
		scrapKeys := make([]PublicKeyView, 0)
		for _, k := range allKeys {
			switch {
			case k.Scrapped:
				scrapKeys = append(scrapKeys, k)
			case k.Paused:
				cooldownKeys = append(cooldownKeys, k)
			default:
				activeKeys = append(activeKeys, k)
			}
		}
		// 排序：数字名按值升序，自定义名按字典序，数字优先排前面。
		byName := func(s []PublicKeyView) {
			sort.Slice(s, func(i, j int) bool {
				ai := strings.TrimPrefix(s[i].Name, "报废")
				aj := strings.TrimPrefix(s[j].Name, "报废")
				ni, ei := strconv.Atoi(ai)
				nj, ej := strconv.Atoi(aj)
				switch {
				case ei == nil && ej == nil:
					return ni < nj
				case ei == nil:
					return true
				case ej == nil:
					return false
				default:
					return ai < aj
				}
			})
		}
		byName(activeKeys)
		byName(cooldownKeys)
		byName(scrapKeys)
		var totalBalance, totalUsed, totalMonthly, activeBalance, overdraft float64
		for _, k := range activeKeys {
			totalBalance += k.LastBalance
			totalUsed += k.LastUsedTotal
			totalMonthly += k.MonthlySpentUSD
			activeBalance += k.LastBalance
			if k.LastBalance < 0 {
				overdraft += k.LastBalance
			}
		}
		for _, k := range cooldownKeys {
			totalBalance += k.LastBalance
			totalUsed += k.LastUsedTotal
			totalMonthly += k.MonthlySpentUSD
			if k.LastBalance < 0 {
				overdraft += k.LastBalance
			}
		}
		activeCount := len(activeKeys)
		stickyKeyName := ""
		state.mu.RLock()
		stickyMode := state.StickyMode
		stickyKeyID := state.StickyKeyID
		if stickyMode && stickyKeyID != "" {
			if sk, ok := state.Keys[stickyKeyID]; ok {
				stickyKeyName = sk.Name
			}
		}
		state.mu.RUnlock()
		retryCodes, maxRetryAttempts := state.retrySettings()
		writeJSON(w, http.StatusOK, map[string]any{
			"completion_window":     cfg.CompletionWindow,
			"keys":                  activeKeys,
			"cooldown_keys":         cooldownKeys,
			"scrap_keys":            scrapKeys,
			"active_count":          activeCount,
			"cooldown_count":        len(cooldownKeys),
			"scrap_count":           len(scrapKeys),
			"total_count":           activeCount + len(cooldownKeys),
			"month":                 time.Now().UTC().Format("2006-01"),
			"total_balance":         round4(totalBalance),
			"total_used":            round4(totalUsed),
			"total_monthly":         round4(totalMonthly),
			"overdraft":             round4(overdraft),
			"active_balance":        round4(activeBalance),
			"sticky_mode":           stickyMode,
			"sticky_key_id":         stickyKeyID,
			"sticky_key_name":       stickyKeyName,
			"retry_status_codes":    retryCodes,
			"max_retry_attempts":    maxRetryAttempts,
		})
	}
}

func handleSettings(state *AppState) http.HandlerFunc {
	type settingsReq struct {
		StatusCodes string `json:"status_codes"`
		MaxAttempts int    `json:"max_attempts"`
	}
	view := func() map[string]any {
		codes, maxAttempts := state.retrySettings()
		return map[string]any{
			"retry_status_codes": codes,
			"max_retry_attempts": maxAttempts,
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, view())
		case http.MethodPost:
			var req settingsReq
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			codes, err := normalizeRetryStatusCodes(req.StatusCodes)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			state.mu.Lock()
			state.RetryCodes = codes
			state.MaxAttempts = clampRetryAttempts(req.MaxAttempts)
			err = state.save()
			state.mu.Unlock()
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, view())
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
	}
}

func handleRetrySettings(state *AppState) http.HandlerFunc {
	return handleSettings(state)
}
