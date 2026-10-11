package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 上游模型目录（缓存 Sail 的 GET /v1/models）
// ---------------------------------------------------------------------------

type upstreamModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelCatalog struct {
	mu        sync.Mutex
	models    []upstreamModel
	fetchedAt time.Time
	ttl       time.Duration
}

func newModelCatalog() *modelCatalog {
	return &modelCatalog{ttl: 10 * time.Minute}
}

// Get 返回缓存的上游模型列表；过期时用任意可用 key 重新拉取，拉取失败则退回旧缓存。
func (c *modelCatalog) Get(ctx context.Context, cfg Config, state *AppState, force bool) ([]upstreamModel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && len(c.models) > 0 && time.Since(c.fetchedAt) < c.ttl {
		return append([]upstreamModel(nil), c.models...), nil
	}
	cands := state.nextProxyCandidates()
	if len(cands) == 0 {
		if len(c.models) > 0 {
			return append([]upstreamModel(nil), c.models...), nil
		}
		return nil, errors.New("no active key available")
	}
	var lastErr error
	for i, cand := range cands {
		if i >= 3 {
			break
		}
		models, err := fetchUpstreamModels(ctx, cfg.GatewayBaseURL, cand.APIKey)
		if err != nil {
			lastErr = err
			continue
		}
		c.models = models
		c.fetchedAt = time.Now()
		return append([]upstreamModel(nil), models...), nil
	}
	if len(c.models) > 0 {
		return append([]upstreamModel(nil), c.models...), nil
	}
	return nil, lastErr
}

func fetchUpstreamModels(ctx context.Context, baseURL, apiKey string) ([]upstreamModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models status=%d body=%s", resp.StatusCode, truncate(string(body), 300))
	}
	var mr struct {
		Data []upstreamModel `json:"data"`
	}
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, err
	}
	return mr.Data, nil
}

// handleModelsList 实现客户端的 GET /v1/models：
// 每个上游模型后面紧跟它在后台勾选的思考强度变体（model-low、model-high ...）。
func handleModelsList(w http.ResponseWriter, r *http.Request, cfg Config, state *AppState, catalog *modelCatalog) {
	models, err := catalog.Get(r.Context(), cfg, state, false)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "fetch models failed: " + err.Error(), "type": "upstream_error"}})
		return
	}
	settings := state.modelSettingsSnapshot()
	data := make([]upstreamModel, 0, len(models)*2)
	for _, m := range models {
		data = append(data, m)
		_, ms := lookupModelSetting(settings, m.ID)
		if ms == nil {
			continue
		}
		for _, e := range ms.Efforts {
			v := m
			v.ID = variantModelName(m.ID, e)
			data = append(data, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---------------------------------------------------------------------------
// 探测：通过免费的 /v1/messages/count_tokens 判断模型支持哪些强度和窗口
// ---------------------------------------------------------------------------

type probeResult struct {
	Model            string   `json:"model"`
	SupportedEfforts []string `json:"supported_efforts"`
	SupportedWindows []string `json:"supported_windows"`
	Error            string   `json:"error,omitempty"`
}

func probeModel(ctx context.Context, baseURL, apiKey, model string) probeResult {
	res := probeResult{Model: model, SupportedEfforts: []string{}, SupportedWindows: []string{}}
	client := &http.Client{Timeout: 20 * time.Second}
	call := func(extra map[string]any) (int, string) {
		payload := map[string]any{
			"model":    model,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		for k, v := range extra {
			payload[k] = v
		}
		b, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/messages/count_tokens", bytes.NewReader(b))
		if err != nil {
			return 0, err.Error()
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, strings.TrimSpace(string(body))
	}

	if code, body := call(nil); code != http.StatusOK {
		res.Error = fmt.Sprintf("status=%d %s", code, truncate(body, 200))
		return res
	}
	type item struct {
		kind, value string
		extra       map[string]any
	}
	var items []item
	for _, e := range sailEfforts {
		items = append(items, item{"effort", e, map[string]any{"output_config": map[string]any{"effort": e}}})
	}
	for _, win := range completionWindows {
		items = append(items, item{"window", win, map[string]any{"metadata": map[string]any{"completion_window": win}}})
	}
	ok := make([]bool, len(items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, it item) {
			defer wg.Done()
			defer func() { <-sem }()
			code, _ := call(it.extra)
			ok[i] = code == http.StatusOK
		}(i, it)
	}
	wg.Wait()
	for i, it := range items {
		if !ok[i] {
			continue
		}
		if it.kind == "effort" {
			res.SupportedEfforts = append(res.SupportedEfforts, it.value)
		} else {
			res.SupportedWindows = append(res.SupportedWindows, it.value)
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// 后台 API：/api/models
// ---------------------------------------------------------------------------

type modelSettingView struct {
	ID               string   `json:"id"`
	OwnedBy          string   `json:"owned_by,omitempty"`
	Upstream         bool     `json:"upstream"`
	Efforts          []string `json:"efforts"`
	DefaultEffort    string   `json:"default_effort"`
	Window           string   `json:"window"`
	SupportedEfforts []string `json:"supported_efforts"`
	SupportedWindows []string `json:"supported_windows"`
	ProbedAt         string   `json:"probed_at,omitempty"`
	Variants         []string `json:"variants"`
}

func buildModelViews(models []upstreamModel, settings map[string]*ModelSetting) []modelSettingView {
	views := make([]modelSettingView, 0, len(models))
	seen := map[string]bool{}
	add := func(id, owner string, upstream bool) {
		_, ms := lookupModelSetting(settings, id)
		v := modelSettingView{ID: id, OwnedBy: owner, Upstream: upstream, Efforts: []string{}, SupportedEfforts: []string{}, SupportedWindows: []string{}, Variants: []string{}}
		if ms != nil {
			v.Efforts = append(v.Efforts, ms.Efforts...)
			v.DefaultEffort = ms.DefaultEffort
			v.Window = ms.Window
			v.SupportedEfforts = append(v.SupportedEfforts, ms.SupportedEfforts...)
			v.SupportedWindows = append(v.SupportedWindows, ms.SupportedWindows...)
			if !ms.ProbedAt.IsZero() {
				v.ProbedAt = ms.ProbedAt.Format(time.RFC3339)
			}
			for _, e := range ms.Efforts {
				v.Variants = append(v.Variants, variantModelName(id, e))
			}
		}
		views = append(views, v)
		seen[strings.ToLower(id)] = true
	}
	for _, m := range models {
		add(m.ID, m.OwnedBy, true)
	}
	// 上游已下线、但后台还留有设置的模型也列出来，方便清理。
	extra := make([]string, 0)
	for name := range settings {
		if !seen[strings.ToLower(name)] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		add(name, "", false)
	}
	return views
}

func handleModelSettings(rtCfg *RuntimeConfig, state *AppState, catalog *modelCatalog) http.HandlerFunc {
	type saveReq struct {
		Model         string   `json:"model"`
		Efforts       []string `json:"efforts"`
		DefaultEffort string   `json:"default_effort"`
		Window        string   `json:"window"`
	}
	type probeReq struct {
		Models []string `json:"models"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		cfg := rtCfg.Get()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/models":
			models, err := catalog.Get(r.Context(), cfg, state, r.URL.Query().Get("refresh") == "1")
			resp := map[string]any{
				"efforts":           sailEfforts,
				"windows":           completionWindows,
				"global_window":     cfg.CompletionWindow,
				"models":            buildModelViews(models, state.modelSettingsSnapshot()),
				"catalog_available": err == nil,
			}
			if err != nil {
				resp["error"] = err.Error()
			}
			writeJSON(w, http.StatusOK, resp)

		case r.Method == http.MethodPost && r.URL.Path == "/api/models":
			var req saveReq
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
				return
			}
			model := strings.TrimSpace(req.Model)
			if model == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model required"})
				return
			}
			if d := strings.TrimSpace(req.DefaultEffort); d != "" && !isValidEffort(d) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid default_effort: " + d})
				return
			}
			if win := strings.TrimSpace(req.Window); win != "" && !isValidCompletionWindow(win) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid window: " + win})
				return
			}
			current := state.modelSettingsSnapshot()
			_, old := lookupModelSetting(current, model)
			next := &ModelSetting{Efforts: req.Efforts, DefaultEffort: req.DefaultEffort, Window: req.Window}
			if old != nil {
				next.SupportedEfforts = old.SupportedEfforts
				next.SupportedWindows = old.SupportedWindows
				next.ProbedAt = old.ProbedAt
			}
			if err := state.setModelSetting(model, next); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			models, _ := catalog.Get(r.Context(), cfg, state, false)
			writeJSON(w, http.StatusOK, map[string]any{"models": buildModelViews(models, state.modelSettingsSnapshot())})

		case r.Method == http.MethodPost && r.URL.Path == "/api/models/probe":
			var req probeReq
			if r.ContentLength > 0 {
				_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
			}
			targets := cleanTokens(req.Models)
			if len(targets) == 0 {
				models, err := catalog.Get(r.Context(), cfg, state, true)
				if err != nil {
					writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
					return
				}
				for _, m := range models {
					targets = append(targets, m.ID)
				}
			}
			cands := state.nextProxyCandidates()
			if len(cands) == 0 {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no active key available"})
				return
			}
			apiKey := cands[0].APIKey
			results := make([]probeResult, len(targets))
			var wg sync.WaitGroup
			sem := make(chan struct{}, 3)
			for i, m := range targets {
				wg.Add(1)
				sem <- struct{}{}
				go func(i int, m string) {
					defer wg.Done()
					defer func() { <-sem }()
					results[i] = probeModel(r.Context(), cfg.GatewayBaseURL, apiKey, m)
				}(i, m)
			}
			wg.Wait()
			now := time.Now().UTC()
			current := state.modelSettingsSnapshot()
			for _, res := range results {
				if res.Error != "" {
					continue
				}
				name, old := lookupModelSetting(current, res.Model)
				if name == "" {
					name = res.Model
				}
				next := &ModelSetting{}
				if old != nil {
					*next = *old
				}
				next.SupportedEfforts = res.SupportedEfforts
				next.SupportedWindows = res.SupportedWindows
				next.ProbedAt = now
				if err := state.setModelSetting(name, next); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
					return
				}
			}
			models, _ := catalog.Get(r.Context(), cfg, state, false)
			writeJSON(w, http.StatusOK, map[string]any{
				"results": results,
				"models":  buildModelViews(models, state.modelSettingsSnapshot()),
			})

		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
	}
}
