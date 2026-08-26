package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modellimits"
)

func TestBuildOpenAICompatibilityConfigModelsWithLimits_Precedence(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name: "vllm",
		Models: []config.OpenAICompatibilityModel{
			{Name: "explicit", MaxContextLength: 4096},
			{Name: "auto"},
			{Name: "unresolved"},
		},
	}
	limits := map[string]modellimits.Resolved{
		"explicit": {Limits: modellimits.Limits{Context: 262144, Output: 8192}, Source: modellimits.SourceUpstream},
		"auto":     {Limits: modellimits.Limits{Context: 1000000, Output: 131072}, Source: modellimits.SourceModelsDev},
	}
	byID := map[string]*ModelInfo{}
	for _, m := range buildOpenAICompatibilityConfigModelsWithLimits(compat, limits) {
		byID[m.ID] = m
	}
	if got := byID["explicit"]; got.ContextLength != 4096 || got.MaxContextLength != 4096 {
		t.Fatalf("explicit max-context-length must win, got ctx=%d max=%d", got.ContextLength, got.MaxContextLength)
	}
	if got := byID["explicit"]; got.MaxCompletionTokens != 8192 {
		t.Fatalf("explicit entry should still gain resolved output, got %d", got.MaxCompletionTokens)
	}
	if got := byID["auto"]; got.ContextLength != 1000000 || got.MaxContextLength != 1000000 || got.MaxCompletionTokens != 131072 {
		t.Fatalf("auto entry = ctx=%d max=%d out=%d", got.ContextLength, got.MaxContextLength, got.MaxCompletionTokens)
	}
	if got := byID["unresolved"]; got.ContextLength != 0 || got.MaxCompletionTokens != 0 {
		t.Fatalf("unresolved entry must stay empty, got %+v", got)
	}
}

func TestCompatProviderSpec_SkipsExplicitModels(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name:          " opencode ",
		BaseURL:       "https://opencode.ai/zen/go/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k1"}, {APIKey: "k2"}},
		Models: []config.OpenAICompatibilityModel{
			{Name: "explicit", MaxContextLength: 1},
			{Name: "auto"},
			{Name: "  "},
		},
	}
	spec := compatProviderSpec(compat)
	if spec.Name != "opencode" || spec.APIKey != "k1" || len(spec.Models) != 1 || spec.Models[0] != "auto" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestModelLimitsOptions_Defaults(t *testing.T) {
	opts := modelLimitsOptions(&config.Config{AuthDir: "/auth"})
	if !opts.Enabled || opts.ModelsDevURL != modellimits.DefaultModelsDevURL || opts.CacheDir != "/auth" || opts.ModelsDevRefresh != modellimits.DefaultModelsDevRefresh {
		t.Fatalf("defaults = %+v", opts)
	}
	off := false
	if modelLimitsOptions(&config.Config{AutoModelLimits: &off}).Enabled {
		t.Fatal("auto-model-limits: false must disable")
	}
	custom := modelLimitsOptions(&config.Config{ModelsDevURL: "http://x/api.json", ModelsDevRefresh: "1h"})
	if custom.ModelsDevURL != "http://x/api.json" || custom.ModelsDevRefresh.Hours() != 1 {
		t.Fatalf("custom = %+v", custom)
	}
}

func TestModelLimitsResolver_ReusedAcrossReloads(t *testing.T) {
	s := &Service{}
	first := s.modelLimitsResolver(&config.Config{AuthDir: "/auth"})
	second := s.modelLimitsResolver(&config.Config{AuthDir: "/auth"})
	if first == nil || first != second {
		t.Fatal("resolver must be reused when options are unchanged")
	}
	if third := s.modelLimitsResolver(&config.Config{AuthDir: "/other"}); third == first {
		t.Fatal("resolver must be rebuilt when options change")
	}
}
