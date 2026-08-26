package codexmodels

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

const sampleCatalog = `{"models":[
 {"slug":"gpt-5.6-sol","display_name":"GPT-5.6 Sol","visibility":"list","context_window":272000,"max_context_window":872000,"priority":1,
  "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},
 {"slug":"gpt-daybreak-blue-latest","display_name":"Daybreak Blue","description":"Cyber model","visibility":"list","context_window":272000,"max_context_window":872000,"priority":3,
  "input_modalities":["text","image"],
  "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},
 {"slug":"gpt-reserve","display_name":"Reserve","visibility":"hide","context_window":272000,"max_context_window":872000,"priority":3,
  "supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}]},
 {"display_name":"no slug"}
]}`

func staticModels() []*registry.ModelInfo {
	return []*registry.ModelInfo{
		{ID: "gpt-5.6-sol", Object: "model", Created: 100, OwnedBy: "openai", Type: "openai", DisplayName: "GPT 5.6 Sol",
			ContextLength: 372000, MaxCompletionTokens: 128000, Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}},
		{ID: "gpt-5.5", Object: "model", Created: 90, OwnedBy: "openai", Type: "openai", DisplayName: "GPT-5.5", ContextLength: 272000},
	}
}

func TestParseDropsEntriesWithoutSlug(t *testing.T) {
	entries, err := Parse([]byte(sampleCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if Visibility(entries[2]) != "hide" {
		t.Fatalf("expected gpt-reserve hidden, got %q", Visibility(entries[2]))
	}
	if _, err = Parse([]byte(`{"foo":1}`)); err == nil {
		t.Fatal("expected error for payload without models")
	}
}

func TestMergeAddsUpdatesAndPreservesStatic(t *testing.T) {
	entries, err := Parse([]byte(sampleCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	result := Merge(staticModels(), entries)

	if got := result.Added; len(got) != 2 || got[0] != "gpt-daybreak-blue-latest" || got[1] != "gpt-reserve" {
		t.Fatalf("unexpected added: %v", got)
	}
	if got := result.Updated; len(got) != 1 || got[0] != "gpt-5.6-sol" {
		t.Fatalf("unexpected updated: %v", got)
	}

	byID := make(map[string]*registry.ModelInfo)
	for _, model := range result.Models {
		byID[model.ID] = model
	}
	if _, ok := byID["gpt-5.5"]; !ok {
		t.Fatal("static-only gpt-5.5 must be preserved")
	}
	daybreak := byID["gpt-daybreak-blue-latest"]
	if daybreak == nil {
		t.Fatal("daybreak missing")
	}
	if daybreak.DisplayName != "Daybreak Blue" || daybreak.ContextLength != 272000 || daybreak.Type != "openai" || daybreak.Created != 100 {
		t.Fatalf("unexpected daybreak model: %+v", daybreak)
	}
	if daybreak.MaxContextLength != 0 {
		t.Fatalf("max_context_window must not become a context override, got %d", daybreak.MaxContextLength)
	}
	if daybreak.Thinking == nil || len(daybreak.Thinking.Levels) != 6 || daybreak.Thinking.Levels[5] != "ultra" {
		t.Fatalf("unexpected daybreak reasoning levels: %+v", daybreak.Thinking)
	}
	if len(daybreak.SupportedInputModalities) != 2 {
		t.Fatalf("unexpected modalities: %v", daybreak.SupportedInputModalities)
	}
	sol := byID["gpt-5.6-sol"]
	if sol.DisplayName != "GPT-5.6 Sol" || sol.ContextLength != 272000 || len(sol.Thinking.Levels) != 6 {
		t.Fatalf("static sol not updated from live: %+v", sol)
	}
	if reserve := byID["gpt-reserve"]; reserve == nil {
		t.Fatal("hidden live model must still be registered")
	}

	// A second merge with the merged output is a no-op.
	again := Merge(result.Models, entries)
	if len(again.Added) != 0 || len(again.Updated) != 0 {
		t.Fatalf("merge must be idempotent, got added=%v updated=%v", again.Added, again.Updated)
	}
}

func TestMergeWithoutLiveKeepsStatic(t *testing.T) {
	static := staticModels()
	result := Merge(static, nil)
	if len(result.Models) != len(static) || len(result.Added) != 0 || len(result.Updated) != 0 {
		t.Fatalf("unexpected merge without live entries: %+v", result)
	}
}

func TestStoreDisabledReturnsNothing(t *testing.T) {
	store := New(Options{Enabled: false, CacheDir: t.TempDir()})
	entries, source := store.Catalog(context.Background(), Credentials{AccountID: "acct", AccessToken: "tok"})
	if entries != nil || source != "" {
		t.Fatalf("disabled store must return nothing, got %d entries (%q)", len(entries), source)
	}
	if stale := store.Stale([]string{"acct"}); stale != nil {
		t.Fatalf("disabled store must report nothing stale, got %v", stale)
	}
}

func TestStoreDiskCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	entries, err := Parse([]byte(sampleCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	fetchedAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	raw, err := json.Marshal(cacheFile{Accounts: map[string]*AccountCatalog{"acct-1": {FetchedAt: fetchedAt, Models: entries}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err = os.WriteFile(filepath.Join(dir, CacheFile), raw, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	store := New(Options{Enabled: true, CacheDir: dir, Refresh: 6 * time.Hour})
	got, source := store.Catalog(context.Background(), Credentials{AccountID: "acct-1", AccessToken: "tok"})
	if source != "cache" || len(got) != 3 || Slug(got[1]) != "gpt-daybreak-blue-latest" {
		t.Fatalf("expected cached catalog, got source=%q entries=%d", source, len(got))
	}
	if stale := store.Stale([]string{"acct-1"}); len(stale) != 0 {
		t.Fatalf("fresh cache must not be stale, got %v", stale)
	}
	if stale := store.Stale([]string{"acct-2"}); len(stale) != 1 {
		t.Fatalf("unknown account must be stale, got %v", stale)
	}

	// A stale cache with an unreachable upstream still serves the cached copy.
	store = New(Options{Enabled: true, CacheDir: dir, Refresh: time.Minute, Timeout: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, source = store.Catalog(ctx, Credentials{AccountID: "acct-1", AccessToken: "tok"})
	if source != "cache" || len(got) != 3 {
		t.Fatalf("expected cache fallback after failed fetch, got source=%q entries=%d", source, len(got))
	}

	// Persisting writes the same file shape back.
	store.mu.Lock()
	store.accounts["acct-2"] = &AccountCatalog{FetchedAt: time.Now(), Models: entries[:1]}
	store.saveDiskLocked()
	store.mu.Unlock()
	raw, err = os.ReadFile(filepath.Join(dir, CacheFile))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var file cacheFile
	if err = json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	if len(file.Accounts) != 2 || len(file.Accounts["acct-2"].Models) != 1 || !file.Accounts["acct-1"].FetchedAt.Equal(fetchedAt) {
		t.Fatalf("unexpected persisted cache: %+v", file.Accounts)
	}
}
