package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func qualityMonitorQuotaFixture(t *testing.T) (*AccountQualityMonitorService, *Account, *AccountQualityMonitorRecord, *SettingService, time.Time) {
	t.Helper()
	s := newQualityMonitorTestService()
	t.Cleanup(s.Stop)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	account := s.accounts.(*qualityMonitorTestAccounts).items[1]
	account.Type, account.ExecutionMode = AccountTypeOAuth, AccountExecutionModeDockerKernel
	account.Credentials = map[string]any{"chatgpt_account_id": "quota-fixture", "model_mapping": map[string]any{"monitor-model": "gpt-5.6-terra"}}
	account.Extra = map[string]any{
		"codex_5h_used_percent": 0.0, "codex_7d_used_percent": 100.0,
		"codex_7d_window_minutes": 43200, "codex_7d_reset_at": now.Add(24 * time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at": now.Format(time.RFC3339),
	}
	record := s.repo.(*qualityMonitorTestRepo).records[1]
	record.Scope, record.Model, record.Enabled, record.NextDueAt = AccountActivityScope(account), "monitor-model", true, &now
	record.LastValid = &AccountQualityMonitorResult{State: "passed", Answer: "21", Model: record.Model, CheckedAt: now.Add(-time.Hour)}
	settings := &SettingService{}
	settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{})
	s.tests = &AccountTestService{openaiGatewayService: &OpenAIGatewayService{settingService: settings}}
	return s, account, record, settings, now
}

func TestAccountQualityQuotaPolicyAcrossEntrypoints(t *testing.T) {
	for _, scenario := range []struct {
		name, reason string
		setup        func(*Account, *SettingService, time.Time)
	}{
		{name: "100 percent without pause policy"},
		{name: "configured global pause", reason: "30 天额度策略暂停", setup: func(_ *Account, settings *SettingService, _ time.Time) {
			settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: .95})
		}},
		{name: "per-account opt-out", setup: func(account *Account, settings *SettingService, _ time.Time) {
			settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: .95})
			account.Extra["auto_pause_7d_disabled"] = true
		}},
		{name: "selected mapped model cooling down", reason: "当前模型临时限流", setup: func(account *Account, _ *SettingService, now time.Time) {
			setAccountModelRateLimitSnapshot(account, "gpt-5.6-terra", now.Add(time.Hour), "fixture", now)
		}},
		{name: "other model cooling down", setup: func(account *Account, _ *SettingService, now time.Time) {
			setAccountModelRateLimitSnapshot(account, "gpt-6.1-sol", now.Add(time.Hour), "fixture", now)
		}},
	} {
		for _, entry := range []string{"manual", "scheduled", "before-execution"} {
			t.Run(scenario.name+"/"+entry, func(t *testing.T) {
				s, account, record, settings, now := qualityMonitorQuotaFixture(t)
				if scenario.setup != nil {
					scenario.setup(account, settings, now)
				}
				// Caller-provided defaults must not replace the actual shared settings.
				ctx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 1})
				view, err := s.Get(ctx, account.ID, true)
				require.NoError(t, err)
				require.Equal(t, scenario.reason, view.PauseReason)
				require.Equal(t, scenario.reason != "", view.State == "paused")
				batch, err := s.Batch(ctx, []int64{account.ID})
				require.NoError(t, err)
				require.Equal(t, scenario.reason, batch[account.ID].PauseReason)
				view, err = s.Save(ctx, account.ID, record.AccountQualityMonitorConfig)
				require.NoError(t, err)
				require.Equal(t, scenario.reason, view.PauseReason)
				var executed atomic.Int32
				s.execute = func(context.Context, int64, string) (*ScheduledTestResult, error) {
					executed.Add(1)
					return nil, errors.New("fixture transport failure")
				}
				switch entry {
				case "manual":
					_, err = s.Start(ctx, account.ID, record.Scope)
					if scenario.reason != "" {
						require.ErrorIs(t, err, ErrAccountQualityUnavailable)
					} else {
						require.NoError(t, err)
					}
				case "scheduled":
					s.RunDue(ctx, now)
				case "before-execution":
					// Dispatch bypasses the prior checks and detaches from ctx. The
					// worker must independently reload current quota settings.
					require.NoError(t, s.dispatch(ctx, copyQualityMonitorRecord(record), false))
				}
				if scenario.reason == "" || entry == "before-execution" {
					awaitQualityMonitorRelease(t, s, 1)
				}
				if scenario.reason == "" {
					require.Equal(t, int32(1), executed.Load())
				} else {
					require.Zero(t, executed.Load())
				}
				stored, err := s.repo.Get(context.Background(), account.ID, true)
				require.NoError(t, err)
				require.True(t, stored.Enabled)
				require.Equal(t, "passed", stored.LastValid.State)
				require.Equal(t, "21", stored.LastValid.Answer)
				require.False(t, account.Schedulable, "management detection must neither require nor change the business switch")
			})
		}
	}
}

func TestAccountQualityQuotaWindowLabelsAndRecovery(t *testing.T) {
	s, account, _, settings, now := qualityMonitorQuotaFixture(t)
	settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: .9})
	for _, tc := range []struct {
		minutes int
		label   string
	}{{43200, "30 天"}, {10080, "7 天"}, {300, "5 小时"}, {90, "90 分钟"}, {0, "7 天"}} {
		account.Extra["codex_7d_window_minutes"] = tc.minutes
		require.Equal(t, tc.label+"额度策略暂停", s.pauseReason(context.Background(), account, "monitor-model", now))
	}
	account.Extra["codex_7d_reset_at"] = now.Add(-time.Minute).Format(time.RFC3339)
	require.Empty(t, s.pauseReason(context.Background(), account, "monitor-model", now), "elapsed windows must recover")
	account.Extra["codex_7d_reset_at"] = now.Add(time.Hour).Format(time.RFC3339)
	settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{})
	require.Empty(t, s.pauseReason(context.Background(), account, "monitor-model", now), "settings changes apply without recreating the monitor")
	account.Extra["auto_pause_7d_threshold"] = .95
	require.NotEmpty(t, s.pauseReason(context.Background(), account, "monitor-model", now), "account thresholds remain authoritative")
	account.Extra["auto_pause_7d_disabled"] = true
	setAccountModelRateLimitSnapshot(account, "gpt-5.6-terra", now.Add(-time.Minute), "fixture", now.Add(-time.Hour))
	require.Empty(t, s.pauseReason(context.Background(), account, "monitor-model", now), "expired model cooldowns must recover")
}

func TestAccountQualityQuotaEvaluationDoesNotNotifyAutoReset(t *testing.T) {
	s, account, record, _, now := qualityMonitorQuotaFixture(t)
	const accountID = int64(9_900_031)
	delete(s.accounts.(*qualityMonitorTestAccounts).items, account.ID)
	delete(s.repo.(*qualityMonitorTestRepo).records, account.ID)
	account.ID, record.AccountID = accountID, accountID
	s.accounts.(*qualityMonitorTestAccounts).items[accountID] = account
	s.repo.(*qualityMonitorTestRepo).records[accountID] = record
	record.Scope = AccountActivityScope(account)
	account.Extra[OpenAIAutoResetCreditEnabledExtraKey] = true
	notifier := &OpenAIQuotaAutoResetService{ctx: context.Background(), queue: make(chan int64, 2)}
	setOpenAIAutoResetNotifier(notifier)
	openAIAutoResetSchedulerNotifiedAt.Delete(accountID)
	t.Cleanup(func() {
		clearOpenAIAutoResetNotifier(notifier)
		openAIAutoResetSchedulerNotifiedAt.Delete(accountID)
	})
	paused, decision := evaluateOpenAIAccountQuotaPause(context.Background(), account, now)
	require.True(t, paused)
	require.Equal(t, "quota_auto_reset_pending_7d", decision.reason)
	view, err := s.Get(context.Background(), accountID, true)
	require.NoError(t, err)
	require.Equal(t, "paused", view.State)
	_, err = s.Batch(context.Background(), []int64{accountID})
	require.NoError(t, err)
	require.Empty(t, notifier.queue, "reading monitor views must not request credit-reset work")
	paused, actual := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
	require.True(t, paused)
	require.Equal(t, decision, actual)
	require.Len(t, notifier.queue, 1, "the ordinary scheduler retains its original notification")
}
