package modellimits

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultTimeout bounds every network fetch performed by the resolver.
	DefaultTimeout = 3 * time.Second
	// DefaultModelsDevRefresh is how often the models.dev catalog is re-fetched.
	DefaultModelsDevRefresh = 24 * time.Hour
	// UpstreamRefresh is how long a provider /models result is reused across
	// reloads and how often auto-discovered models are re-checked.
	UpstreamRefresh = 10 * time.Minute
	// upstreamFailureTTL is how long a failed provider fetch is not retried.
	upstreamFailureTTL = 1 * time.Minute
	modelsDevCacheFile = "models-dev-cache.json"
	maxPayloadBytes    = 32 << 20
)

// Options configures a Resolver.
type Options struct {
	// Enabled toggles all automatic resolution. When false Resolve returns nil.
	Enabled bool
	// ModelsDevURL overrides DefaultModelsDevURL. Empty disables models.dev.
	ModelsDevURL string
	// ModelsDevRefresh overrides DefaultModelsDevRefresh.
	ModelsDevRefresh time.Duration
	// CacheDir is where the models.dev payload is persisted (auth dir). Empty disables disk cache.
	CacheDir string
	// ProxyURL is the global outbound proxy setting.
	ProxyURL string
	// Timeout bounds each fetch. Zero uses DefaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the client built from ProxyURL (tests).
	HTTPClient *http.Client
}

// ProviderSpec describes one openai-compatibility provider to resolve.
type ProviderSpec struct {
	Name    string
	BaseURL string
	APIKey  string
	Headers map[string]string
	// Models lists upstream model names (config `name`, not alias).
	Models []string
}

type upstreamEntry struct {
	limits    map[string]Limits
	ids       []string
	fetchedAt time.Time
	err       error
}

// Resolver caches upstream and models.dev limits across config reloads.
type Resolver struct {
	opts   Options
	client *http.Client

	mu       sync.Mutex
	upstream map[string]*upstreamEntry

	mdMu        sync.Mutex
	mdCatalog   *ModelsDevCatalog
	mdFetchedAt time.Time
	mdLastTry   time.Time
}

// New builds a Resolver from options.
func New(opts Options) *Resolver {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.ModelsDevRefresh <= 0 {
		opts.ModelsDevRefresh = DefaultModelsDevRefresh
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: opts.Timeout}
		if transport, _, err := proxyutil.BuildHTTPTransport(opts.ProxyURL); err == nil && transport != nil {
			client.Transport = transport
		}
	}
	return &Resolver{opts: opts, client: client, upstream: make(map[string]*upstreamEntry)}
}

// Options returns the options the resolver was built with.
func (r *Resolver) Options() Options {
	if r == nil {
		return Options{}
	}
	return r.opts
}

// Prefetch warms the upstream and models.dev caches for all specs in parallel,
// waiting at most one timeout so registration is never delayed by more than that.
// Upstream catalogs are fetched even when limit resolution is disabled so
// model discovery can use them; models.dev is only fetched when enabled.
func (r *Resolver) Prefetch(ctx context.Context, specs []ProviderSpec) {
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, spec := range specs {
		if strings.TrimSpace(spec.BaseURL) == "" {
			continue
		}
		wg.Add(1)
		go func(spec ProviderSpec) {
			defer wg.Done()
			r.upstreamCatalog(ctx, spec)
		}(spec)
	}
	if r.opts.Enabled && r.opts.ModelsDevURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.modelsDevCatalog(ctx)
		}()
	}
	wg.Wait()
}

// Resolve returns limits keyed by upstream model name for every model in spec
// that could be resolved. It never returns an error: unresolved models are absent.
func (r *Resolver) Resolve(ctx context.Context, spec ProviderSpec) map[string]Resolved {
	if r == nil || !r.opts.Enabled || len(spec.Models) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	upstream := r.upstreamCatalog(ctx, spec).limits
	var catalog *ModelsDevCatalog
	out := make(map[string]Resolved, len(spec.Models))
	for _, model := range spec.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if limits, ok := upstream[model]; ok {
			out[model] = Resolved{Limits: limits, Source: SourceUpstream, Provider: spec.Name}
			continue
		}
		if catalog == nil && r.opts.ModelsDevURL != "" {
			catalog = r.modelsDevCatalog(ctx)
		}
		if limits, provider, ok := catalog.Lookup(model, spec.Name, spec.BaseURL); ok {
			out[model] = Resolved{Limits: limits, Source: SourceModelsDev, Provider: provider}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// UpstreamModels returns the model ids advertised by the provider's GET
// {base-url}/models, reusing the same cached fetch as limit resolution. The
// last successful list is kept while the upstream is unavailable; nil means
// the catalog was never fetched successfully.
func (r *Resolver) UpstreamModels(ctx context.Context, spec ProviderSpec) []string {
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	entry := r.upstreamCatalog(ctx, spec)
	if entry.ids == nil {
		return nil
	}
	return append([]string(nil), entry.ids...)
}

// UpstreamStale reports whether the cached catalog for spec is older than the
// upstream cache TTL (or absent), i.e. the next lookup would refetch.
func (r *Resolver) UpstreamStale(spec ProviderSpec) bool {
	if r == nil {
		return false
	}
	baseURL := strings.TrimRight(strings.TrimSpace(spec.BaseURL), "/")
	if baseURL == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.upstream[baseURL+"\x00"+spec.APIKey]
	return entry == nil || time.Since(entry.fetchedAt) >= UpstreamRefresh
}

func (r *Resolver) upstreamCatalog(ctx context.Context, spec ProviderSpec) upstreamEntry {
	baseURL := strings.TrimRight(strings.TrimSpace(spec.BaseURL), "/")
	if baseURL == "" {
		return upstreamEntry{}
	}
	key := baseURL + "\x00" + spec.APIKey
	r.mu.Lock()
	entry := r.upstream[key]
	if entry != nil {
		ttl := UpstreamRefresh
		if entry.err != nil {
			ttl = upstreamFailureTTL
		}
		if time.Since(entry.fetchedAt) < ttl {
			r.mu.Unlock()
			return *entry
		}
	}
	r.mu.Unlock()

	catalog, err := r.fetchUpstream(ctx, baseURL, spec)
	if err != nil {
		log.Infof("model limits: provider %q upstream /models unavailable: %v", spec.Name, err)
	} else if len(catalog.Limits) == 0 {
		log.Infof("model limits: provider %q upstream /models carries no limit fields", spec.Name)
	}
	next := &upstreamEntry{fetchedAt: time.Now(), err: err}
	if err == nil {
		next.limits = catalog.Limits
		next.ids = catalog.IDs
	} else if entry != nil {
		next.limits = entry.limits
		next.ids = entry.ids
	}
	r.mu.Lock()
	r.upstream[key] = next
	r.mu.Unlock()
	return *next
}

func (r *Resolver) fetchUpstream(ctx context.Context, baseURL string, spec ProviderSpec) (*UpstreamCatalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(spec.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range spec.Headers {
		if strings.TrimSpace(name) != "" {
			req.Header.Set(name, value)
		}
	}
	body, err := r.do(req)
	if err != nil {
		return nil, err
	}
	catalog := ParseUpstreamCatalog(body)
	if catalog == nil {
		return nil, errors.New("invalid /models payload")
	}
	return catalog, nil
}

func (r *Resolver) do(req *http.Request) ([]byte, error) {
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPayloadBytes))
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (r *Resolver) cachePath() string {
	if r.opts.CacheDir == "" {
		return ""
	}
	return filepath.Join(r.opts.CacheDir, modelsDevCacheFile)
}

func (r *Resolver) modelsDevCatalog(ctx context.Context) *ModelsDevCatalog {
	r.mdMu.Lock()
	defer r.mdMu.Unlock()

	if r.mdCatalog == nil {
		r.loadModelsDevDisk()
	}
	fresh := r.mdCatalog != nil && time.Since(r.mdFetchedAt) < r.opts.ModelsDevRefresh
	if fresh || time.Since(r.mdLastTry) < upstreamFailureTTL {
		return r.mdCatalog
	}
	r.mdLastTry = time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.opts.ModelsDevURL, nil)
	if err != nil {
		return r.mdCatalog
	}
	req.Header.Set("Accept", "application/json")
	body, err := r.do(req)
	if err != nil {
		log.Infof("model limits: models.dev fetch failed (%v); using cached catalog with %d providers", err, r.mdCatalog.Len())
		return r.mdCatalog
	}
	catalog, err := ParseModelsDev(body)
	if err != nil || catalog.Len() == 0 {
		log.Warnf("model limits: models.dev payload invalid: %v", errors.Join(err, errNoProviders(catalog)))
		return r.mdCatalog
	}
	r.mdCatalog = catalog
	r.mdFetchedAt = time.Now()
	r.saveModelsDevDisk(body)
	log.Infof("model limits: models.dev catalog loaded (%d providers)", catalog.Len())
	return r.mdCatalog
}

func errNoProviders(catalog *ModelsDevCatalog) error {
	if catalog != nil && catalog.Len() == 0 {
		return errors.New("no providers")
	}
	return nil
}

func (r *Resolver) loadModelsDevDisk() {
	path := r.cachePath()
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	catalog, err := ParseModelsDev(body)
	if err != nil || catalog.Len() == 0 {
		return
	}
	r.mdCatalog = catalog
	r.mdFetchedAt = info.ModTime()
	log.Infof("model limits: models.dev catalog restored from %s (%d providers, saved %s)", path, catalog.Len(), info.ModTime().UTC().Format(time.RFC3339))
}

func (r *Resolver) saveModelsDevDisk(body []byte) {
	path := r.cachePath()
	if path == "" {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		log.Warnf("model limits: cannot write models.dev cache: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Warnf("model limits: cannot persist models.dev cache: %v", err)
	}
}

// Describe renders resolved limits as a stable, log-friendly string.
func Describe(resolved map[string]Resolved) string {
	if len(resolved) == 0 {
		return ""
	}
	keys := make([]string, 0, len(resolved))
	for k := range resolved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := resolved[k]
		parts = append(parts, fmt.Sprintf("%s ctx=%d out=%d (%s:%s)", k, v.Context, v.Output, v.Source, v.Provider))
	}
	return strings.Join(parts, "; ")
}
