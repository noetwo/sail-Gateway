package app

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

func normalizeModelName(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func rotateProxyCandidates(list []proxyCandidate, turn uint64) []proxyCandidate {
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if len(list) == 0 {
		return list
	}
	start := int(turn % uint64(len(list)))
	result := make([]proxyCandidate, 0, len(list))
	result = append(result, list[start:]...)
	result = append(result, list[:start]...)
	return result
}

func sortProxyCandidatesByImportTime(list []proxyCandidate) {
	sort.Slice(list, func(i, j int) bool {
		return importTimeLess(list[i].CreatedAt, list[i].ID, list[j].CreatedAt, list[j].ID)
	})
}

// candidateByID 返回指定 key 的候选信息（即使它处于冷却中也返回，用于查询已创建的任务）。
func (s *AppState) candidateByID(id string) (proxyCandidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.Keys[id]
	if !ok || k.Scrapped || strings.TrimSpace(k.APIKey) == "" {
		return proxyCandidate{}, false
	}
	return proxyCandidate{ID: id, APIKey: k.APIKey, CreatedAt: keyImportTime(id, k)}, true
}

// nextProxyCandidates 返回本次请求要依次尝试的 key。
// 轮询模式：每次请求起点后移一位；粘性模式：固定 key 优先，失败后按导入顺序兜底。
func (s *AppState) nextProxyCandidates() []proxyCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]proxyCandidate, 0, len(s.Keys))
	for id, k := range s.Keys {
		if k.Scrapped || k.Paused || strings.TrimSpace(k.APIKey) == "" {
			continue
		}
		keyID := k.ID
		if keyID == "" {
			keyID = id
		}
		list = append(list, proxyCandidate{ID: keyID, APIKey: k.APIKey, CreatedAt: keyImportTime(id, k)})
	}
	if len(list) == 0 {
		return list
	}

	if s.StickyMode {
		sortProxyCandidatesByImportTime(list)
		stickyIdx := -1
		if s.StickyKeyID != "" {
			for i, c := range list {
				if c.ID == s.StickyKeyID {
					stickyIdx = i
					break
				}
			}
		}
		if stickyIdx < 0 {
			stickyIdx = 0
			s.StickyKeyID = list[0].ID
			_ = s.save()
		}
		result := make([]proxyCandidate, 0, len(list))
		result = append(result, list[stickyIdx])
		for i, c := range list {
			if i != stickyIdx {
				result = append(result, c)
			}
		}
		return result
	}

	// 轮询计数只在内存里递增，不再每个请求都写盘。
	turn := s.proxyTurn
	s.proxyTurn++
	return rotateProxyCandidates(list, turn)
}

func (s *AppState) markProxySuccess(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.Keys[id]
	if k == nil {
		return
	}
	now := time.Now().UTC()
	k.LastProxyAt = now
	k.ProxyReqCount++
	k.LastErr = ""
	k.ConsecFails = 0
	k.UpdatedAt = now
	if s.StickyMode && s.StickyKeyID == "" {
		s.StickyKeyID = id
	}
	s.saveThrottledLocked()
}

func (s *AppState) markProxyFailure(id, msg string, statusCode int, cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.Keys[id]
	if k == nil {
		return
	}
	now := time.Now().UTC()
	k.LastProxyAt = now
	k.LastErr = msg
	k.UpdatedAt = now

	switch {
	case retryStatusEnabled(statusCode, cfg.ExhaustStatusCodes):
		// 402：额度用完，直接报废
		k.ConsecFails = 0
		k.Scrapped = true
		k.ScrappedErr = fmt.Sprintf("额度已用完（请求返回 %d）", statusCode)
		k.ScrappedAt = now
		k.Paused = true
		k.AutoCooldown = "exhausted"
		if s.StickyMode && s.StickyKeyID == id {
			s.StickyKeyID = ""
		}
		s.renumberKeys()
		log.Printf("[防打断] key %s (%s) status=%d 额度用完，报废", k.Name, id, statusCode)

	case retryStatusEnabled(statusCode, cfg.CooldownStatusCodes):
		k.ConsecFails = 0
		k.Paused = true
		k.AutoCooldown = "status"
		k.LastErr = fmt.Sprintf("%d 命中冷却策略，自动冷却", statusCode)
		if s.StickyMode && s.StickyKeyID == id {
			s.StickyKeyID = ""
		}
		log.Printf("[防打断] key %s (%s) status=%d 命中冷却策略，进入冷却", k.Name, id, statusCode)

	case retryStatusEnabled(statusCode, cfg.ProxyScrapStatusCodes):
		k.ConsecFails++
		threshold := cfg.ProxyScrapFailThreshold
		if threshold <= 0 {
			threshold = 3
		}
		if k.ConsecFails >= threshold {
			k.Scrapped = true
			k.ScrappedErr = fmt.Sprintf("连续%d次%d错误，自动报废: %s", k.ConsecFails, statusCode, msg)
			k.ScrappedAt = now
			k.Paused = true
			if s.StickyMode && s.StickyKeyID == id {
				s.StickyKeyID = ""
			}
			s.renumberKeys()
			log.Printf("[防打断] key %s (%s) 连续%d次%d，自动报废", k.Name, id, k.ConsecFails, statusCode)
		} else {
			log.Printf("[防打断] key %s (%s) %d错误 (%d/%d)，切换下一个", k.Name, id, statusCode, k.ConsecFails, threshold)
		}

	case statusCode == http.StatusTooManyRequests || statusCode == http.StatusServiceUnavailable:
		// 429 / 503 是 Sail 的限流或容量不足，不代表 key 有问题。
		log.Printf("[防打断] key %s (%s) status=%d 限流/容量不足，切换下一个", k.Name, id, statusCode)

	default:
		k.ConsecFails = 0
		log.Printf("[防打断] key %s (%s) status=%d，切换下一个", k.Name, id, statusCode)
	}

	_ = s.save()
}
