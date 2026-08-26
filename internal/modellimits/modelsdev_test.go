package modellimits

import "testing"

const testModelsDev = `{
 "alpha": {"id":"alpha","name":"Alpha","api":"https://alpha.example/v1","models":{"shared":{"limit":{"context":1000,"output":100}}}},
 "opencode": {"id":"opencode","name":"OpenCode Zen","api":"https://opencode.ai/zen/v1","models":{"shared":{"limit":{"context":2000,"output":200}}}},
 "opencode-go": {"id":"opencode-go","name":"OpenCode Go","api":"https://opencode.ai/zen/go/v1","models":{"shared":{"limit":{"context":3000,"output":300}},"ox-alpha-free":{"limit":{"context":1000000,"output":131072}}}},
 "zeta": {"id":"zeta","name":"Zeta","api":"https://zeta.example/v1","models":{"only-zeta":{"limit":{"context":4000,"output":400}},"empty":{"limit":{}}}}
}`

func mustCatalog(t *testing.T) *ModelsDevCatalog {
	t.Helper()
	catalog, err := ParseModelsDev([]byte(testModelsDev))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return catalog
}

func TestModelsDevLookup_PrefersBaseURLOverName(t *testing.T) {
	catalog := mustCatalog(t)
	limits, provider, ok := catalog.Lookup("shared", "opencode", "https://opencode.ai/zen/go/v1/")
	if !ok || provider != "opencode-go" || limits != (Limits{Context: 3000, Output: 300}) {
		t.Fatalf("got ok=%v provider=%q limits=%+v", ok, provider, limits)
	}
}

func TestModelsDevLookup_NameMatch(t *testing.T) {
	catalog := mustCatalog(t)
	limits, provider, ok := catalog.Lookup("shared", "OpenCode", "https://proxy.example/v1")
	if !ok || provider != "opencode" || limits.Context != 2000 {
		t.Fatalf("got ok=%v provider=%q limits=%+v", ok, provider, limits)
	}
}

func TestModelsDevLookup_FallbackFirstProvider(t *testing.T) {
	catalog := mustCatalog(t)
	limits, provider, ok := catalog.Lookup("shared", "unknown", "https://unknown.example/v1")
	if !ok || provider != "alpha" || limits.Context != 1000 {
		t.Fatalf("got ok=%v provider=%q limits=%+v", ok, provider, limits)
	}
	limits, provider, ok = catalog.Lookup("only-zeta", "", "")
	if !ok || provider != "zeta" || limits != (Limits{Context: 4000, Output: 400}) {
		t.Fatalf("got ok=%v provider=%q limits=%+v", ok, provider, limits)
	}
}

func TestModelsDevLookup_Missing(t *testing.T) {
	catalog := mustCatalog(t)
	if _, _, ok := catalog.Lookup("nope", "", ""); ok {
		t.Fatal("unknown model must not resolve")
	}
	if _, _, ok := catalog.Lookup("empty", "zeta", ""); ok {
		t.Fatal("model with empty limit must not resolve")
	}
	if _, _, ok := (*ModelsDevCatalog)(nil).Lookup("shared", "", ""); ok {
		t.Fatal("nil catalog must not resolve")
	}
}
