package service

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func usageConcurrencyAccount(now time.Time) *Account {
	return &Account{ID: 71, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 15,
		Extra: map[string]any{OpenAIUsageConcurrencyKey: map[string]any{"enabled": true, "threshold_percent": 5.0, "concurrency": 6},
			"codex_usage_updated_at": now.Format(time.RFC3339), "codex_5h_used_percent": 1.0, "codex_7d_used_percent": 4.9}}
}
func TestOpenAIUsageConcurrencyTransitions(t *testing.T) {
	now := time.Now()
	a := usageConcurrencyAccount(now)
	for _, mode := range []string{AccountExecutionModeNormal, AccountExecutionModeDockerKernel} {
		a.ExecutionMode = mode
		for _, tc := range []struct {
			used float64
			want int
		}{{0, 6}, {4.99, 6}, {5, 15}, {80, 15}, {0, 6}} {
			a.Extra["codex_7d_used_percent"] = tc.used
			require.Equal(t, tc.want, a.schedulingConcurrencyAt(now))
			require.Equal(t, 15, a.Concurrency, "never overwrite configured concurrency")
		}
	}
	a.Extra["codex_7d_used_percent"] = 90.0
	a.Extra["codex_7d_reset_at"] = now.Add(-time.Second).Format(time.RFC3339)
	require.Equal(t, 6, a.schedulingConcurrencyAt(now))
	a.Extra["codex_5h_used_percent"] = 6.0
	require.Equal(t, 15, a.schedulingConcurrencyAt(now))
}
func TestOpenAIUsageConcurrencyFallbacks(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		change func(*Account)
	}{
		{"disabled", func(a *Account) { a.Extra[OpenAIUsageConcurrencyKey].(map[string]any)["enabled"] = false }},
		{"missing policy", func(a *Account) { delete(a.Extra, OpenAIUsageConcurrencyKey) }},
		{"missing usage", func(a *Account) { delete(a.Extra, "codex_5h_used_percent"); delete(a.Extra, "codex_7d_used_percent") }},
		{"stale usage", func(a *Account) { a.Extra["codex_usage_updated_at"] = now.Add(-9 * time.Hour).Format(time.RFC3339) }},
		{"invalid usage", func(a *Account) { a.Extra["codex_7d_used_percent"] = "NaN" }},
		{"negative usage", func(a *Account) { a.Extra["codex_7d_used_percent"] = -1.0 }},
		{"invalid policy", func(a *Account) { a.Extra[OpenAIUsageConcurrencyKey] = "bad" }},
		{"non OpenAI", func(a *Account) { a.Platform = PlatformAnthropic }},
		{"base ceiling", func(a *Account) { a.Extra[OpenAIUsageConcurrencyKey].(map[string]any)["concurrency"] = 20 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := usageConcurrencyAccount(now)
			tc.change(a)
			require.Equal(t, 15, a.schedulingConcurrencyAt(now))
		})
	}
	a := usageConcurrencyAccount(now)
	a.Type = AccountTypeAPIKey
	a.Extra["quota_limit"] = 100.0
	a.Extra["quota_used"] = 4.0
	require.Equal(t, 6, a.schedulingConcurrencyAt(now))
	a.Extra["quota_used"] = 5.0
	require.Equal(t, 15, a.schedulingConcurrencyAt(now))
}
func TestOpenAIUsageConcurrencyValidationAndWSRefresh(t *testing.T) {
	a := usageConcurrencyAccount(time.Now())
	require.NoError(t, ValidateOpenAIUsageConcurrencyExtra(a.Platform, a.Extra))
	require.Error(t, ValidateOpenAIUsageConcurrencyExtra(PlatformAnthropic, a.Extra))
	for _, raw := range []any{"bad", map[string]any{"enabled": true, "threshold_percent": 101, "concurrency": 6}, map[string]any{"enabled": true, "threshold_percent": 5, "concurrency": 1.5}} {
		require.Error(t, ValidateOpenAIUsageConcurrencyExtra(PlatformOpenAI, map[string]any{OpenAIUsageConcurrencyKey: raw}))
	}
	stale := *a
	stale.Extra = map[string]any{}
	s := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*a}}}
	got, err := s.ResolveSchedulingConcurrency(context.Background(), &stale)
	require.NoError(t, err)
	require.Equal(t, 6, got)
	a.Extra["codex_7d_used_percent"] = 5.0
	got, err = s.ResolveSchedulingConcurrency(context.Background(), &stale)
	require.NoError(t, err)
	require.Equal(t, 15, got)
}

// Exercise the actual selector entry point, not only the policy helper.
type usagePolicySlotCache struct {
	schedulerTestConcurrencyCache
	limits []int
}

func (c *usagePolicySlotCache) AcquireAccountSlot(_ context.Context, _ int64, limit int, _ string) (bool, error) {
	c.limits = append(c.limits, limit)
	return true, nil
}
func TestOpenAIUsageConcurrencySchedulerEntryPoints(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(strconv.FormatBool(advanced), func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			a := usageConcurrencyAccount(time.Now())
			a.Status = StatusActive
			a.Schedulable = true
			a.Type = AccountTypeAPIKey
			a.Extra["quota_limit"] = 100.0
			a.Extra["quota_used"] = 4.0
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.LoadBatchEnabled = false
			cache := &usagePolicySlotCache{}
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*a}}, cache: &schedulerTestGatewayCache{}, cfg: cfg,
				concurrencyService: NewConcurrencyService(cache), rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(strconv.FormatBool(advanced))}
			for _, tc := range []struct {
				used  float64
				limit int
			}{{4, 6}, {5, 15}, {0, 6}} {
				a.Extra["quota_used"] = tc.used
				cache.limits = nil
				result, _, err := svc.SelectAccountWithScheduler(context.Background(), nil, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.Acquired)
				require.NotEmpty(t, cache.limits)
				for _, limit := range cache.limits {
					require.Equal(t, tc.limit, limit)
				}
				result.ReleaseFunc()
			}
		})
	}
}

func TestOpenAIUsageConcurrencyPreviousResponseAdmission(t *testing.T) {
	ctx := context.Background()
	groupID := int64(23)
	a := usageConcurrencyAccount(time.Now())
	a.Type = AccountTypeAPIKey
	a.Status = StatusActive
	a.Schedulable = true
	a.Extra["quota_limit"] = 100.0
	a.Extra["quota_used"] = 4.0
	a.Extra["openai_apikey_responses_websockets_v2_enabled"] = true
	cache := &stubGatewayCache{}
	store := NewOpenAIWSStateStore(cache)
	slots := &usagePolicySlotCache{}
	svc := &OpenAIGatewayService{accountRepo: stubOpenAIAccountRepo{accounts: []Account{*a}}, cache: cache, cfg: newOpenAIWSV2TestConfig(), concurrencyService: NewConcurrencyService(slots), openaiWSStateStore: store}
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_usage_policy", a.ID, time.Hour))
	for _, tc := range []struct {
		used  float64
		limit int
	}{{4, 6}, {5, 15}} {
		a.Extra["quota_used"] = tc.used
		slots.limits = nil
		selection, err := svc.SelectAccountByPreviousResponseID(ctx, &groupID, "resp_usage_policy", "gpt-5.1", nil, false)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.True(t, selection.Acquired)
		require.Equal(t, []int{tc.limit}, slots.limits)
		selection.ReleaseFunc()
	}
}
