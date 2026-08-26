package codexmodels

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// failureRetry is how long a failed fetch is not retried for one account.
const failureRetry = 15 * time.Minute

// AccountCatalog is the cached live catalog for one account.
type AccountCatalog struct {
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Entry   `json:"models"`
}

type cacheFile struct {
	Accounts map[string]*AccountCatalog `json:"accounts"`
}

// Store keeps per-account live catalogs in memory and on disk.
type Store struct {
	opts Options

	mu       sync.Mutex
	loaded   bool
	accounts map[string]*AccountCatalog
	lastTry  map[string]time.Time
}

// New creates a store; the disk cache is read lazily on first use.
func New(opts Options) *Store {
	return &Store{
		opts:     opts.Normalized(),
		accounts: make(map[string]*AccountCatalog),
		lastTry:  make(map[string]time.Time),
	}
}

// Options returns the effective options.
func (s *Store) Options() Options {
	if s == nil {
		return Options{}
	}
	return s.opts
}

// Catalog returns the live catalog for creds.AccountID, fetching upstream when
// the cached copy is older than the refresh interval. The returned source is
// "live", "cache", or "" when nothing is known for the account.
func (s *Store) Catalog(ctx context.Context, creds Credentials) ([]Entry, string) {
	if s == nil || !s.opts.Enabled {
		return nil, ""
	}
	account := strings.TrimSpace(creds.AccountID)
	if account == "" {
		return nil, ""
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadDiskLocked()

	cached := s.accounts[account]
	now := time.Now()
	fresh := cached != nil && now.Sub(cached.FetchedAt) < s.opts.Refresh
	if fresh || now.Sub(s.lastTry[account]) < failureRetry {
		return cloneEntries(cached), sourceFor(cached)
	}
	s.lastTry[account] = now

	entries, err := Fetch(ctx, s.opts, creds)
	if err != nil {
		log.Infof("codex live models: fetch failed for account %s (%v); using %s", shortAccount(account), err, sourceOrNothing(cached))
		return cloneEntries(cached), sourceFor(cached)
	}
	s.accounts[account] = &AccountCatalog{FetchedAt: now, Models: entries}
	s.saveDiskLocked()
	log.Infof("codex live models: fetched %d models for account %s", len(entries), shortAccount(account))
	return cloneEntries(s.accounts[account]), "live"
}

// Stale reports the accounts whose catalog is due for a refresh attempt.
func (s *Store) Stale(accounts []string) []string {
	if s == nil || !s.opts.Enabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadDiskLocked()
	now := time.Now()
	stale := make([]string, 0, len(accounts))
	for _, account := range accounts {
		account = strings.TrimSpace(account)
		if account == "" {
			continue
		}
		cached := s.accounts[account]
		if cached != nil && now.Sub(cached.FetchedAt) < s.opts.Refresh {
			continue
		}
		if now.Sub(s.lastTry[account]) < failureRetry {
			continue
		}
		stale = append(stale, account)
	}
	sort.Strings(stale)
	return stale
}

func (s *Store) cachePath() string {
	if strings.TrimSpace(s.opts.CacheDir) == "" {
		return ""
	}
	return filepath.Join(s.opts.CacheDir, CacheFile)
}

func (s *Store) loadDiskLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	path := s.cachePath()
	if path == "" {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file cacheFile
	if err = json.Unmarshal(body, &file); err != nil {
		log.Warnf("codex live models: ignoring unreadable cache %s: %v", path, err)
		return
	}
	for account, catalog := range file.Accounts {
		if catalog == nil || len(catalog.Models) == 0 {
			continue
		}
		s.accounts[account] = catalog
	}
	log.Infof("codex live models: restored cache from %s (%d accounts)", path, len(s.accounts))
}

func (s *Store) saveDiskLocked() {
	path := s.cachePath()
	if path == "" {
		return
	}
	body, err := json.Marshal(cacheFile{Accounts: s.accounts})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, body, 0o600); err != nil {
		log.Warnf("codex live models: cannot write cache: %v", err)
		return
	}
	if err = os.Rename(tmp, path); err != nil {
		log.Warnf("codex live models: cannot persist cache: %v", err)
	}
}

func cloneEntries(catalog *AccountCatalog) []Entry {
	if catalog == nil {
		return nil
	}
	raw, err := json.Marshal(catalog.Models)
	if err != nil {
		return nil
	}
	var cloned []Entry
	if err = json.Unmarshal(raw, &cloned); err != nil {
		return nil
	}
	return cloned
}

func sourceFor(catalog *AccountCatalog) string {
	if catalog == nil {
		return ""
	}
	return "cache"
}

func sourceOrNothing(catalog *AccountCatalog) string {
	if catalog == nil {
		return "static models only"
	}
	return "cached catalog from " + catalog.FetchedAt.UTC().Format(time.RFC3339)
}

func shortAccount(account string) string {
	if len(account) <= 8 {
		return account
	}
	return account[:8]
}
