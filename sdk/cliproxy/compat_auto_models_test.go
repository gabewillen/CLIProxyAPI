package cliproxy

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func autoModelNames(models []config.OpenAICompatibilityModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	return out
}

func TestDiscoverCompatModels_UnionExcludeAlias(t *testing.T) {
	compat := &config.OpenAICompatibility{
		Name:              "opencode",
		AutoModels:        true,
		AutoModelsExclude: []string{"gpt-*", "Claude-*", "grok-*"},
		Models: []config.OpenAICompatibilityModel{
			{Name: "glm-5.3", Alias: "glm"},
			{Name: "upstream-name", Alias: "served-alias"},
		},
	}
	upstream := []string{"glm-5.3", "glm-5.3-flash", "gpt-5.6-luna", "claude-opus", "grok-4.6", "served-alias", "GLM-5.3-FLASH", "longcat-2.0", ""}
	got := discoverCompatModels(compat, upstream)
	want := []string{"glm-5.3-flash", "longcat-2.0"}
	if !reflect.DeepEqual(autoModelNames(got), want) {
		t.Fatalf("discovered = %v, want %v", autoModelNames(got), want)
	}
	for _, m := range got {
		if m.Alias != "" || m.MaxContextLength != 0 {
			t.Fatalf("discovered entry must be bare, got %+v", m)
		}
	}
}

func TestDiscoverCompatModels_RemovedUpstreamDropsOut(t *testing.T) {
	compat := &config.OpenAICompatibility{AutoModels: true, Models: []config.OpenAICompatibilityModel{{Name: "static"}}}
	first := autoModelNames(discoverCompatModels(compat, []string{"static", "a", "b"}))
	second := autoModelNames(discoverCompatModels(compat, []string{"static", "a"}))
	if !reflect.DeepEqual(first, []string{"a", "b"}) || !reflect.DeepEqual(second, []string{"a"}) {
		t.Fatalf("first=%v second=%v", first, second)
	}
	added, removed := diffModelIDs(first, second)
	if len(added) != 0 || !reflect.DeepEqual(removed, []string{"b"}) {
		t.Fatalf("diff added=%v removed=%v", added, removed)
	}
}

func TestAutoModelsExcluded_InvalidPatternMatchesVerbatim(t *testing.T) {
	if !autoModelsExcluded("bad[", []string{"bad["}) || autoModelsExcluded("bad", []string{"bad["}) {
		t.Fatal("invalid glob must only match itself")
	}
}

func newAutoModelsTestConfig(t *testing.T, upstream string, autoModels bool) *config.Config {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(upstream)) })
	mux.HandleFunc("/api.json", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusBadGateway) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &config.Config{
		ModelsDevURL: server.URL + "/api.json",
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:              "opencode",
			BaseURL:           server.URL + "/v1",
			AutoModels:        autoModels,
			AutoModelsExclude: []string{"gpt-*"},
			Models:            []config.OpenAICompatibilityModel{{Name: "glm-5.3", Alias: "glm", MaxContextLength: 128}},
		}},
	}
}

func TestBuildCompatConfigModels_AutoModels(t *testing.T) {
	upstream := `{"data":[{"id":"glm-5.3","context_length":200000},{"id":"glm-5.3-flash","context_length":131072,"max_completion_tokens":8192},{"id":"gpt-5.6-luna"}]}`
	cfg := newAutoModelsTestConfig(t, upstream, true)
	s := &Service{}
	byID := map[string]*ModelInfo{}
	for _, m := range s.buildCompatConfigModels(cfg, &cfg.OpenAICompatibility[0]) {
		byID[m.ID] = m
	}
	if len(byID) != 2 {
		t.Fatalf("models = %v, want glm + glm-5.3-flash", byID)
	}
	if got := byID["glm"]; got == nil || got.ContextLength != 128 {
		t.Fatalf("configured alias must keep its explicit limit, got %+v", got)
	}
	if got := byID["glm-5.3-flash"]; got == nil || got.ContextLength != 131072 || got.MaxCompletionTokens != 8192 || got.OwnedBy != "opencode" {
		t.Fatalf("discovered model must get upstream limits, got %+v", got)
	}
	if _, ok := byID["gpt-5.6-luna"]; ok {
		t.Fatal("excluded family must not be registered")
	}
	if seen := s.autoModelsSeen["opencode"]; !reflect.DeepEqual(seen, []string{"glm-5.3-flash"}) {
		t.Fatalf("seen = %v", seen)
	}
}

func TestBuildCompatConfigModels_AutoModelsOffIsStatic(t *testing.T) {
	cfg := newAutoModelsTestConfig(t, `{"data":[{"id":"glm-5.3"},{"id":"glm-5.3-flash"}]}`, false)
	s := &Service{}
	models := s.buildCompatConfigModels(cfg, &cfg.OpenAICompatibility[0])
	if len(models) != 1 || models[0].ID != "glm" {
		t.Fatalf("static list expected, got %v", models)
	}
	if len(s.autoModelsSeen) != 0 {
		t.Fatal("no discovery state when auto-models is off")
	}
}
