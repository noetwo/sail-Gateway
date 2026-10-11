package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func loadRuntimeConfig(seed Config) (*RuntimeConfig, error) {
	if err := os.MkdirAll(filepath.Dir(seed.RuntimeConfigPath), 0o755); err != nil {
		return nil, err
	}
	rt := &RuntimeConfig{filePath: seed.RuntimeConfigPath, current: normalizeConfig(seed)}
	if _, err := os.Stat(seed.RuntimeConfigPath); errors.Is(err, os.ErrNotExist) {
		if err := rt.saveLocked(); err != nil {
			return nil, err
		}
		return rt, nil
	}
	b, err := os.ReadFile(seed.RuntimeConfigPath)
	if err != nil {
		return nil, err
	}
	var disk runtimeConfigFile
	if len(b) > 0 {
		if err := json.Unmarshal(b, &disk); err != nil {
			return nil, err
		}
	}
	cfg := seed
	if strings.TrimSpace(disk.ListenAddr) != "" {
		cfg.ListenAddr = strings.TrimSpace(disk.ListenAddr)
	}
	if base := strings.TrimSpace(disk.GatewayBaseURL); base != "" && !strings.Contains(base, legacyVercelBaseHost) {
		cfg.GatewayBaseURL = strings.TrimRight(base, "/")
	}
	if disk.CompletionWindow != nil {
		cfg.CompletionWindow = normalizeCompletionWindow(*disk.CompletionWindow)
	}
	if disk.StripUnsupported != nil {
		cfg.StripUnsupported = *disk.StripUnsupported
	}
	if strings.TrimSpace(disk.TestModel) != "" {
		cfg.TestModel = strings.TrimSpace(disk.TestModel)
	}
	// 上一版默认 168 小时；现在默认关闭，旧默认值视为未设置。
	if disk.AutoRefreshHours != nil && *disk.AutoRefreshHours != 168 {
		cfg.AutoRefreshHours = *disk.AutoRefreshHours
	}
	if strings.TrimSpace(disk.WebAuthToken) != "" {
		cfg.WebAuthToken = strings.TrimSpace(disk.WebAuthToken)
	}
	if len(disk.ApiAuthTokens) > 0 {
		cfg.ApiAuthTokens = cleanTokens(disk.ApiAuthTokens)
		if len(cfg.ApiAuthTokens) > 0 {
			cfg.ApiAuthToken = cfg.ApiAuthTokens[0]
		}
	}
	// 旧版默认把 402 放在「冷却」里；一次性额度用完不会恢复，迁移到「报废」。
	if strings.TrimSpace(disk.CooldownStatusCodes) == "402" {
		disk.CooldownStatusCodes = ""
	}
	cfg.CooldownStatusCodes = normalizeStatusCodesOrDefault(disk.CooldownStatusCodes, "")
	if disk.ExhaustStatusCodes != nil {
		cfg.ExhaustStatusCodes = normalizeStatusCodesOrDefault(*disk.ExhaustStatusCodes, "")
	}
	cfg.ProxyScrapStatusCodes = normalizeStatusCodesOrDefault(disk.ProxyScrapStatusCodes, cfg.ProxyScrapStatusCodes)
	if disk.ProxyScrapFailThreshold > 0 {
		cfg.ProxyScrapFailThreshold = disk.ProxyScrapFailThreshold
	}
	cfg.RefreshScrapStatusCodes = normalizeStatusCodesOrDefault(disk.RefreshScrapStatusCodes, cfg.RefreshScrapStatusCodes)
	cfg.RefreshCooldownStatusCodes = normalizeStatusCodesOrDefault(disk.RefreshCooldownStatusCodes, cfg.RefreshCooldownStatusCodes)
	rt.current = normalizeConfig(cfg)
	if err := rt.saveLocked(); err != nil {
		return nil, err
	}
	return rt, nil
}

func normalizeConfig(cfg Config) Config {
	cfg.GatewayBaseURL = strings.TrimRight(strings.TrimSpace(cfg.GatewayBaseURL), "/")
	if cfg.GatewayBaseURL == "" {
		cfg.GatewayBaseURL = defaultGatewayBase
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	cfg.CompletionWindow = normalizeCompletionWindow(cfg.CompletionWindow)
	cfg.TestModel = strings.TrimSpace(cfg.TestModel)
	if cfg.TestModel == "" {
		cfg.TestModel = defaultTestModel
	}
	if cfg.AutoRefreshHours < 0 {
		cfg.AutoRefreshHours = 0
	}
	if cfg.AutoRefreshHours > maxAutoRefreshHours {
		cfg.AutoRefreshHours = maxAutoRefreshHours
	}
	cfg.WebAuthToken = strings.TrimSpace(cfg.WebAuthToken)
	cfg.ApiAuthTokens = cleanTokens(cfg.ApiAuthTokens)
	if len(cfg.ApiAuthTokens) == 0 && strings.TrimSpace(cfg.ApiAuthToken) != "" {
		cfg.ApiAuthTokens = cleanTokens([]string{cfg.ApiAuthToken})
	}
	cfg.ApiAuthToken = ""
	if len(cfg.ApiAuthTokens) > 0 {
		cfg.ApiAuthToken = cfg.ApiAuthTokens[0]
	}
	cfg.CooldownStatusCodes = normalizeStatusCodesOrDefault(cfg.CooldownStatusCodes, "")
	cfg.ExhaustStatusCodes = normalizeStatusCodesOrDefault(cfg.ExhaustStatusCodes, "")
	cfg.ProxyScrapStatusCodes = normalizeStatusCodesOrDefault(cfg.ProxyScrapStatusCodes, defaultProxyScrapCodes)
	if cfg.ProxyScrapFailThreshold <= 0 {
		cfg.ProxyScrapFailThreshold = 3
	}
	cfg.RefreshScrapStatusCodes = normalizeStatusCodesOrDefault(cfg.RefreshScrapStatusCodes, defaultRefreshScrapCodes)
	cfg.RefreshCooldownStatusCodes = normalizeStatusCodesOrDefault(cfg.RefreshCooldownStatusCodes, "")
	return cfg
}

func (rt *RuntimeConfig) Get() Config {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	cfg := rt.current
	cfg.ApiAuthTokens = append([]string(nil), rt.current.ApiAuthTokens...)
	return cfg
}

func (rt *RuntimeConfig) Update(next Config) (Config, error) {
	next.RuntimeConfigPath = rt.filePath
	next.StateFilePath = rt.current.StateFilePath
	next = normalizeConfig(next)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.current = next
	if err := rt.saveLocked(); err != nil {
		return Config{}, err
	}
	out := rt.current
	out.ApiAuthTokens = append([]string(nil), rt.current.ApiAuthTokens...)
	return out, nil
}

func (rt *RuntimeConfig) saveLocked() error {
	cfg := normalizeConfig(rt.current)
	disk := runtimeConfigFile{
		ListenAddr:                 cfg.ListenAddr,
		GatewayBaseURL:             cfg.GatewayBaseURL,
		WebAuthToken:               cfg.WebAuthToken,
		ApiAuthTokens:              append([]string(nil), cfg.ApiAuthTokens...),
		CompletionWindow:           &cfg.CompletionWindow,
		StripUnsupported:           &cfg.StripUnsupported,
		TestModel:                  cfg.TestModel,
		AutoRefreshHours:           &cfg.AutoRefreshHours,
		CooldownStatusCodes:        cfg.CooldownStatusCodes,
		ExhaustStatusCodes:         &cfg.ExhaustStatusCodes,
		ProxyScrapStatusCodes:      cfg.ProxyScrapStatusCodes,
		ProxyScrapFailThreshold:    cfg.ProxyScrapFailThreshold,
		RefreshScrapStatusCodes:    cfg.RefreshScrapStatusCodes,
		RefreshCooldownStatusCodes: cfg.RefreshCooldownStatusCodes,
		LastSaved:                  time.Now().UTC(),
	}
	b, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	tmp := rt.filePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, rt.filePath)
}
