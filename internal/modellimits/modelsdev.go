package modellimits

import (
	"encoding/json"
	"net/url"
	"strings"
)

// DefaultModelsDevURL is the public models.dev catalog endpoint.
const DefaultModelsDevURL = "https://models.dev/api.json"

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Name   string                    `json:"name"`
	API    string                    `json:"api"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

// ModelsDevCatalog is a parsed models.dev api.json document.
type ModelsDevCatalog struct {
	providers map[string]modelsDevProvider
	order     []string
}

// ParseModelsDev parses a models.dev api.json payload.
func ParseModelsDev(raw []byte) (*ModelsDevCatalog, error) {
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(raw, &providers); err != nil {
		return nil, err
	}
	for id, provider := range providers {
		if provider.ID == "" {
			provider.ID = id
			providers[id] = provider
		}
	}
	return &ModelsDevCatalog{providers: providers, order: sortedKeys(providers)}, nil
}

// Len returns the number of providers in the catalog.
func (c *ModelsDevCatalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.providers)
}

// Lookup finds limits for modelID. Providers are tried in priority order:
// one whose api URL matches baseURL, then one whose id or name matches
// providerName, then the first provider (sorted by id) that lists the model.
// It returns the chosen provider id alongside the limits.
func (c *ModelsDevCatalog) Lookup(modelID, providerName, baseURL string) (Limits, string, bool) {
	if c == nil || strings.TrimSpace(modelID) == "" {
		return Limits{}, "", false
	}
	modelID = strings.TrimSpace(modelID)
	normalizedBase := normalizeBaseURL(baseURL)
	normalizedName := strings.ToLower(strings.TrimSpace(providerName))

	var byURL, byName string
	for _, id := range c.order {
		provider := c.providers[id]
		if _, ok := provider.Models[modelID]; !ok {
			continue
		}
		if byURL == "" && normalizedBase != "" && normalizeBaseURL(provider.API) == normalizedBase {
			byURL = id
		}
		if byName == "" && normalizedName != "" &&
			(strings.EqualFold(provider.ID, normalizedName) || strings.EqualFold(provider.Name, normalizedName)) {
			byName = id
		}
	}
	chosen := byURL
	if chosen == "" {
		chosen = byName
	}
	if chosen == "" {
		for _, id := range c.order {
			if _, ok := c.providers[id].Models[modelID]; ok {
				chosen = id
				break
			}
		}
	}
	if chosen == "" {
		return Limits{}, "", false
	}
	model := c.providers[chosen].Models[modelID]
	limits := Limits{Context: model.Limit.Context, Output: model.Limit.Output}
	if limits.IsZero() {
		return Limits{}, chosen, false
	}
	return limits, chosen, true
}

func normalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	return strings.ToLower(parsed.Host) + strings.TrimRight(parsed.Path, "/")
}
