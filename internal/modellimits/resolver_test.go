package modellimits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestResolver(t *testing.T, upstream, modelsDev string, cacheDir string) (*Resolver, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if upstream == "" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(upstream))
	})
	mux.HandleFunc("/api.json", func(w http.ResponseWriter, _ *http.Request) {
		if modelsDev == "" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(modelsDev))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	resolver := New(Options{
		Enabled:      true,
		ModelsDevURL: server.URL + "/api.json",
		CacheDir:     cacheDir,
		Timeout:      2 * time.Second,
		HTTPClient:   server.Client(),
	})
	return resolver, server.URL + "/v1"
}

func TestResolve_UpstreamBeatsModelsDev(t *testing.T) {
	resolver, baseURL := newTestResolver(t,
		`{"data":[{"id":"shared","max_model_len":262144}]}`,
		testModelsDev, "")
	spec := ProviderSpec{Name: "opencode-go", BaseURL: baseURL, APIKey: "secret", Models: []string{"shared", "ox-alpha-free", "unknown"}}
	got := resolver.Resolve(context.Background(), spec)
	if r := got["shared"]; r.Source != SourceUpstream || r.Context != 262144 {
		t.Fatalf("shared = %+v, want upstream 262144", r)
	}
	if r := got["ox-alpha-free"]; r.Source != SourceModelsDev || r.Provider != "opencode-go" || r.Context != 1000000 || r.Output != 131072 {
		t.Fatalf("ox-alpha-free = %+v, want models.dev opencode-go 1000000/131072", r)
	}
	if _, ok := got["unknown"]; ok {
		t.Fatal("unknown model must stay unresolved")
	}
}

func TestResolve_DisabledReturnsNil(t *testing.T) {
	resolver := New(Options{Enabled: false})
	if got := resolver.Resolve(context.Background(), ProviderSpec{BaseURL: "http://127.0.0.1:1", Models: []string{"x"}}); got != nil {
		t.Fatalf("disabled resolver returned %v", got)
	}
}

func TestResolve_ModelsDevDiskCacheSurvivesOutage(t *testing.T) {
	dir := t.TempDir()
	first, firstURL := newTestResolver(t, "", testModelsDev, dir)
	spec := ProviderSpec{Name: "x", BaseURL: firstURL, Models: []string{"only-zeta"}}
	if got := first.Resolve(context.Background(), spec); got["only-zeta"].Context != 4000 {
		t.Fatalf("first resolve = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, modelsDevCacheFile)); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	second, secondURL := newTestResolver(t, "", "", dir)
	spec.BaseURL = secondURL
	if got := second.Resolve(context.Background(), spec); got["only-zeta"].Context != 4000 {
		t.Fatalf("offline resolve from disk cache = %+v", got)
	}
}

func TestResolve_UpstreamFailureFallsBackToModelsDev(t *testing.T) {
	resolver, baseURL := newTestResolver(t, "", testModelsDev, "")
	spec := ProviderSpec{Name: "zeta", BaseURL: baseURL, Models: []string{"only-zeta"}}
	if got := resolver.Resolve(context.Background(), spec); got["only-zeta"].Source != SourceModelsDev {
		t.Fatalf("got %+v", got)
	}
}
