// Package codexmodels fetches the live Codex model catalog for a ChatGPT
// OAuth account and merges it with the static Codex model definitions.
package codexmodels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const (
	// DefaultRefresh is how often each account's live catalog is re-fetched.
	DefaultRefresh = 6 * time.Hour
	// DefaultTimeout bounds one catalog fetch.
	DefaultTimeout = 8 * time.Second
	// DefaultClientVersion is the client_version query value sent upstream.
	DefaultClientVersion = "0.146.0"
	// DefaultUserAgent mirrors the Codex executor's upstream User-Agent.
	DefaultUserAgent = "codex-tui/0.146.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.146.0)"
	// DefaultOriginator mirrors the Codex executor's Originator header.
	DefaultOriginator = "codex-tui"
	// CacheFile is the per-account catalog cache written under the auth dir.
	CacheFile = "codex-models-cache.json"

	modelsURL = "https://chatgpt.com/backend-api/codex/models"
)

// Options configures live catalog fetching.
type Options struct {
	Enabled       bool
	Refresh       time.Duration
	Timeout       time.Duration
	CacheDir      string
	ProxyURL      string
	ClientVersion string
	UserAgent     string
	Originator    string
}

// Normalized returns o with defaults filled in.
func (o Options) Normalized() Options {
	if o.Refresh <= 0 {
		o.Refresh = DefaultRefresh
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if strings.TrimSpace(o.ClientVersion) == "" {
		o.ClientVersion = DefaultClientVersion
	}
	if strings.TrimSpace(o.UserAgent) == "" {
		o.UserAgent = DefaultUserAgent
	}
	if strings.TrimSpace(o.Originator) == "" {
		o.Originator = DefaultOriginator
	}
	return o
}

// Entry is one upstream Codex catalog model in its raw form.
type Entry = map[string]any

// Credentials identifies one Codex OAuth account for a catalog fetch.
type Credentials struct {
	AccountID   string
	AccessToken string
	ProxyURL    string
	Attributes  map[string]string
}

type catalogPayload struct {
	Models []Entry `json:"models"`
}

// Parse decodes an upstream catalog payload, dropping entries without a slug.
func Parse(raw []byte) ([]Entry, error) {
	var payload catalogPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode codex catalog: %w", err)
	}
	if payload.Models == nil {
		return nil, fmt.Errorf("codex catalog has no models array")
	}
	entries := make([]Entry, 0, len(payload.Models))
	for _, entry := range payload.Models {
		if Slug(entry) == "" {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Slug returns the trimmed model slug of an entry.
func Slug(entry Entry) string {
	return stringValue(entry, "slug")
}

// Visibility returns the entry visibility ("list", "hide", ...).
func Visibility(entry Entry) string {
	return stringValue(entry, "visibility")
}

// Fetch downloads the live catalog for one account.
func Fetch(ctx context.Context, opts Options, creds Credentials) ([]Entry, error) {
	opts = opts.Normalized()
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("missing access token")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	u, err := url.Parse(modelsURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("client_version", opts.ClientVersion)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Close = true
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(creds.AccessToken))
	req.Header.Set("User-Agent", opts.UserAgent)
	req.Header.Set("Originator", opts.Originator)
	if accountID := strings.TrimSpace(creds.AccountID); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	util.ApplyCustomHeadersFromAttrs(req, creds.Attributes)

	client := &http.Client{Timeout: opts.Timeout}
	proxyURL := strings.TrimSpace(creds.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(opts.ProxyURL)
	}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(proxyURL); errProxy == nil && transport != nil {
		client.Transport = transport
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("codex models request failed with status %d", resp.StatusCode)
	}
	return Parse(body)
}

// MergeResult describes how a live catalog changed the static model list.
type MergeResult struct {
	Models  []*registry.ModelInfo
	Added   []string
	Updated []string
}

// Merge unions static models with live entries. Live entries add unknown
// slugs and refresh display name, context length, and reasoning levels on
// known ones; static models absent from the live catalog are kept.
func Merge(static []*registry.ModelInfo, live []Entry) MergeResult {
	result := MergeResult{Models: make([]*registry.ModelInfo, 0, len(static)+len(live))}
	index := make(map[string]int, len(static))
	var created int64
	for _, model := range static {
		if model == nil || model.ID == "" {
			continue
		}
		index[model.ID] = len(result.Models)
		result.Models = append(result.Models, model)
		if model.Created > created {
			created = model.Created
		}
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	for _, entry := range live {
		slug := Slug(entry)
		if slug == "" {
			continue
		}
		if pos, ok := index[slug]; ok {
			updated := applyEntry(cloneModelInfo(result.Models[pos]), entry)
			if !sameModelInfo(result.Models[pos], updated) {
				result.Models[pos] = updated
				result.Updated = append(result.Updated, slug)
			}
			continue
		}
		model := applyEntry(&registry.ModelInfo{
			ID:                        slug,
			Object:                    "model",
			Created:                   created,
			OwnedBy:                   "openai",
			Type:                      "openai",
			SupportedParameters:       []string{"tools"},
			SupportedOutputModalities: []string{"text"},
		}, entry)
		index[slug] = len(result.Models)
		result.Models = append(result.Models, model)
		result.Added = append(result.Added, slug)
	}
	sort.Strings(result.Added)
	sort.Strings(result.Updated)
	return result
}

func applyEntry(model *registry.ModelInfo, entry Entry) *registry.ModelInfo {
	if name := stringValue(entry, "display_name"); name != "" {
		model.DisplayName = name
	}
	if model.Description == "" {
		model.Description = stringValue(entry, "description")
	}
	if ctxLen := intValue(entry, "context_window"); ctxLen > 0 {
		model.ContextLength = ctxLen
	}
	if levels := reasoningLevels(entry); len(levels) > 0 {
		model.Thinking = &registry.ThinkingSupport{Levels: levels}
	}
	if modalities := stringSlice(entry, "input_modalities"); len(modalities) > 0 {
		model.SupportedInputModalities = modalities
	} else if len(model.SupportedInputModalities) == 0 {
		model.SupportedInputModalities = []string{"text"}
	}
	if model.MaxCompletionTokens == 0 {
		model.MaxCompletionTokens = 128000
	}
	return model
}

func reasoningLevels(entry Entry) []string {
	raw, ok := entry["supported_reasoning_levels"].([]any)
	if !ok {
		return nil
	}
	levels := make([]string, 0, len(raw))
	for _, item := range raw {
		level, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if effort := stringValue(level, "effort"); effort != "" {
			levels = append(levels, strings.ToLower(effort))
		}
	}
	return levels
}

func cloneModelInfo(model *registry.ModelInfo) *registry.ModelInfo {
	cloned := *model
	if model.Thinking != nil {
		thinking := *model.Thinking
		thinking.Levels = append([]string(nil), model.Thinking.Levels...)
		cloned.Thinking = &thinking
	}
	cloned.SupportedInputModalities = append([]string(nil), model.SupportedInputModalities...)
	return &cloned
}

func sameModelInfo(a, b *registry.ModelInfo) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(left) == string(right)
}

func stringValue(entry Entry, key string) string {
	if entry == nil {
		return ""
	}
	if s, ok := entry[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func intValue(entry Entry, key string) int {
	if entry == nil {
		return 0
	}
	switch v := entry[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

func stringSlice(entry Entry, key string) []string {
	raw, ok := entry[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}
