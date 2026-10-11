package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func handleGatewayProxy(rtCfg *RuntimeConfig, state *AppState, proxyLogs *ProxyLogRing, catalog *modelCatalog, affinity *keyAffinity, balances *balanceRefresher) http.HandlerFunc {
	client := &http.Client{Timeout: 30 * time.Minute}

	return func(w http.ResponseWriter, r *http.Request) {
		cfg := rtCfg.Get()
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		logicalPath := r.URL.Path
		// GET /v1/models 由网关自己返回：上游模型 + 后台配置的思考强度变体。
		if r.Method == http.MethodGet && (logicalPath == "/v1/models" || logicalPath == "/v1/models/") {
			handleModelsList(w, r, cfg, state, catalog)
			return
		}

		reqStart := time.Now()
		suffix := strings.TrimPrefix(logicalPath, "/v1")
		targetURL := cfg.GatewayBaseURL + suffix
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}

		body, route := rewriteSailRequest(body, logicalPath, cfg, state.modelSettingsSnapshot())
		body = ensureStreamUsage(body, logicalPath)
		reqModel := route.ClientModel
		wantStream := requestWantsStream(body)
		inputEst := estimateInputTokensFromBody(body)
		isAnthropicPath := isMessagesPath(logicalPath)
		isOpenAIPath := isChatPath(logicalPath)
		isResponses := isResponsesPath(logicalPath)

		// 查询类请求（GET /v1/responses/{id} 等）必须用创建任务的那个 key。
		lookupID := ""
		if r.Method == http.MethodGet || r.Method == http.MethodDelete {
			lookupID = affinityLookupID(logicalPath)
		}
		var cands []proxyCandidate
		if lookupID != "" {
			if kid := affinity.Get(lookupID); kid != "" {
				if c, ok := state.candidateByID(kid); ok {
					cands = []proxyCandidate{c}
				}
			}
		}
		if len(cands) == 0 {
			cands = state.nextProxyCandidates()
		}
		if len(cands) == 0 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no active key available"})
			return
		}
		retryCodes, maxAttempts := state.retrySettings()
		if lookupID != "" {
			// 不知道是哪个 key 创建的（例如网关重启过），只能逐个 key 试，404 视为「不是这个 key」。
			retryCodes += ",404"
			maxAttempts = len(cands)
		}
		if maxAttempts > 0 && len(cands) > maxAttempts {
			cands = cands[:maxAttempts]
		}

		getKeyName := func(id string) string {
			state.mu.RLock()
			defer state.mu.RUnlock()
			if k, ok := state.Keys[id]; ok {
				return k.Name
			}
			return id
		}
		interfaceName := proxyInterfaceName(logicalPath)
		upstreamModel := ""
		if route.UpstreamModel != route.ClientModel {
			upstreamModel = route.UpstreamModel
		}
		addProxyLog := func(c proxyCandidate, statusCode int, success bool, retried bool, errMsg string, usage TokenUsage) {
			usage.finish(inputEst, 0)
			proxyLogs.Add(ProxyLog{
				Time:          time.Now(),
				Model:         reqModel,
				UpstreamModel: upstreamModel,
				Effort:        route.Effort,
				Window:        route.Window,
				KeyName:       getKeyName(c.ID),
				KeyID:         c.ID,
				StatusCode:    statusCode,
				ElapsedMs:     time.Since(reqStart).Milliseconds(),
				Success:       success,
				Retried:       retried,
				Error:         errMsg,
				Path:          r.URL.Path,
				Method:        r.Method,
				Endpoint:      logicalPath,
				Interface:     interfaceName,
				Stream:        wantStream,
				InputTokens:   usage.InputTokens,
				OutputTokens:  usage.OutputTokens,
				TotalTokens:   usage.TotalTokens,
				UsageSource:   usage.Source,
			})
		}

		var lastErr error
		var lastStatus int
		var lastBody []byte
		var lastHeader http.Header
		for i, c := range cands {
			if i > 0 && r.Context().Err() != nil {
				return
			}
			outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			copyProxyHeaders(outReq.Header, r.Header)
			outReq.Header.Set("Authorization", "Bearer "+c.APIKey)
			outReq.Header.Set("Accept-Encoding", "identity")
			if route.Window != "" {
				outReq.Header.Set("X-Sail-Completion-Window", route.Window)
			}
			if wantStream {
				outReq.Header.Set("Accept", "text/event-stream")
			}

			resp, err := client.Do(outReq)
			if err != nil {
				if r.Context().Err() != nil {
					addProxyLog(c, 0, false, i > 0, "client_gone: "+err.Error(), estimatedUsage(inputEst, 0))
					return
				}
				state.markProxyFailure(c.ID, err.Error(), 0, cfg)
				lastErr = err
				lastStatus, lastBody, lastHeader = 0, nil, nil
				addProxyLog(c, 0, false, i > 0, err.Error(), estimatedUsage(inputEst, 0))
				continue
			}

			if shouldRetryWithNextKey(resp.StatusCode, retryCodes) {
				preview, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
				_ = resp.Body.Close()
				errMsg := fmt.Sprintf("upstream status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(preview)))
				if !(lookupID != "" && resp.StatusCode == http.StatusNotFound) {
					state.markProxyFailure(c.ID, truncate(errMsg, 500), resp.StatusCode, cfg)
				}
				lastErr = errors.New(errMsg)
				lastStatus, lastBody, lastHeader = resp.StatusCode, preview, resp.Header.Clone()
				addProxyLog(c, resp.StatusCode, false, i > 0, truncate(errMsg, 300), estimatedUsage(inputEst, 0))
				if i < len(cands)-1 {
					// Sail 返回 429/503 时可能带 Retry-After；换 key 前稍等，最多 3 秒，避免请求风暴。
					if d := retryAfterDelay(resp.Header, 3*time.Second); d > 0 {
						select {
						case <-time.After(d):
						case <-r.Context().Done():
							return
						}
					}
					continue
				}
				break
			}

			copyResponseHeaders(w.Header(), resp.Header)
			w.Header().Del("Content-Length")
			if wantStream {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Del("Content-Encoding")
				w.Header().Set("X-Accel-Buffering", "no")
			}

			w.WriteHeader(resp.StatusCode)
			var copyErr error
			usage := estimatedUsage(inputEst, 0)
			respIsSSE := wantStream && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
			respBody := io.Reader(resp.Body)
			// 只缓存前 2MB 响应用于提取 usage、任务 ID 和错误信息，不影响流式转发。
			captureWriter := newCaptureResponseWriter(w, maxCaptureBytes)
			switch {
			case respIsSSE && isAnthropicPath:
				copyErr = processAnthropicSSE(captureWriter, respBody, r.Context(), inputEst, &usage)
			case respIsSSE && isOpenAIPath:
				copyErr = processOpenAISSE(captureWriter, respBody, r.Context(), inputEst, &usage)
			case respIsSSE && isResponses:
				copyErr = processResponsesSSE(captureWriter, respBody, r.Context(), inputEst, &usage)
			default:
				copyErr = streamCopy(captureWriter, respBody)
				usage = extractUsageFromResponse(captureWriter.Bytes(), inputEst)
			}
			_ = resp.Body.Close()
			clientCancelled := r.Context().Err() != nil
			if copyErr != nil {
				if clientCancelled {
					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						state.markProxySuccess(c.ID)
						balances.Schedule(rtCfg, state, c.ID)
					}
					addProxyLog(c, resp.StatusCode, resp.StatusCode >= 200 && resp.StatusCode < 300, i > 0, "client_gone: "+copyErr.Error(), usage)
					return
				}
				state.markProxyFailure(c.ID, copyErr.Error(), 0, cfg)
				addProxyLog(c, resp.StatusCode, false, i > 0, "stream error: "+copyErr.Error(), usage)
				return
			}

			for _, id := range affinityIDsFromResponse(logicalPath, resp.Header, captureWriter.Bytes()) {
				affinity.Put(id, c.ID)
			}
			if lookupID != "" {
				affinity.Put(lookupID, c.ID)
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				state.markProxySuccess(c.ID)
				if usage.TotalTokens > 0 && lookupID == "" {
					balances.Schedule(rtCfg, state, c.ID)
				}
				addProxyLog(c, resp.StatusCode, true, i > 0, "", usage)
			} else {
				// 非重试状态码（例如 400 参数错误）是请求本身的问题，不计入 key 的失败次数。
				errText := strings.TrimSpace(string(captureWriter.Bytes()))
				addProxyLog(c, resp.StatusCode, false, i > 0, truncate(fmt.Sprintf("status %d %s", resp.StatusCode, errText), 300), usage)
			}
			return
		}

		// 所有 key 都失败：把最后一次上游的原始错误透传给客户端，方便客户端按状态码处理。
		if lastStatus > 0 {
			if ct := lastHeader.Get("Content-Type"); ct != "" {
				w.Header().Set("Content-Type", ct)
			} else {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
			}
			if ra := lastHeader.Get("Retry-After"); ra != "" {
				w.Header().Set("Retry-After", ra)
			}
			w.WriteHeader(lastStatus)
			_, _ = w.Write(lastBody)
			return
		}
		if lastErr == nil {
			lastErr = errors.New("all keys failed")
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": lastErr.Error()})
	}
}

func retryAfterDelay(h http.Header, max time.Duration) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	sec, err := strconv.ParseFloat(v, 64)
	if err != nil || sec <= 0 {
		return 0
	}
	d := time.Duration(sec * float64(time.Second))
	if d > max {
		d = max
	}
	return d
}

func shouldRetryWithNextKey(code int, retryCodes string) bool {
	return retryStatusEnabled(code, retryCodes)
}

func proxyInterfaceName(path string) string {
	switch {
	case strings.Contains(path, "/messages/count_tokens"):
		return "anthropic-count-tokens"
	case strings.Contains(path, "/messages"):
		return "anthropic-messages"
	case strings.Contains(path, "/chat/completions"):
		return "openai-chat"
	case strings.Contains(path, "/responses"):
		return "openai-responses"
	case strings.Contains(path, "/batches"):
		return "sail-batches"
	default:
		return "openai-compatible"
	}
}
