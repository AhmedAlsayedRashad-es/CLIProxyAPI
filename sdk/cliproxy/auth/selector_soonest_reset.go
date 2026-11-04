package auth

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// SoonestResetFirstSelector consumes the credential whose known quota window
// resets next. Unknown or expired resets sort last; ties use credential ID order.
type SoonestResetFirstSelector struct{}

func (s *SoonestResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, errAvailable := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if errAvailable != nil {
		return nil, errAvailable
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var picked *Auth
	var pickedReset time.Time
	for _, candidate := range available {
		reset := nextQuotaReset(candidate, now)
		if picked == nil || quotaResetComesFirst(reset, candidate.ID, pickedReset, picked.ID) {
			picked, pickedReset = candidate, reset
		}
	}
	return picked, nil
}

func quotaResetComesFirst(reset time.Time, id string, otherReset time.Time, otherID string) bool {
	if reset.Equal(otherReset) {
		return id < otherID
	}
	return !reset.IsZero() && (otherReset.IsZero() || reset.Before(otherReset))
}

func (v *readyView) pickSoonestReset(predicate func(*scheduledAuth) bool, now time.Time) *scheduledAuth {
	var picked *scheduledAuth
	var pickedReset time.Time
	for _, entry := range v.flat {
		if entry == nil || entry.auth == nil || (predicate != nil && !predicate(entry)) {
			continue
		}
		reset := nextQuotaReset(entry.auth, now)
		if picked == nil || quotaResetComesFirst(reset, entry.auth.ID, pickedReset, picked.auth.ID) {
			picked, pickedReset = entry, reset
		}
	}
	return picked
}

// nextQuotaReset reads account-wide windows only. Additional Codex model limits,
// code-review limits, Retry-After and token expiry do not describe the next
// account quota reset and must not influence this ordering.
func nextQuotaReset(auth *Auth, now time.Time) time.Time {
	var reset time.Time
	consider := func(value time.Time) {
		if value.After(now) && (reset.IsZero() || value.Before(reset)) {
			reset = value
		}
	}
	quota := auth.Quota
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		for _, key := range []string{
			"Anthropic-Ratelimit-Unified-5h-Reset",
			"Anthropic-Ratelimit-Unified-7d-Reset",
			"Anthropic-Ratelimit-Unified-Reset",
		} {
			consider(parseQuotaResetInstant(quota.Signals[key]))
		}
	case "codex":
		for _, window := range []string{"Primary", "Secondary"} {
			prefix := "X-Codex-" + window
			absolute := parseQuotaResetInstant(quota.Signals[prefix+"-Reset-At"])
			if !absolute.IsZero() {
				consider(absolute)
				continue
			}
			seconds, errParse := strconv.ParseInt(strings.TrimSpace(quota.Signals[prefix+"-Reset-After-Seconds"]), 10, 64)
			if errParse == nil && seconds > 0 && seconds <= math.MaxInt64/int64(time.Second) && !quota.ObservedAt.IsZero() {
				consider(quota.ObservedAt.Add(time.Duration(seconds) * time.Second))
			}
		}
	case "devin":
		consider(parseQuotaResetInstant(quota.Signals["daily_quota_reset_at"]))
		consider(parseQuotaResetInstant(quota.Signals["weekly_quota_reset_at"]))
	}
	return reset
}

func parseQuotaResetInstant(value string) time.Time {
	value = strings.TrimSpace(value)
	if seconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	instant, _ := time.Parse(time.RFC3339Nano, value)
	return instant
}
