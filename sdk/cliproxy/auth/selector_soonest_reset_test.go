package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestNextQuotaReset(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	earlier, later := now.Add(time.Hour), now.Add(24*time.Hour)
	unix := func(value time.Time) string { return strconv.FormatInt(value.Unix(), 10) }
	for _, tc := range []struct {
		name, provider string
		observed       time.Time
		signals        map[string]string
		want           time.Time
	}{
		{"claude windows", "claude", now, map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": unix(later), "Anthropic-Ratelimit-Unified-7d-Reset": unix(earlier)}, earlier},
		{"claude aggregate", "claude", now, map[string]string{"Anthropic-Ratelimit-Unified-Reset": unix(earlier)}, earlier},
		{"codex windows", "codex", now, map[string]string{"X-Codex-Primary-Reset-At": unix(later), "X-Codex-Secondary-Reset-At": unix(earlier)}, earlier},
		{"codex relative anchored to observation", "codex", now.Add(-time.Hour), map[string]string{"X-Codex-Primary-Reset-After-Seconds": "7200"}, earlier},
		{"codex absolute wins over relative", "codex", now, map[string]string{"X-Codex-Primary-Reset-At": unix(later), "X-Codex-Primary-Reset-After-Seconds": "3600"}, later},
		{"codex expired absolute does not restart window", "codex", now, map[string]string{"X-Codex-Primary-Reset-At": unix(now), "X-Codex-Primary-Reset-After-Seconds": "3600"}, time.Time{}},
		{"codex relative needs observation", "codex", time.Time{}, map[string]string{"X-Codex-Primary-Reset-After-Seconds": "3600"}, time.Time{}},
		{"codex expired relative", "codex", now.Add(-2 * time.Hour), map[string]string{"X-Codex-Primary-Reset-After-Seconds": "3600"}, time.Time{}},
		{"codex relative overflow", "codex", now, map[string]string{"X-Codex-Primary-Reset-After-Seconds": "9223372036854775807"}, time.Time{}},
		{"ignore unrelated limits", "codex", now, map[string]string{"X-Codex-Code-Review-Primary-Reset-At": unix(earlier), "X-Codex-Additional-Spark-Primary-Reset-At": unix(earlier), "Retry-After": "3600"}, time.Time{}},
		{"devin timestamps", "devin", now, map[string]string{"daily_quota_reset_at": later.Format(time.RFC3339), "weekly_quota_reset_at": earlier.Format(time.RFC3339Nano)}, earlier},
		{"ignore expired window", "claude", now, map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": unix(now.Add(-time.Hour)), "Anthropic-Ratelimit-Unified-7d-Reset": unix(later)}, later},
		{"invalid reset", "claude", now, map[string]string{"Anthropic-Ratelimit-Unified-Reset": "invalid"}, time.Time{}},
		{"missing reset", "claude", now, nil, time.Time{}},
		{"unsupported provider", "gemini", now, map[string]string{"Anthropic-Ratelimit-Unified-Reset": unix(earlier)}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Provider: tc.provider, Quota: QuotaState{ObservedAt: tc.observed, Signals: tc.signals}}
			if got := nextQuotaReset(auth, now); !got.Equal(tc.want) {
				t.Fatalf("nextQuotaReset() = %v, want %v", got, tc.want)
			}
		})
	}
}

func soonestResetTestAuth(id, provider string, reset time.Time) *Auth {
	key := "X-Codex-Primary-Reset-At"
	if provider == "claude" {
		key = "Anthropic-Ratelimit-Unified-5h-Reset"
	}
	signals := map[string]string{}
	if !reset.IsZero() {
		signals[key] = strconv.FormatInt(reset.Unix(), 10)
	}
	return &Auth{ID: id, Provider: provider, Status: StatusActive, Quota: QuotaState{Signals: signals}}
}

func TestSoonestResetFirstSelection(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		auths []*Auth
		want  string
	}{
		{"known before unknown", []*Auth{soonestResetTestAuth("a", "codex", time.Time{}), soonestResetTestAuth("z", "codex", now.Add(time.Hour))}, "z"},
		{"soonest window", []*Auth{soonestResetTestAuth("a", "codex", now.Add(24*time.Hour)), soonestResetTestAuth("z", "codex", now.Add(time.Hour))}, "z"},
		{"equal resets use ID", []*Auth{soonestResetTestAuth("z", "codex", now.Add(time.Hour)), soonestResetTestAuth("a", "codex", now.Add(time.Hour))}, "a"},
		{"unknown resets use ID", []*Auth{soonestResetTestAuth("z", "codex", time.Time{}), soonestResetTestAuth("a", "codex", time.Time{})}, "a"},
		{"expired reset is unknown", []*Auth{soonestResetTestAuth("a", "codex", now.Add(-time.Hour)), soonestResetTestAuth("z", "codex", now.Add(time.Hour))}, "z"},
	} {
		for _, path := range []string{"selector", "single", "mixed"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				selector := &SoonestResetFirstSelector{}
				var got *Auth
				var err error
				switch path {
				case "selector":
					got, err = selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, tc.auths)
				case "single":
					got, err = newSchedulerForTest(selector, tc.auths...).pickSingle(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
				case "mixed":
					got, _, err = newSchedulerForTest(selector, tc.auths...).pickMixed(context.Background(), []string{"codex", "claude"}, "", cliproxyexecutor.Options{}, nil)
				}
				if err != nil || got == nil || got.ID != tc.want {
					t.Fatalf("pick = %v, %v, want %s", got, err, tc.want)
				}
			})
		}
	}
}

func TestSoonestResetFirstSchedulerMixedPriorityAndExclusions(t *testing.T) {
	now := time.Now()
	low := soonestResetTestAuth("low", "claude", now.Add(time.Minute))
	low.Attributes = map[string]string{"priority": "-1"}
	soon := soonestResetTestAuth("soon", "codex", now.Add(time.Hour))
	later := soonestResetTestAuth("later", "claude", now.Add(2*time.Hour))
	disabled := soonestResetTestAuth("disabled", "codex", now.Add(time.Minute))
	disabled.Disabled = true
	scheduler := newSchedulerForTest(&SoonestResetFirstSelector{}, later, soon, low, disabled)
	for _, tc := range []struct {
		name, want string
		tried      map[string]struct{}
	}{
		{"earliest across providers", "soon", nil},
		{"retry excludes earliest", "later", map[string]struct{}{"soon": {}}},
		{"lower priority fallback", "low", map[string]struct{}{"soon": {}, "later": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, provider, err := scheduler.pickMixed(context.Background(), []string{"claude", "codex"}, "", cliproxyexecutor.Options{}, tc.tried)
			if err != nil || got == nil || got.ID != tc.want || provider != got.Provider {
				t.Fatalf("pickMixed = %v, %q, %v, want %s", got, provider, err, tc.want)
			}
		})
	}
	soon.Unavailable = true
	soon.NextRetryAfter = now.Add(time.Hour)
	soon.Quota.Exceeded = true
	soon.Quota.NextRecoverAt = soon.NextRetryAfter
	scheduler.upsertAuth(soon)
	got, _, err := scheduler.pickMixed(context.Background(), []string{"claude", "codex"}, "", cliproxyexecutor.Options{}, nil)
	if err != nil || got == nil || got.ID != later.ID {
		t.Fatalf("pickMixed during cooldown = %v, %v, want later", got, err)
	}
}

func TestSoonestResetFirstPreservesWebsocketPreference(t *testing.T) {
	now := time.Now()
	soon := soonestResetTestAuth("soon", "codex", now.Add(time.Hour))
	ws := soonestResetTestAuth("ws", "codex", now.Add(2*time.Hour))
	ws.Attributes = map[string]string{"websockets": "true"}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	selector := &SoonestResetFirstSelector{}
	got, err := selector.Pick(ctx, "codex", "", cliproxyexecutor.Options{}, []*Auth{soon, ws})
	if err != nil || got == nil || got.ID != ws.ID {
		t.Fatalf("selector websocket pick = %v, %v, want ws", got, err)
	}
	got, err = newSchedulerForTest(selector, soon, ws).pickSingle(ctx, "codex", "", cliproxyexecutor.Options{}, nil)
	if err != nil || got == nil || got.ID != ws.ID {
		t.Fatalf("scheduler websocket pick = %v, %v, want ws", got, err)
	}
}

func TestSoonestResetFirstObservationsAndAffinity(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		t.Run(strconv.FormatBool(affinity), func(t *testing.T) {
			ctx := logging.WithResponseHeadersHolder(context.Background())
			var selector Selector = &SoonestResetFirstSelector{}
			if affinity {
				wrapper := NewSessionAffinitySelector(selector)
				t.Cleanup(wrapper.Stop)
				selector = wrapper
			}
			manager := NewManager(nil, selector, nil)
			manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
			a := soonestResetTestAuth("a", "codex", time.Now().Add(time.Hour))
			b := soonestResetTestAuth("b", "codex", time.Now().Add(2*time.Hour))
			for _, auth := range []*Auth{a, b} {
				if _, err := manager.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
			}
			opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-Id": []string{"test-session"}}}
			got, err := manager.SelectAuth(ctx, "codex", "", opts)
			if err != nil || got == nil || got.ID != "a" {
				t.Fatalf("initial selection = %v, %v, want a", got, err)
			}
			headers := http.Header{"X-Codex-Primary-Reset-At": []string{strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10)}}
			logging.SetResponseHeaders(ctx, headers)
			manager.MarkResult(ctx, Result{AuthID: "b", Provider: "codex", Success: true})
			got, err = manager.SelectAuth(ctx, "codex", "", opts)
			want := "b"
			if affinity {
				want = "a"
			}
			if err != nil || got == nil || got.ID != want {
				t.Fatalf("selection after new observation = %v, %v, want %s", got, err, want)
			}
			if affinity {
				opts.Headers.Set("X-Session-Id", "new-session")
				got, err = manager.SelectAuth(ctx, "codex", "", opts)
				if err != nil || got == nil || got.ID != "b" {
					t.Fatalf("new session = %v, %v, want b", got, err)
				}
			}
		})
	}
}

func TestSoonestResetFirstObservationsAcrossModelShards(t *testing.T) {
	for _, switchStrategy := range []bool{false, true} {
		t.Run(strconv.FormatBool(switchStrategy), func(t *testing.T) {
			a := soonestResetTestAuth("reset-shard-a", "codex", time.Now().Add(time.Hour))
			b := soonestResetTestAuth("reset-shard-b", "codex", time.Now().Add(2*time.Hour))
			for _, auth := range []*Auth{a, b} {
				registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "model-a"}, {ID: "model-b"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}
			var selector Selector = &SoonestResetFirstSelector{}
			if switchStrategy {
				selector = &RoundRobinSelector{}
			}
			scheduler := newSchedulerForTest(selector, a, b)
			for _, model := range []string{"model-a", "model-b"} {
				if _, err := scheduler.pickSingle(context.Background(), "codex", model, cliproxyexecutor.Options{}, nil); err != nil {
					t.Fatal(err)
				}
			}
			updated := soonestResetTestAuth(b.ID, "codex", time.Now().Add(30*time.Minute))
			updated.Generation = b.Generation + 1
			scheduler.upsertAuthResult(updated, []string{"model-a"}, false)
			if switchStrategy {
				scheduler.setSelector(&SoonestResetFirstSelector{})
			}
			got, err := scheduler.pickSingle(context.Background(), "codex", "model-b", cliproxyexecutor.Options{}, nil)
			if err != nil || got == nil || got.ID != b.ID {
				t.Fatalf("pick after observation on other model = %v, %v, want %s", got, err, b.ID)
			}
		})
	}
}
