package openai

import "testing"

func TestFilterOpenAIModelsList_ExtendedFieldsFlag(t *testing.T) {
	models := []map[string]any{{
		"id": "m", "object": "model", "created": int64(1), "owned_by": "p",
		"context_length": 262144, "max_context_length": 262144, "max_completion_tokens": 8192,
		"display_name": "hidden",
	}}
	strict := filterOpenAIModelsList(models, false)[0]
	if len(strict) != 4 {
		t.Fatalf("strict mode must keep 4 fields, got %v", strict)
	}
	extended := filterOpenAIModelsList(models, true)[0]
	if extended["context_length"] != 262144 || extended["max_context_length"] != 262144 || extended["max_completion_tokens"] != 8192 {
		t.Fatalf("extended fields missing: %v", extended)
	}
	if _, ok := extended["display_name"]; ok {
		t.Fatal("unrelated fields must still be stripped")
	}
	if len(extended) != 7 {
		t.Fatalf("extended field count = %d, want 7", len(extended))
	}
}
