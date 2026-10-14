package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBorrowTierTriesFreeAccountBeforeReturningStickyWait(t *testing.T) {
	ids := map[int64]struct{}{11: {}, 12: {}}
	var calls []int64
	selection, _, err := selectBorrowTier(t.Context(), ids, func(ctx context.Context) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
		if borrowSchedulingAllowsAccount(ctx, 11) {
			calls = append(calls, 11)
			return &AccountSelectionResult{Account: &Account{ID: 11}, WaitPlan: &AccountWaitPlan{AccountID: 11}}, OpenAIAccountScheduleDecision{}, nil
		}
		require.True(t, borrowSchedulingAllowsAccount(ctx, 12))
		calls = append(calls, 12)
		return &AccountSelectionResult{Account: &Account{ID: 12}, Acquired: true}, OpenAIAccountScheduleDecision{}, nil
	})
	require.NoError(t, err)
	require.True(t, selection.Acquired)
	require.EqualValues(t, 12, selection.Account.ID)
	require.Equal(t, []int64{11, 12}, calls)
	require.Len(t, ids, 2, "ordinary fallback must still exclude all borrowed IDs")
}

func TestOpenAIBorrowPreferredScheduling(t *testing.T) {
	for _, mode := range []string{"legacy", "load-batch", "advanced"} {
		for _, scenario := range []string{"ready", "alpha-native", "alpha-pat", "alias", "future-model", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-sol", "busy", "no-route", "expired", "admission-cutoff", "cooldown", "disabled", "excluded", "model", "both-busy", "group", "native-previous"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
				borrow, repo := borrowFixture()
				borrowed := *borrow.owner.accountRepo.(borrowTestAccounts).account
				borrowed.Concurrency, borrowed.Priority = 5, 100
				ordinary := Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: 1}
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:sticky": ordinary.ID}}
				cfg := &config.Config{}
				cfg.Gateway.Scheduling.LoadBatchEnabled = mode != "legacy"
				cfg.Gateway.OpenAIWS.LBTopK = 1
				cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
				slots := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{11: true, 12: true}}
				model := "gpt-6-astra"
				capability := OpenAIEndpointCapabilityResponses
				var groupID *int64
				previous := ""
				excluded := map[int64]struct{}{}
				want := int64(12)
				switch scenario {
				case "ready":
					want = 11
				case "alpha-native":
					capability = OpenAIEndpointCapabilityAlphaSearch
				case "alpha-pat":
					capability, want = OpenAIEndpointCapabilityAlphaSearch, 11
					borrowed.Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken
					repo.route.IdentityHash = borrowAccountIdentity(&borrowed)
				case "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-sol":
					model, want = scenario, 11
				case "alias":
					model, want = "customer-alias", 11
					borrowed.Credentials["model_mapping"] = map[string]any{"customer-alias": "gpt-6.1-sol"}
				case "future-model":
					model, want = "custom-future-model", 11
					// Borrowing must not add a model restriction to normal passthrough.
				case "busy":
					slots.acquireResults[11] = false
				case "both-busy":
					slots.acquireResults[11], slots.acquireResults[12] = false, false
					want = 11
				case "no-route":
					repo.route = nil
				case "expired":
					repo.route.ExpiresAt = repo.now.Add(-time.Second)
				case "admission-cutoff":
					repo.minRemaining = 120 * time.Second
				case "cooldown":
					until := time.Now().Add(time.Minute)
					borrowed.RateLimitResetAt = &until
				case "disabled":
					repo.enabled = false
				case "excluded":
					excluded[11] = struct{}{}
				case "group":
					group := int64(9)
					groupID = &group
					ordinary.GroupIDs = []int64{9}
					borrowed.GroupIDs = []int64{10}
				case "model":
					model = "gpt-other"
					borrowed.Credentials["model_mapping"] = map[string]any{"gpt-6-astra": "gpt-6-astra"}
				}
				gateway := &OpenAIGatewayService{accountRepo: schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{ordinary, borrowed}}}, cache: cache, cfg: cfg, concurrencyService: NewConcurrencyService(slots), borrowRoutes: borrow}
				if mode == "advanced" {
					openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{enabled: true, expiresAt: time.Now().Add(time.Minute).UnixNano()})
					gateway.openaiScheduler = &defaultOpenAIAccountScheduler{service: gateway, stats: newOpenAIAccountRuntimeStats()}
				}
				if scenario == "native-previous" {
					previous = "resp_native"
					require.NoError(t, gateway.getOpenAIWSStateStore().BindResponseAccount(t.Context(), 0, previous, ordinary.ID, time.Hour))
				}
				selection, decision, err := gateway.selectAccountWithScheduler(t.Context(), groupID, previous, "sticky", model, excluded, OpenAIUpstreamTransportHTTPSSE, capability, "", false, PlatformOpenAI, false, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.Equal(t, want, selection.Account.ID)
				require.Equal(t, want == 11 && scenario != "both-busy", decision.BorrowPreferred)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				if scenario == "both-busy" {
					require.False(t, selection.Acquired)
					require.NotNil(t, selection.WaitPlan)
				}
				require.Empty(t, cache.deletedSessions, "preference must not delete ordinary affinity")
			})
		}
	}
}

func TestOpenAIBorrowRoutePublishWakesNormalSelection(t *testing.T) {
	borrow, repo := borrowFixture()
	route := repo.route
	repo.route = nil
	borrowed := *borrow.owner.accountRepo.(borrowTestAccounts).account
	borrowed.Concurrency = 5
	gateway := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{borrowed}}, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{11: true}}), borrowRoutes: borrow}
	ctx, r := WithBorrowBusinessRecovery(t.Context(), nil)
	defer r.Close()
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		time.Sleep(30 * time.Millisecond)
		repo.mu.Lock()
		repo.route = route
		repo.mu.Unlock()
		borrow.notifyRoutePublished()
	}()
	start := time.Now()
	selected, _, err := gateway.selectAccountWithScheduler(ctx, nil, "", "", "gpt-6.1-sol", nil, OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityResponses, "", false, PlatformOpenAI, false, false)
	<-completed
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.EqualValues(t, 11, selected.Account.ID)
	require.Less(t, time.Since(start), time.Second, "local publication should not wait for the polling interval")
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
	require.False(t, r.Active(), "selection does not activate transport recovery")
}
