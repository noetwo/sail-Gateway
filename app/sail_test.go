package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testCfg() Config {
	return Config{CompletionWindow: "balanced", StripUnsupported: true}
}

func decodeBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid json: %v (%s)", err, b)
	}
	return m
}

func TestResolveModelOnlyMatchesConfiguredEfforts(t *testing.T) {
	settings := map[string]*ModelSetting{
		"zai-org/GLM-5.3": {Efforts: []string{"low", "high"}},
	}
	if up, e, _ := resolveModel(settings, "zai-org/GLM-5.3-high"); up != "zai-org/GLM-5.3" || e != "high" {
		t.Fatalf("got %q %q", up, e)
	}
	// 没勾选的强度不识别为变体
	if up, e, _ := resolveModel(settings, "zai-org/GLM-5.3-medium"); up != "zai-org/GLM-5.3-medium" || e != "" {
		t.Fatalf("got %q %q", up, e)
	}
	// 本身带 -Flash 后缀的模型不受影响
	if up, e, _ := resolveModel(settings, "zai-org/GLM-5.3-Flash"); up != "zai-org/GLM-5.3-Flash" || e != "" {
		t.Fatalf("got %q %q", up, e)
	}
}

func TestRewriteInjectsEffortPerInterface(t *testing.T) {
	settings := map[string]*ModelSetting{"m/x": {Efforts: []string{"high"}}}
	cases := []struct {
		path  string
		check func(map[string]any) bool
	}{
		{"/v1/chat/completions", func(m map[string]any) bool { return m["reasoning_effort"] == "high" }},
		{"/v1/responses", func(m map[string]any) bool {
			r, _ := m["reasoning"].(map[string]any)
			return r["effort"] == "high"
		}},
		{"/v1/messages", func(m map[string]any) bool {
			oc, _ := m["output_config"].(map[string]any)
			return oc["effort"] == "high"
		}},
	}
	for _, c := range cases {
		// 客户端自带的各种思考参数都应被模型后缀覆盖
		in := `{"model":"m/x-high","reasoning_effort":"low","reasoning":{"effort":"low","summary":"auto"},"thinking":{"type":"disabled"},"output_config":{"effort":"none"}}`
		out, route := rewriteSailRequest([]byte(in), c.path, testCfg(), settings)
		m := decodeBody(t, out)
		if m["model"] != "m/x" || !c.check(m) {
			t.Fatalf("%s: unexpected body %s", c.path, out)
		}
		if _, ok := m["thinking"]; ok && isMessagesPath(c.path) {
			t.Fatalf("%s: client thinking not removed: %s", c.path, out)
		}
		if _, ok := m["reasoning"]; ok && isChatPath(c.path) {
			t.Fatalf("%s: client reasoning not removed: %s", c.path, out)
		}
		if route.Effort != "high" || route.UpstreamModel != "m/x" || route.ClientModel != "m/x-high" {
			t.Fatalf("%s: route %+v", c.path, route)
		}
	}
}

func TestAutoRefreshDue(t *testing.T) {
	state := testState(t, map[string]*Key{})
	if state.autoRefreshDue(time.Hour) {
		t.Fatal("first run should only record time")
	}
	if state.autoRefreshDue(time.Hour) {
		t.Fatal("should not be due yet")
	}
	state.LastAutoRefresh = time.Now().Add(-2 * time.Hour)
	if !state.autoRefreshDue(time.Hour) {
		t.Fatal("should be due")
	}
}

func TestRewriteDefaultEffortDoesNotOverrideClient(t *testing.T) {
	settings := map[string]*ModelSetting{"m/x": {DefaultEffort: "low"}}
	out, route := rewriteSailRequest([]byte(`{"model":"m/x"}`), "/v1/chat/completions", testCfg(), settings)
	if decodeBody(t, out)["reasoning_effort"] != "low" || route.Effort != "low" {
		t.Fatalf("default effort not injected: %s", out)
	}
	out, route = rewriteSailRequest([]byte(`{"model":"m/x","reasoning_effort":"max"}`), "/v1/chat/completions", testCfg(), settings)
	if decodeBody(t, out)["reasoning_effort"] != "max" || route.Effort != "" {
		t.Fatalf("client effort overridden: %s", out)
	}
}

func TestRewriteWindow(t *testing.T) {
	cfg := testCfg()
	_, route := rewriteSailRequest([]byte(`{"model":"a"}`), "/v1/chat/completions", cfg, nil)
	if route.Window != "balanced" {
		t.Fatalf("window = %q", route.Window)
	}
	// 客户端在 metadata 中自己指定时不注入
	_, route = rewriteSailRequest([]byte(`{"model":"a","metadata":{"completion_window":"asap"}}`), "/v1/chat/completions", cfg, nil)
	if route.Window != "" {
		t.Fatalf("window = %q", route.Window)
	}
	// flex 只对后台 responses 生效，其余降级为 balanced
	cfg.CompletionWindow = "flex"
	_, route = rewriteSailRequest([]byte(`{"model":"a"}`), "/v1/chat/completions", cfg, nil)
	if route.Window != "balanced" {
		t.Fatalf("window = %q", route.Window)
	}
	_, route = rewriteSailRequest([]byte(`{"model":"a","background":true}`), "/v1/responses", cfg, nil)
	if route.Window != "flex" {
		t.Fatalf("window = %q", route.Window)
	}
	// 单模型窗口优先于全局；探测到只支持 flex 的模型在后台请求时自动用 flex
	settings := map[string]*ModelSetting{
		"a": {Window: "asap"},
		"q": {SupportedWindows: []string{"flex"}},
	}
	cfg.CompletionWindow = "balanced"
	_, route = rewriteSailRequest([]byte(`{"model":"a"}`), "/v1/chat/completions", cfg, settings)
	if route.Window != "asap" {
		t.Fatalf("window = %q", route.Window)
	}
	_, route = rewriteSailRequest([]byte(`{"model":"q","background":true}`), "/v1/responses", cfg, settings)
	if route.Window != "flex" {
		t.Fatalf("window = %q", route.Window)
	}
	// 非推理接口不注入
	_, route = rewriteSailRequest([]byte(`{"model":"a"}`), "/v1/messages/count_tokens", cfg, nil)
	if route.Window != "" {
		t.Fatalf("window = %q", route.Window)
	}
}

func TestStripUnsupportedParams(t *testing.T) {
	out, _ := rewriteSailRequest([]byte(`{"model":"a","stop":["x"],"seed":1,"n":1,"presence_penalty":0}`), "/v1/chat/completions", testCfg(), nil)
	m := decodeBody(t, out)
	for _, k := range []string{"stop", "seed", "n", "presence_penalty"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%s not stripped: %s", k, out)
		}
	}
	out, _ = rewriteSailRequest([]byte(`{"model":"a","stop_sequences":["x"],"top_k":5,"temperature":1.5}`), "/v1/messages", testCfg(), nil)
	m = decodeBody(t, out)
	if _, ok := m["top_k"]; ok || m["temperature"].(float64) != 1 {
		t.Fatalf("messages not sanitized: %s", out)
	}
	cfg := testCfg()
	cfg.StripUnsupported = false
	out, _ = rewriteSailRequest([]byte(`{"model":"a","seed":1}`), "/v1/chat/completions", cfg, nil)
	if _, ok := decodeBody(t, out)["seed"]; !ok {
		t.Fatalf("seed stripped although disabled")
	}
}

func TestPollOneScrapsExhaustedKeyAndRecovers(t *testing.T) {
	balance := 0.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/usage/summary" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"balance": balance * 100, "period_spend": 100.0})
	}))
	defer srv.Close()
	state := testState(t, map[string]*Key{"01": {ID: "01", Name: "1", APIKey: "sk_a"}})
	cfg := Config{GatewayBaseURL: srv.URL + "/v1"}

	if err := pollOne(cfg, state, "01"); err != nil {
		t.Fatal(err)
	}
	k := state.Keys["01"]
	if !k.Scrapped || k.AutoCooldown != "exhausted" || k.MonthlySpentUSD != 1 {
		t.Fatalf("expected scrapped as exhausted: %+v", k)
	}
	balance = 5
	if err := pollOne(cfg, state, "01"); err != nil {
		t.Fatal(err)
	}
	if k.Scrapped || k.Paused || k.LastBalance != 5 {
		t.Fatalf("expected recover after top-up: %+v", k)
	}
	// 手动报废的 key 不会因为有余额被自动恢复
	k.Scrapped, k.AutoCooldown = true, ""
	if err := pollOne(cfg, state, "01"); err != nil {
		t.Fatal(err)
	}
	if !k.Scrapped {
		t.Fatalf("manual scrap was cleared")
	}
}

func TestProxy402ScrapsKey(t *testing.T) {
	state := testState(t, map[string]*Key{"01": {ID: "01", Name: "1", APIKey: "sk_a"}})
	state.markProxyFailure("01", "no credit", http.StatusPaymentRequired, Config{ExhaustStatusCodes: "402", ProxyScrapStatusCodes: "401"})
	if k := state.Keys["01"]; !k.Scrapped || k.AutoCooldown != "exhausted" {
		t.Fatalf("402 should scrap key: %+v", k)
	}
}

func TestModelsListIncludesVariants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m/a","object":"model"},{"id":"m/b","object":"model"}]}`))
	}))
	defer srv.Close()
	state := testState(t, map[string]*Key{"01": {ID: "01", APIKey: "sk_a"}})
	state.ModelSettings = map[string]*ModelSetting{"m/a": {Efforts: []string{"low", "high"}}}
	cfg := Config{GatewayBaseURL: srv.URL}
	rec := httptest.NewRecorder()
	handleModelsList(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil), cfg, state, newModelCatalog())
	var out struct {
		Data []upstreamModel `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	var ids []string
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	want := "m/a,m/a-low,m/a-high,m/b"
	if strings.Join(ids, ",") != want {
		t.Fatalf("ids = %v, want %s", ids, want)
	}
}

func TestProxyRoutesLookupToCreatingKey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+" "+r.Header.Get("X-Sail-Completion-Window"))
		mu.Unlock()
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"resp_1","status":"queued"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	rt, err := loadRuntimeConfig(Config{
		GatewayBaseURL:    srv.URL + "/v1",
		RuntimeConfigPath: filepath.Join(dir, "config.json"),
		WebAuthToken:      "x",
		CompletionWindow:  "flex",
	})
	if err != nil {
		t.Fatal(err)
	}
	state := testState(t, map[string]*Key{
		"01": {ID: "01", APIKey: "sk_1", CreatedAt: time.Unix(1, 0)},
		"02": {ID: "02", APIKey: "sk_2", CreatedAt: time.Unix(2, 0)},
	})
	h := handleGatewayProxy(rt, state, newProxyLogRing(10), newModelCatalog(), newKeyAffinity(time.Hour, 100), nil)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","background":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d", rec.Code)
	}
	for i := 0; i < 3; i++ {
		rec = httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("lookup status = %d", rec.Code)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("seen = %v", seen)
	}
	creator := strings.Fields(seen[0])[2]
	for _, s := range seen[1:] {
		if strings.Fields(s)[2] != creator {
			t.Fatalf("lookup used a different key: %v", seen)
		}
	}
	if !strings.HasSuffix(seen[0], "flex") {
		t.Fatalf("background responses should use flex window: %v", seen[0])
	}
}

func TestProxyPassesThroughLastUpstreamError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()
	rt, err := loadRuntimeConfig(Config{
		GatewayBaseURL:    srv.URL + "/v1",
		RuntimeConfigPath: filepath.Join(t.TempDir(), "config.json"),
		WebAuthToken:      "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	state := testState(t, map[string]*Key{
		"01": {ID: "01", APIKey: "sk_1"},
		"02": {ID: "02", APIKey: "sk_2"},
	})
	h := handleGatewayProxy(rt, state, newProxyLogRing(10), newModelCatalog(), newKeyAffinity(time.Hour, 100), nil)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "rate limited") || calls != 2 {
		t.Fatalf("code=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	// 429 是限流，不应计入 key 失败
	if state.Keys["01"].ConsecFails != 0 || state.Keys["01"].Paused {
		t.Fatalf("429 should not penalize key: %+v", state.Keys["01"])
	}
}
