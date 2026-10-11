package app

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// keyAffinity 记录「Sail 任务 ID → 创建它的 key」。
// Sail 的 background response、batch、超时 message 只能用创建时的 API key 查询；
// 网关轮询多个 key，所以查询请求必须路由回原来的 key，否则会 404。
type keyAffinity struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	data map[string]affinityEntry
}

type affinityEntry struct {
	keyID string
	at    time.Time
}

func newKeyAffinity(ttl time.Duration, max int) *keyAffinity {
	return &keyAffinity{ttl: ttl, max: max, data: map[string]affinityEntry{}}
}

func (a *keyAffinity) Put(id, keyID string) {
	id = strings.TrimSpace(id)
	if a == nil || id == "" || keyID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.data) >= a.max {
		for k, v := range a.data {
			if now.Sub(v.at) > a.ttl {
				delete(a.data, k)
			}
		}
		// 仍然超限就随机淘汰一部分（map 遍历顺序随机）。
		for k := range a.data {
			if len(a.data) < a.max*9/10 {
				break
			}
			delete(a.data, k)
		}
	}
	a.data[id] = affinityEntry{keyID: keyID, at: now}
}

func (a *keyAffinity) Get(id string) string {
	if a == nil || id == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.data[id]
	if !ok {
		return ""
	}
	if time.Since(e.at) > a.ttl {
		delete(a.data, id)
		return ""
	}
	return e.keyID
}

// affinityLookupID 从查询类路径中取出任务 ID：
//
//	/v1/responses/{id}、/v1/messages/{id}、/v1/batches/{id}[/...]
func affinityLookupID(path string) string {
	for _, prefix := range []string{"/v1/responses/", "/v1/messages/", "/v1/batches/"} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
		if rest == "" || rest == "count_tokens" {
			return ""
		}
		return rest
	}
	return ""
}

// affinityIDsFromResponse 从创建类请求的响应里取出需要记住的任务 ID。
func affinityIDsFromResponse(path string, header map[string][]string, body []byte) []string {
	var ids []string
	for k, vals := range header {
		if strings.EqualFold(k, "X-Sail-Message-Id") {
			for _, v := range vals {
				if v = strings.TrimSpace(v); v != "" {
					ids = append(ids, v)
				}
			}
		}
	}
	if path != "/v1/responses" && path != "/v1/batches" && path != "/v1/messages" {
		return ids
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return ids
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &obj) == nil && strings.TrimSpace(obj.ID) != "" {
		ids = append(ids, obj.ID)
	}
	return ids
}

// preferCandidate 把指定 key 挪到候选列表第一位。
func preferCandidate(cands []proxyCandidate, keyID string) []proxyCandidate {
	if keyID == "" {
		return cands
	}
	for i, c := range cands {
		if c.ID == keyID {
			out := make([]proxyCandidate, 0, len(cands))
			out = append(out, c)
			out = append(out, cands[:i]...)
			out = append(out, cands[i+1:]...)
			return out
		}
	}
	return cands
}
