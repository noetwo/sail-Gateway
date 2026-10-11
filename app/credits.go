package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fetchSailBalance 调用 Sail 的 GET /v2/usage/summary?range=period 查询余额和本计费周期消费。
// Sail 返回的金额单位是「美分」，这里换算成美元。
func fetchSailBalance(baseURL, apiKey string) (balanceUSD, periodSpendUSD float64, err error) {
	req, err := http.NewRequest(http.MethodGet, sailAPIRoot(baseURL)+"/v2/usage/summary?range=period", nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, fmt.Errorf("usage status=%d body=%s", resp.StatusCode, truncate(strings.TrimSpace(string(b)), 300))
	}
	var sum sailUsageSummary
	if err := json.Unmarshal(b, &sum); err != nil {
		return 0, 0, err
	}
	if sum.BalanceUnavailable {
		return 0, 0, errors.New("Sail 暂时无法返回余额（balance_unavailable）")
	}
	return sum.Balance / 100, sum.PeriodSpend / 100, nil
}

// sailAPIRoot 把 https://api.sailresearch.com/v1 转成 https://api.sailresearch.com。
func sailAPIRoot(baseURL string) string {
	base := strings.TrimRight(baseURL, "/")
	return strings.TrimSuffix(base, "/v1")
}

// pollOne 从 Sail 查询单个 key 的真实余额。
//   - 余额 <= 0：额度用完，直接报废（Sail 免费账号是一次性额度，不会自动补充）
//   - 报废原因是「额度用完」的 key，若再次查到余额 > 0（例如账号充值、绑卡），自动恢复
//   - 返回 401 等无效状态码：按配置报废
func pollOne(cfg Config, state *AppState, id string) error {
	state.mu.RLock()
	k, ok := state.Keys[id]
	if !ok {
		state.mu.RUnlock()
		return errors.New("key not found")
	}
	apiKey := k.APIKey
	state.mu.RUnlock()

	balance, spend, err := fetchSailBalance(cfg.GatewayBaseURL, apiKey)
	now := time.Now().UTC()

	state.mu.Lock()
	defer state.mu.Unlock()
	k2, ok := state.Keys[id]
	if !ok {
		return errors.New("key not found")
	}
	k2.LastPollAt = now
	k2.UpdatedAt = now
	clearSticky := func() {
		if state.StickyMode && state.StickyKeyID == id {
			state.StickyKeyID = ""
		}
	}
	if err != nil {
		k2.LastErr = err.Error()
		statusCode := statusCodeFromError(err.Error())
		if retryStatusEnabled(statusCode, cfg.RefreshScrapStatusCodes) {
			k2.Scrapped = true
			k2.ScrappedErr = err.Error()
			k2.ScrappedAt = now
			k2.Paused = true
			clearSticky()
			state.renumberKeys()
		} else if retryStatusEnabled(statusCode, cfg.RefreshCooldownStatusCodes) {
			k2.Paused = true
			k2.AutoCooldown = "status"
			clearSticky()
		}
		_ = state.save()
		return err
	}
	k2.LastErr = ""
	k2.RequestCount++
	k2.LastBalance = balance
	k2.MonthlySpentUSD = spend
	k2.LastUsedTotal = spend

	switch {
	case balance <= 0 && !k2.Scrapped:
		k2.Scrapped = true
		k2.ScrappedErr = fmt.Sprintf("额度已用完（余额 $%.4f）", balance)
		k2.ScrappedAt = now
		k2.Paused = true
		k2.AutoCooldown = "exhausted"
		clearSticky()
		state.renumberKeys()
	case balance > 0 && k2.Scrapped && k2.AutoCooldown == "exhausted":
		k2.Scrapped = false
		k2.ScrappedErr = ""
		k2.ScrappedAt = time.Time{}
		k2.Paused = false
		k2.AutoCooldown = ""
		k2.ConsecFails = 0
		state.renumberKeys()
	}
	return state.save()
}

// balanceRefresher 在 key 被成功使用后延迟查询一次真实余额。
// Sail 的余额更新有几秒延迟，所以等 15 秒；同一个 key 在等待期间的多次请求只查一次。
type balanceRefresher struct {
	mu      sync.Mutex
	pending map[string]bool
	delay   time.Duration
}

func newBalanceRefresher(delay time.Duration) *balanceRefresher {
	return &balanceRefresher{pending: map[string]bool{}, delay: delay}
}

func (b *balanceRefresher) Schedule(rtCfg *RuntimeConfig, state *AppState, id string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	if b.pending[id] {
		b.mu.Unlock()
		return
	}
	b.pending[id] = true
	b.mu.Unlock()
	go func() {
		time.Sleep(b.delay)
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		_ = pollOne(rtCfg.Get(), state, id)
	}()
}

func statusCodeFromError(msg string) int {
	idx := strings.Index(msg, "status=")
	if idx < 0 {
		return 0
	}
	rest := msg[idx+len("status="):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}
