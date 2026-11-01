package clientusage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestStoreAddAggregatesPerKeyAndModel(t *testing.T) {
	s := NewStore()
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	s.Add(Event{APIKey: "key-a", Model: "gpt-5", At: at, Detail: coreusage.Detail{
		InputTokens: 100, OutputTokens: 20, ReasoningTokens: 5, CacheReadTokens: 40, TotalTokens: 120,
	}})
	s.Add(Event{APIKey: "key-a", Model: "gemini-3", At: at.Add(time.Minute), Failed: true})
	s.Add(Event{APIKey: " key-b ", Model: "gpt-5", At: at, Detail: coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
	s.Add(Event{APIKey: "", Model: "gpt-5", Detail: coreusage.Detail{TotalTokens: 999}})

	snap := s.Snapshot()
	if len(snap.Keys) != 2 {
		t.Fatalf("keys = %d, want 2", len(snap.Keys))
	}
	a := snap.Keys["key-a"]
	if a.Requests != 2 || a.Success != 1 || a.Failed != 1 {
		t.Fatalf("key-a counts = %+v", a.Totals)
	}
	if a.InputTokens != 100 || a.OutputTokens != 20 || a.ReasoningTokens != 5 || a.CachedTokens != 40 || a.TotalTokens != 120 {
		t.Fatalf("key-a tokens = %+v", a.Totals)
	}
	if !a.FirstUsedAt.Equal(at) || !a.LastUsedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("key-a times = %v %v", a.FirstUsedAt, a.LastUsedAt)
	}
	if a.Models["gpt-5"].TotalTokens != 120 || a.Models["gemini-3"].Failed != 1 {
		t.Fatalf("key-a models = %+v", a.Models)
	}
	if snap.Keys["key-b"].TotalTokens != 2 {
		t.Fatalf("key-b = %+v", snap.Keys["key-b"])
	}
}

func TestStoreSnapshotIsACopy(t *testing.T) {
	s := NewStore()
	s.Add(Event{APIKey: "k", Model: "m", Detail: coreusage.Detail{TotalTokens: 1}})
	snap := s.Snapshot()
	snap.Keys["k"].Models["m"] = Totals{TotalTokens: 50}
	if got := s.Snapshot().Keys["k"].Models["m"].TotalTokens; got != 1 {
		t.Fatalf("store changed through snapshot: %d", got)
	}
}

func TestStoreReset(t *testing.T) {
	s := NewStore()
	s.Add(Event{APIKey: "a", Model: "m"})
	s.Add(Event{APIKey: "b", Model: "m"})
	if s.Reset("missing") {
		t.Fatal("reset of missing key reported a removal")
	}
	if !s.Reset("a") {
		t.Fatal("reset of key a reported no removal")
	}
	if _, ok := s.Snapshot().Keys["a"]; ok {
		t.Fatal("key a still present")
	}
	if !s.Reset("") || len(s.Snapshot().Keys) != 0 {
		t.Fatal("reset all did not clear keys")
	}
}

func TestStoreSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateDirName, StateFileName)
	s := NewStore()
	s.Add(Event{APIKey: "k", Model: "m", Detail: coreusage.Detail{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}})
	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded := NewStore()
	loaded.Add(Event{APIKey: "k", Model: "m", Detail: coreusage.Detail{TotalTokens: 1}})
	if err := loaded.Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	got := loaded.Snapshot().Keys["k"]
	if got.Requests != 2 || got.TotalTokens != 8 || got.Models["m"].InputTokens != 3 {
		t.Fatalf("merged totals = %+v", got)
	}
	if loaded.Snapshot().Since.After(s.Snapshot().Since) {
		t.Fatal("since did not keep the older start time")
	}
}

func TestStoreLoadMissingFileIsNotAnError(t *testing.T) {
	if err := NewStore().Load(filepath.Join(t.TempDir(), "none.json")); err != nil {
		t.Fatalf("load missing: %v", err)
	}
}

func TestStoreSaveSkipsWhenClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	s := NewStore()
	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := NewStore().Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
}

func TestRunLoadsOnceAndSavesOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	seed := NewStore()
	seed.Add(Event{APIKey: "k", Model: "m", Detail: coreusage.Detail{TotalTokens: 10}})
	if err := seed.Save(path); err != nil {
		t.Fatal(err)
	}

	s := NewStore()
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Run(ctx, path, time.Hour); close(done) }()
		time.Sleep(50 * time.Millisecond)
		cancel()
		<-done
	}
	if got := s.Snapshot().Keys["k"].TotalTokens; got != 10 {
		t.Fatalf("total after two runs = %d, want 10", got)
	}

	s.Add(Event{APIKey: "k", Model: "m", Detail: coreusage.Detail{TotalTokens: 5}})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	check := NewStore()
	if err := check.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := check.Snapshot().Keys["k"].TotalTokens; got != 15 {
		t.Fatalf("saved total = %d, want 15", got)
	}
}

func TestStatePath(t *testing.T) {
	if StatePath("") != "" {
		t.Fatal("empty config path must disable persistence")
	}
	want := filepath.Join("cfg", StateDirName, StateFileName)
	if got := StatePath(filepath.Join("cfg", "config.yaml")); got != want {
		t.Fatalf("StatePath = %q, want %q", got, want)
	}
}

func TestPluginFollowsUsageStatisticsSwitch(t *testing.T) {
	prev := redisqueue.UsageStatisticsEnabled()
	t.Cleanup(func() { redisqueue.SetUsageStatisticsEnabled(prev) })

	p := &usagePlugin{store: NewStore()}
	record := coreusage.Record{
		APIKey: "client-key", Model: "gpt-5", Alias: "my-alias", Provider: "openai",
		Detail: coreusage.Detail{InputTokens: 10, OutputTokens: 2},
	}

	redisqueue.SetUsageStatisticsEnabled(false)
	p.HandleUsage(context.Background(), record)
	if len(p.store.Snapshot().Keys) != 0 {
		t.Fatal("plugin counted usage while statistics are disabled")
	}

	redisqueue.SetUsageStatisticsEnabled(true)
	p.HandleUsage(context.Background(), record)
	got := p.store.Snapshot().Keys["client-key"]
	if got.Requests != 1 || got.InputTokens != 10 || got.OutputTokens != 2 {
		t.Fatalf("plugin totals = %+v", got.Totals)
	}
	if got.TotalTokens == 0 {
		t.Fatal("total tokens were not derived from the breakdown")
	}
	if _, ok := got.Models["my-alias"]; !ok {
		t.Fatalf("models = %+v, want alias key", got.Models)
	}
}
