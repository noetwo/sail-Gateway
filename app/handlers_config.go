package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func handleConfig(rtCfg *RuntimeConfig) http.HandlerFunc {
	type configReq struct {
		ListenAddr                 string   `json:"listen_addr"`
		GatewayBaseURL             string   `json:"gateway_base_url"`
		WebAuthToken               string   `json:"web_auth_token"`
		ApiAuthTokens              []string `json:"api_auth_tokens"`
		CompletionWindow           *string  `json:"completion_window"`
		StripUnsupported           *bool    `json:"strip_unsupported_params"`
		TestModel                  *string  `json:"test_model"`
		AutoRefreshHours           *int     `json:"auto_refresh_hours"`
		CooldownStatusCodes        string   `json:"cooldown_status_codes"`
		ExhaustStatusCodes         *string  `json:"exhaust_status_codes"`
		ProxyScrapStatusCodes      string   `json:"proxy_scrap_status_codes"`
		ProxyScrapFailThreshold    int      `json:"proxy_scrap_fail_threshold"`
		RefreshScrapStatusCodes    string   `json:"refresh_scrap_status_codes"`
		RefreshCooldownStatusCodes string   `json:"refresh_cooldown_status_codes"`
	}
	view := func(cfg Config) map[string]any {
		return map[string]any{
			"listen_addr":                   cfg.ListenAddr,
			"gateway_base_url":              cfg.GatewayBaseURL,
			"web_auth_token":                cfg.WebAuthToken,
			"api_auth_tokens":               cfg.ApiAuthTokens,
			"completion_window":             cfg.CompletionWindow,
			"strip_unsupported_params":      cfg.StripUnsupported,
			"test_model":                    cfg.TestModel,
			"auto_refresh_hours":            cfg.AutoRefreshHours,
			"cooldown_status_codes":         cfg.CooldownStatusCodes,
			"exhaust_status_codes":          cfg.ExhaustStatusCodes,
			"proxy_scrap_status_codes":      cfg.ProxyScrapStatusCodes,
			"proxy_scrap_fail_threshold":    cfg.ProxyScrapFailThreshold,
			"refresh_scrap_status_codes":    cfg.RefreshScrapStatusCodes,
			"refresh_cooldown_status_codes": cfg.RefreshCooldownStatusCodes,
			"runtime_config_path":           cfg.RuntimeConfigPath,
			"listen_addr_restart_required":  true,
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, view(rtCfg.Get()))
		case http.MethodPost:
			var req configReq
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			cfg := rtCfg.Get()
			cfg.ListenAddr = strings.TrimSpace(req.ListenAddr)
			cfg.GatewayBaseURL = strings.TrimSpace(req.GatewayBaseURL)
			cfg.WebAuthToken = strings.TrimSpace(req.WebAuthToken)
			cfg.ApiAuthTokens = cleanTokens(req.ApiAuthTokens)
			if req.CompletionWindow != nil {
				win := strings.ToLower(strings.TrimSpace(*req.CompletionWindow))
				if !isValidCompletionWindow(win) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "completion_window must be asap / balanced / flex"})
					return
				}
				cfg.CompletionWindow = win
			}
			if req.StripUnsupported != nil {
				cfg.StripUnsupported = *req.StripUnsupported
			}
			if req.TestModel != nil {
				cfg.TestModel = strings.TrimSpace(*req.TestModel)
			}
			if req.AutoRefreshHours != nil {
				cfg.AutoRefreshHours = *req.AutoRefreshHours
			}
			cfg.CooldownStatusCodes = strings.TrimSpace(req.CooldownStatusCodes)
			if req.ExhaustStatusCodes != nil {
				cfg.ExhaustStatusCodes = strings.TrimSpace(*req.ExhaustStatusCodes)
			}
			cfg.ProxyScrapStatusCodes = strings.TrimSpace(req.ProxyScrapStatusCodes)
			cfg.ProxyScrapFailThreshold = req.ProxyScrapFailThreshold
			cfg.RefreshScrapStatusCodes = strings.TrimSpace(req.RefreshScrapStatusCodes)
			cfg.RefreshCooldownStatusCodes = strings.TrimSpace(req.RefreshCooldownStatusCodes)
			if cfg.WebAuthToken == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "web auth token cannot be empty"})
				return
			}
			saved, err := rtCfg.Update(cfg)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, view(saved))
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
	}
}
