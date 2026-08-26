package cliproxy

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexLiveModelsOptions(t *testing.T) {
	if !codexLiveModelsOptions(&config.Config{}).Enabled {
		t.Fatal("codex-live-models must default to enabled")
	}
	off := false
	opts := codexLiveModelsOptions(&config.Config{CodexLiveModels: &off, CodexLiveModelsRefresh: "2h"})
	if opts.Enabled {
		t.Fatal("codex-live-models: false must disable")
	}
	if opts.Refresh != 2*time.Hour {
		t.Fatalf("refresh = %s, want 2h", opts.Refresh)
	}
	if codexLiveModelsOptions(&config.Config{CodexLiveModelsRefresh: "bogus"}).Refresh != 6*time.Hour {
		t.Fatal("invalid refresh must fall back to 6h")
	}
}

func TestApplyCodexLiveModelsDisabledKeepsStatic(t *testing.T) {
	off := false
	s := &Service{cfg: &config.Config{CodexLiveModels: &off}}
	static := []*ModelInfo{{ID: "gpt-5.5"}}
	auth := &coreauth.Auth{ID: "codex-1", Provider: "codex", Metadata: map[string]any{"account_id": "acct", "access_token": "tok"}}
	got := s.applyCodexLiveModels(nil, auth, static)
	if len(got) != 1 || got[0] != static[0] {
		t.Fatalf("flag off must return static models unchanged, got %+v", got)
	}
	if live, _ := registry.GetCodexLiveClientModelsSnapshot(); len(live) != 0 {
		t.Fatalf("flag off must not populate the live overlay, got %v", live)
	}
}
