package models

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestCodexClientModelsResponse_LiveOverlayAddsAndUpdatesTemplates(t *testing.T) {
	t.Cleanup(func() {
		registry.SetCodexLiveClientModels("acct-live", nil)
	})
	registry.SetCodexLiveClientModels("acct-live", []map[string]any{
		{
			"slug": "gpt-daybreak-blue-latest", "display_name": "Daybreak Blue", "visibility": "list",
			"context_window": float64(272000), "max_context_window": float64(872000), "priority": float64(3),
			"default_reasoning_level": "low",
			"supported_reasoning_levels": []any{
				map[string]any{"effort": "low"}, map[string]any{"effort": "ultra"},
			},
		},
		{
			"slug": "gpt-reserve", "display_name": "Reserve", "visibility": "hide",
			"context_window": float64(272000), "max_context_window": float64(872000), "priority": float64(3),
		},
		{
			"slug": "gpt-5.6-sol", "display_name": "GPT-5.6 Sol (live)", "visibility": "list",
			"context_window": float64(272000), "max_context_window": float64(872000), "priority": float64(1),
		},
	})

	resp := BuildResponse([]map[string]any{
		{"id": "gpt-daybreak-blue-latest"}, {"id": "gpt-reserve"}, {"id": "gpt-5.6-sol"},
	}, nil, false)
	models, _ := resp["models"].([]map[string]any)
	bySlug := make(map[string]map[string]any, len(models))
	for _, model := range models {
		bySlug[stringModelValue(model, "slug")] = model
	}

	daybreak := bySlug["gpt-daybreak-blue-latest"]
	if daybreak == nil {
		t.Fatal("daybreak missing from Codex catalog")
	}
	if daybreak["display_name"] != "Daybreak Blue" || daybreak["visibility"] != "list" || intModelValue(daybreak, "context_window") != 272000 || intModelValue(daybreak, "max_context_window") != 872000 {
		t.Fatalf("unexpected daybreak entry: %+v", daybreak)
	}
	if levels, _ := daybreak["supported_reasoning_levels"].([]any); len(levels) != 2 {
		t.Fatalf("expected live reasoning levels, got %+v", daybreak["supported_reasoning_levels"])
	}
	if reserve := bySlug["gpt-reserve"]; reserve == nil || reserve["visibility"] != "hide" {
		t.Fatalf("hidden live model must stay hidden, got %+v", reserve)
	}
	sol := bySlug["gpt-5.6-sol"]
	if sol["display_name"] != "GPT-5.6 Sol (live)" || intModelValue(sol, "context_window") != 272000 {
		t.Fatalf("static template not updated from live: %+v", sol)
	}
	if _, ok := sol["model_messages"]; !ok {
		t.Fatalf("static template fields must survive the overlay")
	}
}
