package app

import (
	"errors"
	"log"
	"net/http"
	"time"
)

// Run 启动整个服务。loginHTML / indexHTML 由调用方（顶层 main.go）通过 //go:embed 注入。
func Run(loginHTML, indexHTML string) {
	loadConfigFile(defaultConfigFile)
	seedCfg := readConfig()
	rtCfg, err := loadRuntimeConfig(seedCfg)
	if err != nil {
		log.Fatalf("load runtime config failed: %v", err)
	}
	cfg := rtCfg.Get()
	state, err := loadState(cfg.StateFilePath)
	if err != nil {
		log.Fatalf("load state failed: %v", err)
	}

	if cfg.WebAuthToken == "" {
		log.Fatalf("WEB_AUTH_TOKEN is required (set in config.txt or env), cannot be empty")
	}
	webAuth := requireDynamicAuth(func() []string { return []string{rtCfg.Get().WebAuthToken} })
	apiAuth := requireDynamicAuthFailClosed(func() []string { return rtCfg.Get().ApiAuthTokens })
	log.Printf("auth enabled (web token length=%d, api tokens=%d)", len(cfg.WebAuthToken), len(cfg.ApiAuthTokens))

	proxyLogs := newProxyLogRing(500)
	catalog := newModelCatalog()
	affinity := newKeyAffinity(48*time.Hour, 50000)
	balances := newBalanceRefresher(15 * time.Second)
	proxy := withCORS(apiAuth(handleGatewayProxy(rtCfg, state, proxyLogs, catalog, affinity, balances)))
	go backgroundLoop(rtCfg, state)

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndexDynamic(func() string { return rtCfg.Get().WebAuthToken }, loginHTML, indexHTML))
	mux.HandleFunc("/api/login", withCORS(handleDynamicLogin(func() string { return rtCfg.Get().WebAuthToken })))
	mux.HandleFunc("/api/logout", withCORS(handleLogout))
	mux.HandleFunc("/api/state", withCORS(webAuth(handleGetState(state, rtCfg))))
	mux.HandleFunc("/api/settings", withCORS(webAuth(handleSettings(state))))
	mux.HandleFunc("/api/retry-settings", withCORS(webAuth(handleRetrySettings(state))))
	mux.HandleFunc("/api/config", withCORS(webAuth(handleConfig(rtCfg))))
	mux.HandleFunc("/api/models", withCORS(webAuth(handleModelSettings(rtCfg, state, catalog))))
	mux.HandleFunc("/api/models/probe", withCORS(webAuth(handleModelSettings(rtCfg, state, catalog))))
	mux.HandleFunc("/api/export/keys", withCORS(webAuth(handleExportKeys(state))))
	mux.HandleFunc("/api/refresh", withCORS(webAuth(handleRefresh(rtCfg, state))))
	mux.HandleFunc("/api/test/", withCORS(webAuth(handleTestKey(rtCfg, state))))
	mux.HandleFunc("/api/sticky", withCORS(webAuth(handleStickyToggle(state))))
	mux.HandleFunc("/api/sticky/select", withCORS(webAuth(handleStickySelect(state))))
	mux.HandleFunc("/api/logs", withCORS(webAuth(handleLogs(proxyLogs))))
	mux.HandleFunc("/api/keys", withCORS(webAuth(handleKeys(state))))
	mux.HandleFunc("/api/keys/bulk", withCORS(webAuth(handleKeysBulk(state))))
	mux.HandleFunc("/api/keys/batch", withCORS(webAuth(handleKeysBatch(state))))
	mux.HandleFunc("/api/keys/batch/test", withCORS(webAuth(handleKeysBatchTest(rtCfg, state))))
	mux.HandleFunc("/api/keys/", withCORS(webAuth(handleKeyByID(state))))
	mux.HandleFunc("/v1", proxy)
	mux.HandleFunc("/v1/", proxy)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           logRequest(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("Sail gateway listening on http://%s", displayListenAddr(cfg.ListenAddr))
	log.Printf("upstream=%s window=%s auto_refresh=%dh", cfg.GatewayBaseURL, cfg.CompletionWindow, cfg.AutoRefreshHours)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

// backgroundLoop 负责两件事：
//  1. 定期把节流期间未落盘的状态写入 state.json；
//  2. 按配置的间隔自动刷新所有 key 的余额（被自动冷却的 key 在有余额后会自动恢复）。
func backgroundLoop(rtCfg *RuntimeConfig, state *AppState) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		state.flushIfDirty()
		cfg := rtCfg.Get()
		if cfg.AutoRefreshHours <= 0 {
			continue
		}
		if !state.autoRefreshDue(time.Duration(cfg.AutoRefreshHours) * time.Hour) {
			continue
		}
		ok, failed := refreshAllKeys(cfg, state)
		log.Printf("auto refresh balance: ok=%d failed=%d", ok, failed)
	}
}
