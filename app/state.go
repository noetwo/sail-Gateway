package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func loadState(path string) (*AppState, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	state := &AppState{filePath: path, Keys: map[string]*Key{}}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		state.normalizeRetrySettings()
		state.normalizeModelSettings()
		if err := state.save(); err != nil {
			return nil, err
		}
		return state, nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, state); err != nil {
			return nil, err
		}
	}
	if state.Keys == nil {
		state.Keys = map[string]*Key{}
	}
	state.filePath = path
	// 从 Vercel 版升级：旧的默认重试码包含 400 和全部 5xx，在 Sail 上会导致重复扣费，换成新默认值。
	if strings.TrimSpace(state.RetryCodes) == legacyRetryStatusCodes {
		state.RetryCodes = defaultRetryStatusCodes
	}
	state.normalizeRetrySettings()
	state.normalizeModelSettings()

	for id, k := range state.Keys {
		if k.CreatedAt.IsZero() {
			k.CreatedAt = keyImportTime(id, k)
		}
		if k.UpdatedAt.IsZero() && !k.CreatedAt.IsZero() {
			k.UpdatedAt = k.CreatedAt
		}
	}
	if err := state.save(); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *AppState) save() error {
	s.LastSaved = time.Now().UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

// saveThrottledLocked 用于高频的成功计数：最多每 5 秒落盘一次，避免每个请求都写整个 state.json。
// 调用方需持有写锁。
func (s *AppState) saveThrottledLocked() {
	if time.Since(s.LastSaved) < 5*time.Second {
		s.dirty = true
		return
	}
	s.dirty = false
	_ = s.save()
}

// autoRefreshDue 判断是否到了自动刷新时间；到期则记录本次时间并落盘。
// 首次运行（从未自动刷新过）时只记录时间、不立即刷新，避免每次部署都触发一轮。
func (s *AppState) autoRefreshDue(interval time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if s.LastAutoRefresh.IsZero() {
		s.LastAutoRefresh = now
		_ = s.save()
		return false
	}
	if now.Sub(s.LastAutoRefresh) < interval {
		return false
	}
	s.LastAutoRefresh = now
	_ = s.save()
	return true
}

// flushIfDirty 把节流期间积累的改动写盘，由后台定时调用。
func (s *AppState) flushIfDirty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		s.dirty = false
		_ = s.save()
	}
}
