package cliproxy

import (
	"context"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexmodels"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	codexLiveModelsTick          = 15 * time.Minute
	codexLiveAccessTokenLeeway   = 30 * time.Second
	codexLiveTokenRefreshRetries = 3
)

func codexLiveModelsOptions(cfg *config.Config) codexmodels.Options {
	opts := codexmodels.Options{
		Enabled: cfg != nil && cfg.CodexLiveModelsEnabled(),
		Refresh: codexmodels.DefaultRefresh,
	}
	if cfg == nil {
		return opts
	}
	opts.ProxyURL = cfg.ProxyURL
	opts.CacheDir = strings.TrimSpace(cfg.AuthDir)
	opts.UserAgent = strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent)
	if raw := strings.TrimSpace(cfg.CodexLiveModelsRefresh); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			opts.Refresh = parsed
		} else {
			log.Warnf("codex live models: invalid codex-live-models-refresh %q; using default", raw)
		}
	}
	return opts
}

func sameCodexLiveModelsOptions(a, b codexmodels.Options) bool {
	return a.Enabled == b.Enabled && a.Refresh == b.Refresh && a.CacheDir == b.CacheDir &&
		a.ProxyURL == b.ProxyURL && a.UserAgent == b.UserAgent
}

// codexLiveStore returns the shared live catalog store, rebuilding it only
// when the relevant options changed so caches survive config reloads.
func (s *Service) codexLiveStore() *codexmodels.Store {
	if s == nil {
		return nil
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	opts := codexLiveModelsOptions(cfg)
	if !opts.Enabled {
		return nil
	}
	s.codexLiveMu.Lock()
	defer s.codexLiveMu.Unlock()
	if s.codexLive == nil || !sameCodexLiveModelsOptions(s.codexLive.Options(), opts.Normalized()) {
		s.codexLive = codexmodels.New(opts)
	}
	return s.codexLive
}

func codexLiveCredentials(auth *coreauth.Auth) codexmodels.Credentials {
	creds := codexmodels.Credentials{ProxyURL: auth.ProxyURL, Attributes: auth.Attributes}
	if auth.Metadata == nil {
		return creds
	}
	if v, ok := auth.Metadata["account_id"].(string); ok {
		creds.AccountID = strings.TrimSpace(v)
	}
	if creds.AccountID == "" {
		if v, ok := auth.Metadata["chatgpt_account_id"].(string); ok {
			creds.AccountID = strings.TrimSpace(v)
		}
	}
	if v, ok := auth.Metadata["access_token"].(string); ok {
		creds.AccessToken = strings.TrimSpace(v)
	}
	return creds
}

// applyCodexLiveModels merges the account's live Codex catalog into the static
// plan models. Failures leave the static list untouched.
func (s *Service) applyCodexLiveModels(ctx context.Context, auth *coreauth.Auth, static []*ModelInfo) []*ModelInfo {
	store := s.codexLiveStore()
	if store == nil || auth == nil {
		return static
	}
	creds := codexLiveCredentials(auth)
	if creds.AccountID == "" || creds.AccessToken == "" {
		return static
	}
	if token := s.refreshCodexLiveAccessToken(ctx, auth); token != "" {
		creds.AccessToken = token
	}
	entries, source := store.Catalog(ctx, creds)
	if len(entries) == 0 {
		return static
	}
	registry.SetCodexLiveClientModels(creds.AccountID, entries)
	merged := codexmodels.Merge(static, entries)
	summary := "added " + strings.Join(merged.Added, ",") + "; updated " + strings.Join(merged.Updated, ",")
	if s.noteCodexLiveSummary(creds.AccountID, source+"|"+summary) {
		log.Infof("codex live models: account %s (%s) %s", auth.ID, source, summary)
	}
	return merged.Models
}

// refreshCodexLiveAccessToken refreshes an expired Codex access token the same
// way the executor does and persists the result. It returns the fresh token
// or "" when no refresh was needed or possible.
func (s *Service) refreshCodexLiveAccessToken(ctx context.Context, auth *coreauth.Auth) string {
	if expiresAt, ok := auth.ExpirationTime(); !ok || time.Now().Add(codexLiveAccessTokenLeeway).Before(expiresAt) {
		return ""
	}
	refreshToken, _ := auth.Metadata["refresh_token"].(string)
	if strings.TrimSpace(refreshToken) == "" {
		return ""
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	svc := codexauth.NewCodexAuthWithProxyURL(cfg, auth.ProxyURL)
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, codexLiveTokenRefreshRetries)
	if err != nil || td == nil || strings.TrimSpace(td.AccessToken) == "" {
		log.Infof("codex live models: token refresh failed for %s: %v", auth.ID, err)
		return ""
	}
	updated := auth.Clone()
	updated.Metadata["id_token"] = td.IDToken
	updated.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		updated.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.AccountID != "" {
		updated.Metadata["account_id"] = td.AccountID
	}
	updated.Metadata["email"] = td.Email
	updated.Metadata["expired"] = td.Expire
	updated.Metadata["type"] = "codex"
	updated.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)
	if s.coreManager != nil {
		if _, errUpdate := s.coreManager.Update(ctx, updated); errUpdate != nil {
			log.Warnf("codex live models: cannot persist refreshed token for %s: %v", auth.ID, errUpdate)
		}
	}
	return td.AccessToken
}

func (s *Service) noteCodexLiveSummary(account, summary string) bool {
	s.codexLiveMu.Lock()
	defer s.codexLiveMu.Unlock()
	if s.codexLiveLogged == nil {
		s.codexLiveLogged = make(map[string]string)
	}
	if s.codexLiveLogged[account] == summary {
		return false
	}
	s.codexLiveLogged[account] = summary
	return true
}

// startCodexLiveModelsRefresh re-registers Codex OAuth auths whose live
// catalog is due for a refresh.
func (s *Service) startCodexLiveModelsRefresh(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(codexLiveModelsTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshStaleCodexLiveModels(ctx)
			}
		}
	}()
}

func (s *Service) refreshStaleCodexLiveModels(ctx context.Context) {
	store := s.codexLiveStore()
	if store == nil {
		return
	}
	byAccount := make(map[string][]*coreauth.Auth)
	for _, item := range s.coreManager.List() {
		if item == nil || item.Disabled || !strings.EqualFold(strings.TrimSpace(item.Provider), "codex") {
			continue
		}
		if item.AuthKind() == coreauth.AuthKindAPIKey {
			continue
		}
		creds := codexLiveCredentials(item)
		if creds.AccountID == "" || creds.AccessToken == "" {
			continue
		}
		byAccount[creds.AccountID] = append(byAccount[creds.AccountID], item)
	}
	accounts := make([]string, 0, len(byAccount))
	for account := range byAccount {
		accounts = append(accounts, account)
	}
	for _, account := range store.Stale(accounts) {
		for _, auth := range byAccount[account] {
			if ctx.Err() != nil {
				return
			}
			s.refreshModelRegistrationForAuth(auth)
		}
	}
}
