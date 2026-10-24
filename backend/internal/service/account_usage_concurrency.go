package service

import (
	"context"
	"encoding/json"
	"math"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const OpenAIUsageConcurrencyKey = "openai_usage_concurrency"

type OpenAIUsageConcurrency struct {
	Enabled          bool    `json:"enabled"`
	ThresholdPercent float64 `json:"threshold_percent"`
	Concurrency      int     `json:"concurrency"`
}

func usageConcurrencyPolicy(extra map[string]any) (OpenAIUsageConcurrency, bool) {
	var policy OpenAIUsageConcurrency
	raw := extra[OpenAIUsageConcurrencyKey]
	if raw == nil {
		return policy, false
	}
	data, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(data, &policy) != nil {
		return policy, false
	}
	return policy, !math.IsNaN(policy.ThresholdPercent) && !math.IsInf(policy.ThresholdPercent, 0) &&
		policy.ThresholdPercent > 0 && policy.ThresholdPercent <= 100 && policy.Concurrency > 0 && policy.Concurrency <= math.MaxInt32
}

// Validate at administrative write boundaries; runtime evaluation never changes
// the stored concurrency, account status, or existing in-flight leases.
func ValidateOpenAIUsageConcurrencyExtra(platform string, extra map[string]any) error {
	raw, exists := extra[OpenAIUsageConcurrencyKey]
	if !exists || raw == nil {
		return nil
	}
	policy, valid := usageConcurrencyPolicy(extra)
	if !valid {
		return infraerrors.BadRequest("INVALID_USAGE_CONCURRENCY", "Usage concurrency requires a threshold above 0 and at most 100, and a positive integer concurrency")
	}
	if policy.Enabled && platform != PlatformOpenAI {
		return infraerrors.BadRequest("INVALID_USAGE_CONCURRENCY_PLATFORM", "Usage concurrency is only supported for OpenAI accounts")
	}
	return nil
}

func (a *Account) SchedulingConcurrency() int { return a.schedulingConcurrencyAt(time.Now()) }

func (a *Account) schedulingConcurrencyAt(now time.Time) int {
	if a == nil {
		return 0
	}
	base := a.Concurrency
	if a.Platform != PlatformOpenAI {
		return base
	}
	policy, valid := usageConcurrencyPolicy(a.Extra)
	if !valid || !policy.Enabled {
		return base
	}
	var used float64
	var known bool
	if a.Type == AccountTypeAPIKey {
		// API-key accounts already expose a total billed quota, when configured.
		limit, spent := a.GetQuotaLimit(), a.GetQuotaUsed()
		if limit > 0 && spent >= 0 {
			used, known = spent/limit*100, true
		}
	} else {
		if openAIQuotaHeadroomSnapshotStale(a.Extra, now) {
			return base
		}
		window5h, window7d := openAICanonicalQuotaWindows(a.Extra, now)
		for _, window := range []openAICanonicalQuotaWindow{window5h, window7d} {
			if window.hasUsed && (math.IsNaN(window.usedPercent) || math.IsInf(window.usedPercent, 0) || window.usedPercent < 0) {
				return base
			}
		}
		// Reuse the existing overall account usage interpretation (max 5h/7d),
		// including normalization of legacy fields and elapsed-window resets.
		used, known = QualityUsagePercent(a.Extra, now)
	}
	if !known || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used >= policy.ThresholdPercent {
		return base
	}
	// This policy narrows the configured account capacity; it never raises it.
	if base > 0 && policy.Concurrency >= base {
		return base
	}
	return policy.Concurrency
}

// ResolveSchedulingConcurrency refreshes a long-lived WS connection's account
// snapshot before each new turn, so a quota reset does not retain the old cap.
func (s *OpenAIGatewayService) ResolveSchedulingConcurrency(ctx context.Context, selected *Account) (int, error) {
	if selected == nil {
		return 0, ErrAccountNotFound
	}
	if s == nil || selected.Platform != PlatformOpenAI {
		return selected.SchedulingConcurrency(), nil
	}
	var current *Account
	var err error
	if s.schedulerSnapshot != nil {
		current, err = s.schedulerSnapshot.GetAccount(ctx, selected.ID)
	} else if s.accountRepo != nil {
		current, err = s.accountRepo.GetByID(ctx, selected.ID)
	} else {
		return selected.SchedulingConcurrency(), nil
	}
	if err != nil {
		return 0, err
	}
	if current == nil {
		return 0, ErrAccountNotFound
	}
	return current.SchedulingConcurrency(), nil
}
