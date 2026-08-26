package registry

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// codexLiveModelsStore holds per-account live Codex catalog entries that
// overlay the static Codex client templates.
type codexLiveModelsStore struct {
	mu        sync.RWMutex
	byAccount map[string][]map[string]any
	revision  uint64
}

var codexLiveCatalogStore = &codexLiveModelsStore{byAccount: make(map[string][]map[string]any)}

// SetCodexLiveClientModels stores the live catalog entries fetched for one
// account and reports whether the overlay changed.
func SetCodexLiveClientModels(account string, models []map[string]any) bool {
	account = strings.TrimSpace(account)
	if account == "" {
		return false
	}
	next, err := json.Marshal(models)
	if err != nil {
		return false
	}
	codexLiveCatalogStore.mu.Lock()
	defer codexLiveCatalogStore.mu.Unlock()
	if prev, errPrev := json.Marshal(codexLiveCatalogStore.byAccount[account]); errPrev == nil && string(prev) == string(next) {
		return false
	}
	var cloned []map[string]any
	if err = json.Unmarshal(next, &cloned); err != nil {
		return false
	}
	if len(cloned) == 0 {
		delete(codexLiveCatalogStore.byAccount, account)
	} else {
		codexLiveCatalogStore.byAccount[account] = cloned
	}
	codexLiveCatalogStore.revision++
	return true
}

// GetCodexLiveClientModelsSnapshot returns the union of live entries across
// accounts keyed by slug (accounts iterated in sorted order, first wins) and
// the overlay revision.
func GetCodexLiveClientModelsSnapshot() (map[string]map[string]any, uint64) {
	codexLiveCatalogStore.mu.RLock()
	defer codexLiveCatalogStore.mu.RUnlock()
	accounts := make([]string, 0, len(codexLiveCatalogStore.byAccount))
	for account := range codexLiveCatalogStore.byAccount {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	union := make(map[string]map[string]any)
	for _, account := range accounts {
		for _, entry := range codexLiveCatalogStore.byAccount[account] {
			slug, _ := entry["slug"].(string)
			slug = strings.TrimSpace(slug)
			if slug == "" {
				continue
			}
			if _, exists := union[slug]; exists {
				continue
			}
			union[slug] = entry
		}
	}
	return union, codexLiveCatalogStore.revision
}
