package cliproxy

import (
	"context"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/modellimits"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

func modelLimitsOptions(cfg *config.Config) modellimits.Options {
	opts := modellimits.Options{
		Enabled:          cfg != nil && cfg.AutoModelLimitsEnabled(),
		ModelsDevURL:     modellimits.DefaultModelsDevURL,
		ModelsDevRefresh: modellimits.DefaultModelsDevRefresh,
	}
	if cfg == nil {
		return opts
	}
	opts.ProxyURL = cfg.ProxyURL
	opts.CacheDir = strings.TrimSpace(cfg.AuthDir)
	if raw := strings.TrimSpace(cfg.ModelsDevURL); raw != "" {
		opts.ModelsDevURL = raw
	}
	if raw := strings.TrimSpace(cfg.ModelsDevRefresh); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			opts.ModelsDevRefresh = parsed
		} else {
			log.Warnf("model limits: invalid models-dev-refresh %q; using default", raw)
		}
	}
	return opts
}

func sameModelLimitsOptions(a, b modellimits.Options) bool {
	return a.Enabled == b.Enabled && a.ModelsDevURL == b.ModelsDevURL &&
		a.ModelsDevRefresh == b.ModelsDevRefresh && a.CacheDir == b.CacheDir && a.ProxyURL == b.ProxyURL
}

// modelLimitsResolver returns the shared resolver for cfg, rebuilding it only
// when the relevant options changed so caches survive config reloads. It is
// nil when neither limit resolution nor auto-models needs it.
func (s *Service) modelLimitsResolver(cfg *config.Config) *modellimits.Resolver {
	if s == nil {
		return nil
	}
	opts := modelLimitsOptions(cfg)
	if !opts.Enabled && !anyCompatAutoModels(cfg) {
		return nil
	}
	s.modelLimitsMu.Lock()
	defer s.modelLimitsMu.Unlock()
	if s.modelLimits == nil || !sameModelLimitsOptions(s.modelLimits.Options(), opts) {
		s.modelLimits = modellimits.New(opts)
	}
	return s.modelLimits
}

func compatProviderSpec(compat *config.OpenAICompatibility) modellimits.ProviderSpec {
	spec := modellimits.ProviderSpec{
		Name:    strings.TrimSpace(compat.Name),
		BaseURL: strings.TrimSpace(compat.BaseURL),
		Headers: compat.Headers,
	}
	if len(compat.APIKeyEntries) > 0 {
		spec.APIKey = strings.TrimSpace(compat.APIKeyEntries[0].APIKey)
	}
	for i := range compat.Models {
		if compat.Models[i].MaxContextLength > 0 {
			continue
		}
		if name := strings.TrimSpace(compat.Models[i].Name); name != "" {
			spec.Models = append(spec.Models, name)
		}
	}
	return spec
}

// prefetchModelLimits warms limit caches for every enabled provider in parallel,
// bounded by one resolver timeout.
func (s *Service) prefetchModelLimits(cfg *config.Config) {
	resolver := s.modelLimitsResolver(cfg)
	if resolver == nil || cfg == nil {
		return
	}
	specs := make([]modellimits.ProviderSpec, 0, len(cfg.OpenAICompatibility))
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		if spec := compatProviderSpec(compat); len(spec.Models) > 0 || compat.AutoModels {
			specs = append(specs, spec)
		}
	}
	if len(specs) == 0 {
		return
	}
	resolver.Prefetch(context.Background(), specs)
}

// buildCompatConfigModels builds compat models, unions auto-discovered upstream
// models when enabled, and fills in auto-resolved limits.
func (s *Service) buildCompatConfigModels(cfg *config.Config, compat *config.OpenAICompatibility) []*ModelInfo {
	resolver := s.modelLimitsResolver(cfg)
	if resolver == nil || compat == nil {
		return buildOpenAICompatibilityConfigModels(compat)
	}
	spec := compatProviderSpec(compat)
	if compat.AutoModels {
		compat = s.withDiscoveredCompatModels(resolver, compat, &spec)
	}
	if len(spec.Models) == 0 {
		return buildOpenAICompatibilityConfigModels(compat)
	}
	resolved := resolver.Resolve(context.Background(), spec)
	if summary := modellimits.Describe(resolved); summary != "" && s.noteModelLimitsSummary(spec.Name, summary) {
		log.Infof("model limits: provider %q resolved %s", spec.Name, summary)
	}
	return buildOpenAICompatibilityConfigModelsWithLimits(compat, resolved)
}

// noteModelLimitsSummary records the last logged summary per provider and
// reports whether it changed, so repeated registrations do not spam the log.
func (s *Service) noteModelLimitsSummary(provider, summary string) bool {
	s.modelLimitsMu.Lock()
	defer s.modelLimitsMu.Unlock()
	if s.modelLimitsLogged == nil {
		s.modelLimitsLogged = make(map[string]string)
	}
	if s.modelLimitsLogged[provider] == summary {
		return false
	}
	s.modelLimitsLogged[provider] = summary
	return true
}
