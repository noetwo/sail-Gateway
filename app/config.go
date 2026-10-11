package app

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RuntimeConfig struct {
	mu       sync.RWMutex
	filePath string
	current  Config
}

type runtimeConfigFile struct {
	ListenAddr                 string    `json:"listen_addr"`
	GatewayBaseURL             string    `json:"gateway_base_url"`
	WebAuthToken               string    `json:"web_auth_token"`
	ApiAuthTokens              []string  `json:"api_auth_tokens"`
	CompletionWindow           *string   `json:"completion_window,omitempty"`
	StripUnsupported           *bool     `json:"strip_unsupported_params,omitempty"`
	TestModel                  string    `json:"test_model,omitempty"`
	AutoRefreshHours           *int      `json:"auto_refresh_hours,omitempty"`
	CooldownStatusCodes        string    `json:"cooldown_status_codes"`
	ExhaustStatusCodes         *string   `json:"exhaust_status_codes,omitempty"`
	ProxyScrapStatusCodes      string    `json:"proxy_scrap_status_codes"`
	ProxyScrapFailThreshold    int       `json:"proxy_scrap_fail_threshold"`
	RefreshScrapStatusCodes    string    `json:"refresh_scrap_status_codes"`
	RefreshCooldownStatusCodes string    `json:"refresh_cooldown_status_codes"`
	LastSaved                  time.Time `json:"last_saved"`
}

func readConfig() Config {
	listen := getenvDefault("LISTEN_ADDR", defaultListenAddr)
	base := strings.TrimRight(getenvDefault("GATEWAY_BASE_URL", defaultGatewayBase), "/")
	stateDir := getenvDefault("STATE_DIR", defaultStateDirName)
	webAuthToken := strings.TrimSpace(os.Getenv("WEB_AUTH_TOKEN"))
	apiAuthTokens := parseTokenList(os.Getenv("API_AUTH_TOKEN"))
	apiAuthToken := ""
	if len(apiAuthTokens) > 0 {
		apiAuthToken = apiAuthTokens[0]
	}
	stripUnsupported := !strings.EqualFold(strings.TrimSpace(os.Getenv("STRIP_UNSUPPORTED_PARAMS")), "false")
	return Config{
		ListenAddr:                 listen,
		GatewayBaseURL:             base,
		StateFilePath:              filepath.Join(stateDir, defaultStateFile),
		RuntimeConfigPath:          filepath.Join(stateDir, defaultRuntimeConfigFile),
		WebAuthToken:               webAuthToken,
		ApiAuthToken:               apiAuthToken,
		ApiAuthTokens:              apiAuthTokens,
		CompletionWindow:           normalizeCompletionWindow(getenvDefault("COMPLETION_WINDOW", defaultCompletionWindow)),
		StripUnsupported:           stripUnsupported,
		TestModel:                  getenvDefault("TEST_MODEL", defaultTestModel),
		AutoRefreshHours:           getenvIntDefault("AUTO_REFRESH_HOURS", defaultAutoRefreshHours, 0, maxAutoRefreshHours),
		CooldownStatusCodes:        normalizeStatusCodesOrDefault(os.Getenv("COOLDOWN_STATUS_CODES"), defaultCooldownCodes),
		ExhaustStatusCodes:         normalizeStatusCodesOrDefault(os.Getenv("EXHAUST_STATUS_CODES"), defaultExhaustCodes),
		ProxyScrapStatusCodes:      normalizeStatusCodesOrDefault(os.Getenv("PROXY_SCRAP_STATUS_CODES"), defaultProxyScrapCodes),
		ProxyScrapFailThreshold:    getenvIntDefault("PROXY_SCRAP_FAIL_THRESHOLD", 3, 1, 50),
		RefreshScrapStatusCodes:    normalizeStatusCodesOrDefault(os.Getenv("REFRESH_SCRAP_STATUS_CODES"), defaultRefreshScrapCodes),
		RefreshCooldownStatusCodes: normalizeStatusCodesOrDefault(os.Getenv("REFRESH_COOLDOWN_STATUS_CODES"), ""),
	}
}
