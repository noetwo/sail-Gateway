package app

import (
	"path/filepath"
	"testing"
	"time"
)

func testState(t *testing.T, keys map[string]*Key) *AppState {
	t.Helper()
	now := time.Now().UTC()
	for _, k := range keys {
		if k.CreatedAt.IsZero() {
			k.CreatedAt = now
		}
		if k.UpdatedAt.IsZero() {
			k.UpdatedAt = now
		}
	}
	return &AppState{
		filePath: filepath.Join(t.TempDir(), "state.json"),
		Keys:     keys,
	}
}

func candidateIDs(cands []proxyCandidate) []string {
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	return ids
}

func requireIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got ids %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got ids %v, want %v", got, want)
		}
	}
}

func TestNextProxyCandidatesRotates(t *testing.T) {
	state := testState(t, map[string]*Key{
		"01": {ID: "01", APIKey: "sk_1"},
		"02": {ID: "02", APIKey: "sk_2"},
		"03": {ID: "03", APIKey: "sk_3", Paused: true},
	})
	requireIDs(t, candidateIDs(state.nextProxyCandidates()), []string{"01", "02"})
	requireIDs(t, candidateIDs(state.nextProxyCandidates()), []string{"02", "01"})
}

func TestNextProxyCandidatesStickyAutoSelectsOldestImportedKey(t *testing.T) {
	base := time.Date(2026, 6, 22, 22, 18, 41, 0, time.UTC)
	state := testState(t, map[string]*Key{
		"01-newer": {ID: "01-newer", APIKey: "vck_team_new", CreatedAt: base.Add(time.Hour)},
		"02-older": {ID: "02-older", APIKey: "vck_team_old", CreatedAt: base},
		"03-scrap": {ID: "03-scrap", APIKey: "sk_scrap", Scrapped: true, CreatedAt: base.Add(-time.Hour)},
	})
	state.StickyMode = true

	got := candidateIDs(state.nextProxyCandidates())
	requireIDs(t, got, []string{"02-older", "01-newer"})
	if state.StickyKeyID != "02-older" {
		t.Fatalf("StickyKeyID = %q, want 02-older", state.StickyKeyID)
	}
}

func TestFirstAvailableStickyKeyIDLockedUsesOldestImportTime(t *testing.T) {
	base := time.Date(2026, 6, 22, 22, 18, 41, 0, time.UTC)
	state := testState(t, map[string]*Key{
		"01-newer":  {ID: "01-newer", APIKey: "vck_team_new", CreatedAt: base.Add(time.Hour)},
		"02-older":  {ID: "02-older", APIKey: "vck_team_old", CreatedAt: base},
		"03-paused": {ID: "03-paused", APIKey: "vck_paused", Paused: true, CreatedAt: base.Add(-time.Hour)},
	})

	if got := state.firstAvailableStickyKeyIDLocked(); got != "02-older" {
		t.Fatalf("firstAvailableStickyKeyIDLocked = %q, want 02-older", got)
	}
}
