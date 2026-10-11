package app

import (
	"encoding/json"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 完成窗口（completion window）
// ---------------------------------------------------------------------------

var completionWindows = []string{"asap", "balanced", "flex"}

func isValidCompletionWindow(w string) bool {
	w = strings.ToLower(strings.TrimSpace(w))
	for _, v := range completionWindows {
		if v == w {
			return true
		}
	}
	return false
}

// normalizeCompletionWindow 规范化全局窗口；非法值回落到默认 balanced。
func normalizeCompletionWindow(w string) string {
	w = strings.ToLower(strings.TrimSpace(w))
	if isValidCompletionWindow(w) {
		return w
	}
	return defaultCompletionWindow
}

// normalizeModelWindow 规范化单模型窗口；空 / 非法值表示跟随全局。
func normalizeModelWindow(w string) string {
	w = strings.ToLower(strings.TrimSpace(w))
	if isValidCompletionWindow(w) {
		return w
	}
	return ""
}

// effectiveWindow 计算本次请求实际使用的窗口。
//   - Sail 规定 flex 只能用于 background=true 的 /v1/responses（或 Batch），其他请求选了 flex 自动降级到 balanced。
//   - 如果探测过该模型支持的窗口（supported 非空），且选中的窗口不可用，则换成最接近的可用窗口。
//     例如只支持 flex 的模型在同步请求下没有可用窗口，此时保持原选择，让 Sail 返回明确的错误。
func effectiveWindow(window, path string, req map[string]any, supported []string) string {
	window = normalizeCompletionWindow(window)
	bg, _ := req["background"].(bool)
	flexAllowed := isResponsesPath(path) && bg
	if window == "flex" && !flexAllowed {
		window = "balanced"
	}
	if len(supported) == 0 {
		return window
	}
	allowed := func(w string) bool {
		if w == "flex" && !flexAllowed {
			return false
		}
		for _, s := range supported {
			if s == w {
				return true
			}
		}
		return false
	}
	if allowed(window) {
		return window
	}
	// 按「越便宜越优先」的顺序回退：flex → balanced → asap；若原本选 asap 则优先更快的。
	order := []string{"flex", "balanced", "asap"}
	if window == "asap" {
		order = []string{"asap", "balanced", "flex"}
	}
	for _, w := range order {
		if allowed(w) {
			return w
		}
	}
	return window
}

// clientCompletionWindow 返回客户端在 metadata.completion_window 中自己指定的窗口。
// Sail 中 body 的 metadata 优先于请求头，所以客户端指定时网关不再干预。
func clientCompletionWindow(req map[string]any) string {
	md, _ := req["metadata"].(map[string]any)
	if md == nil {
		return ""
	}
	w, _ := md["completion_window"].(string)
	return strings.ToLower(strings.TrimSpace(w))
}

// ---------------------------------------------------------------------------
// 思考强度（reasoning effort）
// ---------------------------------------------------------------------------

// sailEfforts 是 Sail 支持的全部强度，顺序即前端展示顺序。
// 具体某个模型支持哪些，以「探测」结果为准。
var sailEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func isValidEffort(e string) bool {
	e = strings.ToLower(strings.TrimSpace(e))
	for _, v := range sailEfforts {
		if v == e {
			return true
		}
	}
	return false
}

func effortRank(e string) int {
	for i, v := range sailEfforts {
		if v == e {
			return i
		}
	}
	return len(sailEfforts)
}

func normalizeEffortList(list []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, e := range list {
		e = strings.ToLower(strings.TrimSpace(e))
		if !isValidEffort(e) || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return effortRank(out[i]) < effortRank(out[j]) })
	return out
}

func normalizeModelSetting(ms *ModelSetting) *ModelSetting {
	if ms == nil {
		return nil
	}
	out := &ModelSetting{
		Efforts: normalizeEffortList(ms.Efforts),
		Window:  normalizeModelWindow(ms.Window),
	}
	if d := strings.ToLower(strings.TrimSpace(ms.DefaultEffort)); isValidEffort(d) {
		out.DefaultEffort = d
	}
	out.SupportedEfforts = normalizeEffortList(ms.SupportedEfforts)
	for _, w := range completionWindows {
		for _, sw := range ms.SupportedWindows {
			if strings.EqualFold(strings.TrimSpace(sw), w) {
				out.SupportedWindows = append(out.SupportedWindows, w)
				break
			}
		}
	}
	out.ProbedAt = ms.ProbedAt
	if len(out.Efforts) == 0 && out.DefaultEffort == "" && out.Window == "" && out.ProbedAt.IsZero() {
		return nil
	}
	return out
}

func variantModelName(base, effort string) string {
	return base + "-" + effort
}

// requestRoute 是一次代理请求解析出的路由信息。
type requestRoute struct {
	ClientModel   string // 客户端传入的模型名
	UpstreamModel string // 实际发给 Sail 的模型名（去掉强度后缀）
	Effort        string // 网关注入的强度；客户端自己指定时为空
	Window        string // 网关通过请求头注入的窗口；客户端自己指定时为空
	setting       *ModelSetting
}

// lookupModelSetting 不区分大小写地查找模型设置，返回规范的模型名。
func lookupModelSetting(settings map[string]*ModelSetting, model string) (string, *ModelSetting) {
	if ms, ok := settings[model]; ok {
		return model, ms
	}
	for name, ms := range settings {
		if strings.EqualFold(name, model) {
			return name, ms
		}
	}
	return "", nil
}

// resolveModel 把客户端模型名解析成「上游模型 + 强度」。
// 只有在后台为该模型勾选过的强度才会被识别为变体，避免误伤本身带 -high 之类后缀的模型。
func resolveModel(settings map[string]*ModelSetting, model string) (upstream, effort string, ms *ModelSetting) {
	if name, s := lookupModelSetting(settings, model); s != nil {
		return name, "", s
	}
	idx := strings.LastIndex(model, "-")
	if idx <= 0 {
		return model, "", nil
	}
	base, suffix := model[:idx], strings.ToLower(model[idx+1:])
	name, s := lookupModelSetting(settings, base)
	if s == nil {
		return model, "", nil
	}
	for _, e := range s.Efforts {
		if e == suffix {
			return name, e, s
		}
	}
	return model, "", nil
}

func isChatPath(path string) bool      { return strings.Contains(path, "/chat/completions") }
func isResponsesPath(path string) bool { return strings.Contains(path, "/responses") }
func isMessagesPath(path string) bool {
	return strings.Contains(path, "/messages") && !strings.Contains(path, "/count_tokens")
}

// clientSetsEffort 判断客户端是否已经自己带了思考参数。
func clientSetsEffort(req map[string]any, path string) bool {
	switch {
	case isChatPath(path):
		if _, ok := req["reasoning"]; ok {
			return true
		}
		_, ok := req["reasoning_effort"]
		return ok
	case isResponsesPath(path):
		r, _ := req["reasoning"].(map[string]any)
		_, ok := r["effort"]
		return ok
	case isMessagesPath(path):
		if _, ok := req["thinking"]; ok {
			return true
		}
		oc, _ := req["output_config"].(map[string]any)
		_, ok := oc["effort"]
		return ok
	}
	return false
}

// applyEffort 按接口类型写入 Sail 认可的强度字段，并删除客户端自带的其它思考参数，
// 保证模型名后缀（或默认强度）是唯一生效的强度来源。
//   - Chat Completions：reasoning_effort（删除客户端的 reasoning 对象）
//   - Responses：reasoning.effort（保留客户端的 reasoning.summary 等其它字段）
//   - Messages：output_config.effort（删除客户端的 thinking；实测 Sail 中 output_config.effort 优先于 thinking）
func applyEffort(req map[string]any, path, effort string) bool {
	switch {
	case isChatPath(path):
		delete(req, "reasoning")
		req["reasoning_effort"] = effort
	case isResponsesPath(path):
		delete(req, "reasoning_effort")
		r, _ := req["reasoning"].(map[string]any)
		if r == nil {
			r = map[string]any{}
		}
		r["effort"] = effort
		req["reasoning"] = r
	case isMessagesPath(path):
		delete(req, "thinking")
		oc, _ := req["output_config"].(map[string]any)
		if oc == nil {
			oc = map[string]any{}
		}
		oc["effort"] = effort
		req["output_config"] = oc
	default:
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 请求改写主入口
// ---------------------------------------------------------------------------

// rewriteSailRequest 完成模型变体解析、强度注入、窗口计算和不兼容参数清理。
// 返回改写后的 body 和路由信息；body 不是 JSON 时原样返回。
func rewriteSailRequest(body []byte, path string, cfg Config, settings map[string]*ModelSetting) ([]byte, requestRoute) {
	var route requestRoute
	var req map[string]any
	if len(body) == 0 || json.Unmarshal(body, &req) != nil || req == nil {
		return body, route
	}
	changed := false

	model, _ := req["model"].(string)
	route.ClientModel = model
	route.UpstreamModel = model
	if model != "" {
		upstream, effort, ms := resolveModel(settings, model)
		route.setting = ms
		if upstream != model {
			req["model"] = upstream
			route.UpstreamModel = upstream
			changed = true
		}
		// 优先级：模型名后缀 > 客户端自带的思考参数 > 后台默认强度
		switch {
		case effort != "":
			if applyEffort(req, path, effort) {
				route.Effort = effort
				changed = true
			}
		case ms != nil && ms.DefaultEffort != "" && !clientSetsEffort(req, path):
			if applyEffort(req, path, ms.DefaultEffort) {
				route.Effort = ms.DefaultEffort
				changed = true
			}
		}
	}

	if isChatPath(path) || isResponsesPath(path) || isMessagesPath(path) {
		if clientCompletionWindow(req) == "" {
			window := cfg.CompletionWindow
			if route.setting != nil && route.setting.Window != "" {
				window = route.setting.Window
			}
			var supported []string
			if route.setting != nil {
				supported = route.setting.SupportedWindows
			}
			route.Window = effectiveWindow(window, path, req, supported)
		}
		if cfg.StripUnsupported && stripUnsupportedParams(req, path) {
			changed = true
		}
	}

	if !changed {
		return body, route
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, route
	}
	return out, route
}

// stripUnsupportedParams 删除 Sail 明确会拒绝（HTTP 400）的参数，让常见客户端开箱即用。
// 依据 https://docs.sailresearch.com/support 的 "Not yet supported" 列表。
func stripUnsupportedParams(req map[string]any, path string) bool {
	changed := false
	del := func(keys ...string) {
		for _, k := range keys {
			if _, ok := req[k]; ok {
				delete(req, k)
				changed = true
			}
		}
	}
	dropServiceTier := func() {
		if v, ok := req["service_tier"]; ok {
			if s, _ := v.(string); s != "auto" {
				delete(req, "service_tier")
				changed = true
			}
		}
	}
	switch {
	case isChatPath(path):
		del("frequency_penalty", "presence_penalty", "logit_bias", "stop", "seed",
			"top_logprobs", "logprobs", "verbosity", "prediction", "web_search_options",
			"audio", "modalities")
		if n, ok := req["n"].(float64); ok && n == 1 {
			del("n")
		}
		dropServiceTier()
	case isResponsesPath(path):
		if t, ok := req["truncation"].(string); ok && t != "disabled" {
			del("truncation")
		}
		dropServiceTier()
	case isMessagesPath(path):
		del("stop_sequences", "top_k")
		if t, ok := req["temperature"].(float64); ok && t > 1 {
			req["temperature"] = 1.0
			changed = true
		}
	}
	return changed
}

// ---------------------------------------------------------------------------
// AppState：模型设置
// ---------------------------------------------------------------------------

func (s *AppState) normalizeModelSettings() {
	out := map[string]*ModelSetting{}
	for name, ms := range s.ModelSettings {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if n := normalizeModelSetting(ms); n != nil {
			out[name] = n
		}
	}
	s.ModelSettings = out
}

// modelSettingsSnapshot 返回模型设置的深拷贝，可在锁外安全使用。
func (s *AppState) modelSettingsSnapshot() map[string]*ModelSetting {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*ModelSetting, len(s.ModelSettings))
	for name, ms := range s.ModelSettings {
		cp := *ms
		cp.Efforts = append([]string(nil), ms.Efforts...)
		cp.SupportedEfforts = append([]string(nil), ms.SupportedEfforts...)
		cp.SupportedWindows = append([]string(nil), ms.SupportedWindows...)
		out[name] = &cp
	}
	return out
}

func (s *AppState) setModelSetting(model string, ms *ModelSetting) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ModelSettings == nil {
		s.ModelSettings = map[string]*ModelSetting{}
	}
	for name := range s.ModelSettings {
		if strings.EqualFold(name, model) {
			delete(s.ModelSettings, name)
		}
	}
	if n := normalizeModelSetting(ms); n != nil {
		s.ModelSettings[model] = n
	}
	return s.save()
}

func requestWantsStream(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return req.Stream
}
