// Package modellimits resolves context/output token limits for
// openai-compatibility models that carry no explicit max-context-length in
// configuration. Sources, in priority order: the upstream provider's own
// GET {base-url}/models payload, then the models.dev catalog.
package modellimits

import (
	"encoding/json"
	"sort"
	"strings"
)

// Limits holds resolved token limits for one model. Zero means unknown.
type Limits struct {
	Context int
	Output  int
}

// Source identifies where a limit came from.
type Source string

const (
	SourceUpstream  Source = "upstream"
	SourceModelsDev Source = "models.dev"
)

// Resolved is a limit together with its provenance.
type Resolved struct {
	Limits
	Source   Source
	Provider string
}

// IsZero reports whether no limit is known.
func (l Limits) IsZero() bool { return l.Context <= 0 && l.Output <= 0 }

var upstreamContextKeys = []string{
	"max_model_len",
	"context_length",
	"context_window",
	"max_context_length",
	"max_input_tokens",
}

var upstreamOutputKeys = []string{
	"max_output_tokens",
	"max_completion_tokens",
	"max_tokens",
}

// UpstreamCatalog is the parsed result of one provider GET /models payload.
type UpstreamCatalog struct {
	// IDs lists every model id in payload order, deduplicated.
	IDs []string
	// Limits holds the entries that carried a limit field, keyed by id.
	Limits map[string]Limits
}

// ParseUpstreamModels extracts limits from an OpenAI-style GET /models payload.
// See ParseUpstreamCatalog for the accepted shapes.
func ParseUpstreamModels(raw []byte) map[string]Limits {
	catalog := ParseUpstreamCatalog(raw)
	if catalog == nil {
		return nil
	}
	return catalog.Limits
}

// ParseUpstreamCatalog parses an OpenAI-style GET /models payload into model
// ids and limits. It accepts a top-level "data" (OpenAI, vLLM, OpenRouter,
// LiteLLM) or "models" list, and reads limit fields from the entry itself or
// from nested "top_provider"/"limit" objects (OpenRouter/models.dev style).
// It returns nil for payloads that are not JSON.
func ParseUpstreamCatalog(raw []byte) *UpstreamCatalog {
	var payload struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	entries := payload.Data
	if len(entries) == 0 {
		entries = payload.Models
	}
	catalog := &UpstreamCatalog{IDs: make([]string, 0, len(entries))}
	out := make(map[string]Limits, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(stringValue(entry["id"]))
		if id == "" {
			id = strings.TrimSpace(stringValue(entry["name"]))
		}
		if id == "" {
			continue
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			catalog.IDs = append(catalog.IDs, id)
		}
		limits := Limits{
			Context: firstInt(entry, upstreamContextKeys),
			Output:  firstInt(entry, upstreamOutputKeys),
		}
		for _, nested := range []string{"top_provider", "limit", "limits"} {
			sub, ok := entry[nested].(map[string]any)
			if !ok {
				continue
			}
			if limits.Context <= 0 {
				limits.Context = firstInt(sub, append([]string{"context"}, upstreamContextKeys...))
			}
			if limits.Output <= 0 {
				limits.Output = firstInt(sub, append([]string{"output"}, upstreamOutputKeys...))
			}
		}
		if limits.IsZero() {
			continue
		}
		out[id] = limits
	}
	if len(out) > 0 {
		catalog.Limits = out
	}
	return catalog
}

func firstInt(entry map[string]any, keys []string) int {
	for _, key := range keys {
		if v := intValue(entry[key]); v > 0 {
			return v
		}
	}
	return 0
}

func intValue(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
