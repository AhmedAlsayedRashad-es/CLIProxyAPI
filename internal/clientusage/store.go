// Package clientusage aggregates token usage per client API key, the keys that
// clients send to the proxy, and optionally persists the totals to disk.
package clientusage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	snapshotVersion = 1
	// DefaultSaveInterval is how often dirty totals are written to disk.
	DefaultSaveInterval = 30 * time.Second
	// StateDirName is the folder, next to the config file, that holds the state file.
	// A dedicated folder keeps the file out of the auth directory watcher even when
	// the auth directory and the config directory are the same.
	StateDirName = "usage-stats"
	// StateFileName is the state file name inside StateDirName.
	StateFileName = "client-keys.json"
)

// Totals holds request and token counters.
type Totals struct {
	Requests        int64     `json:"requests"`
	Success         int64     `json:"success"`
	Failed          int64     `json:"failed"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CachedTokens    int64     `json:"cached_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
	FirstUsedAt     time.Time `json:"first_used_at"`
	LastUsedAt      time.Time `json:"last_used_at"`
}

// KeyUsage holds the totals of one client API key and a breakdown per model.
type KeyUsage struct {
	Totals
	Models map[string]Totals `json:"models"`
}

// Snapshot is a point-in-time copy of all totals.
type Snapshot struct {
	Version int                 `json:"version"`
	Since   time.Time           `json:"since"`
	Keys    map[string]KeyUsage `json:"keys"`
}

// Event is one usage record reduced to the fields the store counts.
type Event struct {
	APIKey string
	Model  string
	Failed bool
	At     time.Time
	Detail coreusage.Detail
}

// Store aggregates usage per client API key. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	since   time.Time
	keys    map[string]*KeyUsage
	dirty   bool
	path    string
	saveMu  sync.Mutex
	nowFunc func() time.Time
}

// NewStore returns an empty store.
func NewStore() *Store {
	s := &Store{keys: make(map[string]*KeyUsage), nowFunc: time.Now}
	s.since = s.nowFunc().UTC()
	return s
}

var defaultStore = NewStore()

// Default returns the process-wide store fed by the usage plugin.
func Default() *Store { return defaultStore }

// StatePath returns the state file path for a config file path, or "" when the
// config path is empty.
func StatePath(configPath string) string {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), StateDirName, StateFileName)
}

func addTotals(t *Totals, ev Event) {
	t.Requests++
	if ev.Failed {
		t.Failed++
	} else {
		t.Success++
	}
	t.InputTokens += ev.Detail.InputTokens
	t.OutputTokens += ev.Detail.OutputTokens
	t.ReasoningTokens += ev.Detail.ReasoningTokens
	t.CachedTokens += cachedTokens(ev.Detail)
	t.TotalTokens += ev.Detail.TotalTokens
	if t.FirstUsedAt.IsZero() || ev.At.Before(t.FirstUsedAt) {
		t.FirstUsedAt = ev.At
	}
	if ev.At.After(t.LastUsedAt) {
		t.LastUsedAt = ev.At
	}
}

// cachedTokens prefers the explicit cache-read counter and falls back to the
// legacy cached counter so a single value is not counted twice.
func cachedTokens(d coreusage.Detail) int64 {
	if d.CacheReadTokens > 0 {
		return d.CacheReadTokens
	}
	return d.CachedTokens
}

// Add records one usage event. Events without an API key are ignored.
func (s *Store) Add(ev Event) {
	key := strings.TrimSpace(ev.APIKey)
	if s == nil || key == "" {
		return
	}
	if ev.At.IsZero() {
		ev.At = s.nowFunc()
	}
	ev.At = ev.At.UTC()
	model := strings.TrimSpace(ev.Model)
	if model == "" {
		model = "unknown"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.keys[key]
	if !ok {
		entry = &KeyUsage{Models: make(map[string]Totals)}
		s.keys[key] = entry
	}
	addTotals(&entry.Totals, ev)
	modelTotals := entry.Models[model]
	addTotals(&modelTotals, ev)
	entry.Models[model] = modelTotals
	s.dirty = true
}

// Snapshot returns a deep copy of the current totals.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() Snapshot {
	out := Snapshot{Version: snapshotVersion, Since: s.since, Keys: make(map[string]KeyUsage, len(s.keys))}
	for key, entry := range s.keys {
		models := make(map[string]Totals, len(entry.Models))
		for model, totals := range entry.Models {
			models[model] = totals
		}
		out.Keys[key] = KeyUsage{Totals: entry.Totals, Models: models}
	}
	return out
}

// Reset clears the totals of one key, or of every key when key is empty.
// It reports whether anything was removed.
func (s *Store) Reset(key string) bool {
	key = strings.TrimSpace(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		removed := len(s.keys) > 0
		s.keys = make(map[string]*KeyUsage)
		s.since = s.nowFunc().UTC()
		s.dirty = true
		return removed
	}
	if _, ok := s.keys[key]; !ok {
		return false
	}
	delete(s.keys, key)
	s.dirty = true
	return true
}

// Load replaces the in-memory totals with the state file content. A missing
// file is not an error. Events recorded before Load are kept and merged.
func (s *Store) Load(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("clientusage: read state: %w", err)
	}
	var snap Snapshot
	if err = json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("clientusage: parse state: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !snap.Since.IsZero() && snap.Since.Before(s.since) {
		s.since = snap.Since.UTC()
	}
	for key, loaded := range snap.Keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		entry, ok := s.keys[key]
		if !ok {
			entry = &KeyUsage{Models: make(map[string]Totals)}
			s.keys[key] = entry
		}
		mergeTotals(&entry.Totals, loaded.Totals)
		for model, totals := range loaded.Models {
			current := entry.Models[model]
			mergeTotals(&current, totals)
			entry.Models[model] = current
		}
	}
	return nil
}

func mergeTotals(dst *Totals, src Totals) {
	dst.Requests += src.Requests
	dst.Success += src.Success
	dst.Failed += src.Failed
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.ReasoningTokens += src.ReasoningTokens
	dst.CachedTokens += src.CachedTokens
	dst.TotalTokens += src.TotalTokens
	if !src.FirstUsedAt.IsZero() && (dst.FirstUsedAt.IsZero() || src.FirstUsedAt.Before(dst.FirstUsedAt)) {
		dst.FirstUsedAt = src.FirstUsedAt
	}
	if src.LastUsedAt.After(dst.LastUsedAt) {
		dst.LastUsedAt = src.LastUsedAt
	}
}

// Save writes the totals to path when they changed since the last save.
// The write goes to a temporary file first and then replaces the state file.
func (s *Store) Save(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	snap := s.snapshotLocked()
	s.dirty = false
	s.mu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err == nil {
		err = writeFileAtomic(path, data)
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("clientusage: save state: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Run loads the state file, saves dirty totals every interval, and saves a
// last time when ctx ends. It blocks until ctx ends. An empty path disables
// persistence.
func (s *Store) Run(ctx context.Context, path string, interval time.Duration) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	if interval <= 0 {
		interval = DefaultSaveInterval
	}
	s.mu.Lock()
	alreadyLoaded := s.path == path
	s.path = path
	s.mu.Unlock()
	// Load merges into memory, so a second Run in the same process (for example
	// after a service restart) must not load the same file again.
	if !alreadyLoaded {
		if err := s.Load(path); err != nil {
			log.Warnf("client key usage: %v", err)
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.Save(path); err != nil {
				log.Warnf("client key usage: %v", err)
			}
			return
		case <-ticker.C:
			if err := s.Save(path); err != nil {
				log.Warnf("client key usage: %v", err)
			}
		}
	}
}

// Flush saves dirty totals to the path given to Run, if any.
func (s *Store) Flush() error {
	s.mu.Lock()
	path := s.path
	s.mu.Unlock()
	return s.Save(path)
}
