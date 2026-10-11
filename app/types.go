package app

import (
	"sync"
	"time"
)

const (
	defaultListenAddr        = ":9090"
	defaultGatewayBase       = "https://api.sailresearch.com/v1"
	legacyVercelBaseHost     = "ai-gateway.vercel.sh"
	defaultCompletionWindow  = "balanced"
	defaultTestModel         = "google/gemma-4-12B-it"
	defaultAutoRefreshHours  = 0   // 默认关闭：一次性额度只会减少，靠请求后自动查余额即可
	maxAutoRefreshHours      = 720 // 30 天
	defaultStateDirName      = "data"
	defaultStateFile         = "state.json"
	defaultRuntimeConfigFile = "config.json"
	maxCaptureBytes          = 2 << 20
	defaultConfigFile        = "config.txt"
	defaultRetryStatusCodes  = "401,402,403,429,503"
	legacyRetryStatusCodes   = "400,401,402,403,429,5xx"
	defaultCooldownCodes     = ""
	defaultExhaustCodes      = "402"
	defaultProxyScrapCodes   = "401"
	defaultRefreshScrapCodes = "401"
	defaultMaxRetryAttempts  = 20
	maxAllowedRetryAttempts  = 20
)

type AppState struct {
	mu            sync.RWMutex
	filePath      string
	proxyTurn     uint64
	dirty         bool
	LastSaved     time.Time                `json:"last_saved"`
	Keys          map[string]*Key          `json:"keys"`
	StickyMode    bool                     `json:"sticky_mode"`
	StickyKeyID   string                   `json:"sticky_key_id,omitempty"`
	RetryCodes    string                   `json:"retry_status_codes,omitempty"`
	MaxAttempts   int                      `json:"max_retry_attempts,omitempty"`
	ModelSettings map[string]*ModelSetting `json:"model_settings,omitempty"`
	// LastAutoRefresh 记录上次自动刷新余额的时间，写入 state.json，重启后不会重新计时。
	LastAutoRefresh time.Time `json:"last_auto_refresh,omitempty"`
}

// ModelSetting 是单个 Sail 模型的网关设置。
//   - Efforts：对外暴露的思考强度变体，例如 ["low","high"] 会在 /v1/models 里生成 model-low、model-high。
//   - DefaultEffort：客户端使用原模型名且未自带思考参数时注入的强度，留空表示不注入。
//   - Window：该模型的完成窗口，留空表示跟随全局配置。
type ModelSetting struct {
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
	Window        string   `json:"window,omitempty"`
	// 以下为「探测」结果（通过免费的 /v1/messages/count_tokens 获得），仅用于提示和窗口兜底。
	SupportedEfforts []string  `json:"supported_efforts,omitempty"`
	SupportedWindows []string  `json:"supported_windows,omitempty"`
	ProbedAt         time.Time `json:"probed_at,omitempty"`
}

type Key struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Tier            string    `json:"tier"`
	APIKey          string    `json:"api_key"`
	MonthlySpentUSD float64   `json:"monthly_spent_usd"`
	Paused          bool      `json:"paused"`
	LastBalance     float64   `json:"last_balance"`
	LastUsedTotal   float64   `json:"last_used_total"`
	LastPollAt      time.Time `json:"last_poll_at"`
	LastProxyAt     time.Time `json:"last_proxy_at"`
	LastErr         string    `json:"last_error"`
	RequestCount    int64     `json:"request_count"`
	ProxyReqCount   int64     `json:"proxy_request_count"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Scrapped        bool      `json:"scrapped"`
	ScrappedErr     string    `json:"scrapped_error,omitempty"`
	ScrappedAt      time.Time `json:"scrapped_at,omitempty"`
	ConsecFails     int       `json:"consec_fails"`
	// AutoCooldown 记录自动冷却原因：balance / spend / status；手动暂停为空。
	// 刷新余额时只会自动解冻 AutoCooldown 非空的 key。
	AutoCooldown string `json:"auto_cooldown,omitempty"`
}

type PublicKeyView struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Tier            string    `json:"tier"`
	APIKey          string    `json:"api_key,omitempty"`
	MonthlySpentUSD float64   `json:"monthly_spent_usd"`
	Paused          bool      `json:"paused"`
	LastBalance     float64   `json:"last_balance"`
	LastUsedTotal   float64   `json:"last_used_total"`
	LastPollAt      time.Time `json:"last_poll_at"`
	LastProxyAt     time.Time `json:"last_proxy_at"`
	LastErr         string    `json:"last_error"`
	RequestCount    int64     `json:"request_count"`
	ProxyReqCount   int64     `json:"proxy_request_count"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Scrapped        bool      `json:"scrapped"`
	ScrappedErr     string    `json:"scrapped_error,omitempty"`
	ScrappedAt      time.Time `json:"scrapped_at,omitempty"`
	ConsecFails     int       `json:"consec_fails"`
	AutoCooldown    string    `json:"auto_cooldown,omitempty"`
}

type CreateKeyReq struct {
	Name   string `json:"name"`
	Tier   string `json:"tier"`
	APIKey string `json:"api_key"`
}

type UpdateKeyReq struct {
	Name      *string `json:"name"`
	Tier      *string `json:"tier"`
	APIKey    *string `json:"api_key"`
	Paused    *bool   `json:"paused"`
	ResetCost *bool   `json:"reset_cost"`
	Scrapped  *bool   `json:"scrapped"`
}

// sailUsageSummary 对应 GET /v2/usage/summary 的部分字段。金额单位是美分。
type sailUsageSummary struct {
	Balance            float64 `json:"balance"`
	BalanceUnavailable bool    `json:"balance_unavailable"`
	PeriodSpend        float64 `json:"period_spend"`
}

type Config struct {
	ListenAddr                 string
	GatewayBaseURL             string
	StateFilePath              string
	RuntimeConfigPath          string
	WebAuthToken               string
	ApiAuthToken               string
	ApiAuthTokens              []string
	CompletionWindow           string
	StripUnsupported           bool
	TestModel                  string
	AutoRefreshHours           int
	CooldownStatusCodes        string
	ExhaustStatusCodes         string
	ProxyScrapStatusCodes      string
	ProxyScrapFailThreshold    int
	RefreshScrapStatusCodes    string
	RefreshCooldownStatusCodes string
}

type proxyCandidate struct {
	ID        string
	APIKey    string
	CreatedAt time.Time
}

type ProxyLog struct {
	Time          time.Time `json:"time"`
	Model         string    `json:"model"`
	KeyName       string    `json:"key_name"`
	KeyID         string    `json:"key_id"`
	StatusCode    int       `json:"status_code"`
	ElapsedMs     int64     `json:"elapsed_ms"`
	Success       bool      `json:"success"`
	Retried       bool      `json:"retried"`
	Error         string    `json:"error,omitempty"`
	Path          string    `json:"path"`
	Method        string    `json:"method,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
	Interface     string    `json:"interface,omitempty"`
	UpstreamModel string    `json:"upstream_model,omitempty"`
	Effort        string    `json:"effort,omitempty"`
	Window        string    `json:"window,omitempty"`
	Stream        bool      `json:"stream,omitempty"`
	InputTokens   int       `json:"input_tokens,omitempty"`
	OutputTokens  int       `json:"output_tokens,omitempty"`
	TotalTokens   int       `json:"total_tokens,omitempty"`
	UsageSource   string    `json:"usage_source,omitempty"`
}

type TokenUsage struct {
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	TotalTokens  int    `json:"total_tokens,omitempty"`
	Source       string `json:"usage_source,omitempty"`
}

type ProxyLogRing struct {
	mu   sync.Mutex
	logs []ProxyLog
	cap  int
}

func newProxyLogRing(cap int) *ProxyLogRing {
	return &ProxyLogRing{logs: make([]ProxyLog, 0, cap), cap: cap}
}

func (r *ProxyLogRing) Add(entry ProxyLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.logs) >= r.cap {
		r.logs = r.logs[1:]
	}
	r.logs = append(r.logs, entry)
}

func (r *ProxyLogRing) Recent(n int) []ProxyLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || len(r.logs) == 0 {
		return nil
	}
	if n > len(r.logs) {
		n = len(r.logs)
	}
	result := make([]ProxyLog, n)
	for i := 0; i < n; i++ {
		result[i] = r.logs[len(r.logs)-1-i]
	}
	return result
}

type BatchTestResult struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Restored bool   `json:"restored"`
	Status   int    `json:"status_code"`
	Error    string `json:"error,omitempty"`
	Content  string `json:"content,omitempty"`
}
