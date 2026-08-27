package cliproxy

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/modellimits"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

func anyCompatAutoModels(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.OpenAICompatibility {
		if cfg.OpenAICompatibility[i].AutoModels && !cfg.OpenAICompatibility[i].Disabled {
			return true
		}
	}
	return false
}

// discoverCompatModels returns config entries for every upstream id that is
// neither configured (by name or alias) nor excluded by a glob. Discovered
// entries carry no alias, so they are served under their upstream id.
func discoverCompatModels(compat *config.OpenAICompatibility, upstream []string) []config.OpenAICompatibilityModel {
	if compat == nil || len(upstream) == 0 {
		return nil
	}
	configured := make(map[string]struct{}, 2*len(compat.Models))
	for _, model := range compat.Models {
		for _, key := range []string{model.Name, model.Alias} {
			if key = strings.ToLower(strings.TrimSpace(key)); key != "" {
				configured[key] = struct{}{}
			}
		}
	}
	seen := make(map[string]struct{}, len(upstream))
	out := make([]config.OpenAICompatibilityModel, 0, len(upstream))
	for _, id := range upstream {
		id = strings.TrimSpace(id)
		key := strings.ToLower(id)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := configured[key]; ok {
			continue
		}
		if autoModelsExcluded(key, compat.AutoModelsExclude) {
			continue
		}
		out = append(out, config.OpenAICompatibilityModel{Name: id})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// autoModelsExcluded matches a lower-cased id against path.Match globs
// (case-insensitive); an invalid pattern only matches the id verbatim.
func autoModelsExcluded(key string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if matched, err := path.Match(pattern, key); err == nil {
			if matched {
				return true
			}
		} else if pattern == key {
			return true
		}
	}
	return false
}

// withDiscoveredCompatModels returns compat extended with upstream-discovered
// models (or compat itself when nothing was discovered) and appends the new
// names to spec.Models so their limits get resolved too.
func (s *Service) withDiscoveredCompatModels(resolver *modellimits.Resolver, compat *config.OpenAICompatibility, spec *modellimits.ProviderSpec) *config.OpenAICompatibility {
	ids := resolver.UpstreamModels(context.Background(), *spec)
	discovered := discoverCompatModels(compat, ids)
	names := make([]string, 0, len(discovered))
	for _, model := range discovered {
		names = append(names, model.Name)
	}
	if ids != nil {
		s.logCompatModelsDiff(spec.Name, names)
	}
	if len(discovered) == 0 {
		return compat
	}
	merged := *compat
	merged.Models = make([]config.OpenAICompatibilityModel, 0, len(compat.Models)+len(discovered))
	merged.Models = append(merged.Models, compat.Models...)
	merged.Models = append(merged.Models, discovered...)
	spec.Models = append(spec.Models, names...)
	return &merged
}

// logCompatModelsDiff logs the change in discovered models since the last
// registration of provider; the first result logs every id as added.
func (s *Service) logCompatModelsDiff(provider string, current []string) {
	s.modelLimitsMu.Lock()
	defer s.modelLimitsMu.Unlock()
	if s.autoModelsSeen == nil {
		s.autoModelsSeen = make(map[string][]string)
	}
	previous, known := s.autoModelsSeen[provider]
	added, removed := diffModelIDs(previous, current)
	s.autoModelsSeen[provider] = append([]string(nil), current...)
	if len(added) == 0 && len(removed) == 0 {
		if !known {
			log.Infof("compat models: provider %q discovered nothing beyond config", provider)
		}
		return
	}
	parts := make([]string, 0, len(added)+len(removed))
	for _, id := range added {
		parts = append(parts, "+"+id)
	}
	for _, id := range removed {
		parts = append(parts, "-"+id)
	}
	log.Infof("compat models: provider %q discovered %s", provider, strings.Join(parts, ","))
}

func diffModelIDs(previous, current []string) (added, removed []string) {
	prev := make(map[string]struct{}, len(previous))
	for _, id := range previous {
		prev[id] = struct{}{}
	}
	cur := make(map[string]struct{}, len(current))
	for _, id := range current {
		cur[id] = struct{}{}
		if _, ok := prev[id]; !ok {
			added = append(added, id)
		}
	}
	for _, id := range previous {
		if _, ok := cur[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// startCompatAutoModelsRefresh re-registers auths of auto-models providers
// whenever their cached upstream catalog is due for a refresh.
func (s *Service) startCompatAutoModelsRefresh(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(modellimits.UpstreamRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshStaleCompatAutoModels(ctx)
			}
		}
	}()
}

func (s *Service) refreshStaleCompatAutoModels(ctx context.Context) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	resolver := s.modelLimitsResolver(cfg)
	if resolver == nil || !anyCompatAutoModels(cfg) {
		return
	}
	auths := s.coreManager.List()
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled || !compat.AutoModels {
			continue
		}
		spec := compatProviderSpec(compat)
		if !resolver.UpstreamStale(spec) {
			continue
		}
		for _, auth := range compatAuthsForProvider(auths, compat.Name) {
			if ctx.Err() != nil {
				return
			}
			s.refreshModelRegistrationForAuth(auth)
		}
	}
}

func compatAuthsForProvider(auths []*coreauth.Auth, name string) []*coreauth.Auth {
	name = strings.TrimSpace(name)
	out := make([]*coreauth.Auth, 0, 1)
	for _, auth := range auths {
		if auth == nil || auth.Disabled || auth.Attributes == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(auth.Attributes["compat_name"]), name) {
			out = append(out, auth)
		}
	}
	return out
}
