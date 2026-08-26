package modellimits

import "testing"

func TestParseUpstreamModels_VLLMShape(t *testing.T) {
	raw := []byte(`{"object":"list","data":[{"id":"Qwen3.8-27B","object":"model","max_model_len":262144},{"id":"no-limits","object":"model"}]}`)
	got := ParseUpstreamModels(raw)
	if got["Qwen3.8-27B"] != (Limits{Context: 262144}) {
		t.Fatalf("vLLM limits = %+v", got["Qwen3.8-27B"])
	}
	if _, ok := got["no-limits"]; ok {
		t.Fatal("entry without limit fields must be skipped")
	}
}

func TestParseUpstreamModels_OpenRouterShape(t *testing.T) {
	raw := []byte(`{"data":[{"id":"vendor/model","context_length":131072,"top_provider":{"context_length":131072,"max_completion_tokens":16384}},{"id":"nested-only","top_provider":{"context_length":8192,"max_completion_tokens":1024}}]}`)
	got := ParseUpstreamModels(raw)
	if got["vendor/model"] != (Limits{Context: 131072, Output: 16384}) {
		t.Fatalf("OpenRouter limits = %+v", got["vendor/model"])
	}
	if got["nested-only"] != (Limits{Context: 8192, Output: 1024}) {
		t.Fatalf("nested-only limits = %+v", got["nested-only"])
	}
}

func TestParseUpstreamModels_Invalid(t *testing.T) {
	if got := ParseUpstreamModels([]byte(`not json`)); got != nil {
		t.Fatalf("expected nil for invalid payload, got %v", got)
	}
}
