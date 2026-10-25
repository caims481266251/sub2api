package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type affinityTestCache struct {
	schedulerTestGatewayCache
	mu       sync.Mutex
	bindings map[string]OpenAISessionAffinityBinding
	err      error
}

func (c *affinityTestCache) GetOpenAISessionAffinity(_ context.Context, key string) (*OpenAISessionAffinityBinding, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if b, ok := c.bindings[key]; ok {
		return &b, nil
	}
	return nil, nil
}
func (c *affinityTestCache) ClaimOpenAISessionAffinity(_ context.Context, key string, next OpenAISessionAffinityBinding, _ time.Duration) (*OpenAISessionAffinityBinding, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if b, ok := c.bindings[key]; ok {
		return &b, nil
	}
	next.Pending = true
	c.bindings[key] = next
	return &next, nil
}
func (c *affinityTestCache) CompleteOpenAISessionAffinity(_ context.Context, key string, expected, next OpenAISessionAffinityBinding, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	b, ok := c.bindings[key]
	if !ok || b.Version != expected.Version || (!b.Pending && expected.Pending && b.AccountID != next.AccountID) {
		return false, nil
	}
	next.Pending = false
	c.bindings[key] = next
	return true, nil
}

func affinityTestRequest() OpenAISessionAffinityRequest {
	return OpenAISessionAffinityRequest{UserID: 1, APIKeyID: 2, SessionHash: "session", Model: "gpt-6-astra", Platform: PlatformOpenAI}
}

func newAffinitySchedulerFixture(t *testing.T, mode string) (*OpenAIGatewayService, *borrowTestRepo, *affinityTestCache, map[int64]bool) {
	t.Helper()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	borrow, repo := borrowFixture()
	borrowed := *borrow.owner.accountRepo.(borrowTestAccounts).account
	borrowed.Concurrency, borrowed.Priority = 5, 100
	ordinary := Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 5, Priority: 1}
	cache := &affinityTestCache{bindings: map[string]OpenAISessionAffinityBinding{}}
	cache.sessionBindings = map[string]int64{"openai:session": 12}
	cfg := &config.Config{}
	cfg.Gateway.OpenAISessionAffinityEnabled = true
	cfg.Gateway.Scheduling.LoadBatchEnabled = mode != "legacy"
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	slots := map[int64]bool{11: true, 12: true}
	gateway := &OpenAIGatewayService{accountRepo: schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{ordinary, borrowed}}}, cache: cache, cfg: cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: slots}), borrowRoutes: borrow}
	if mode == "advanced" {
		openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{enabled: true, expiresAt: time.Now().Add(time.Minute).UnixNano()})
		gateway.openaiScheduler = &defaultOpenAIAccountScheduler{service: gateway, stats: newOpenAIAccountRuntimeStats()}
	}
	return gateway, repo, cache, slots
}

func selectAffinityTest(t *testing.T, s *OpenAIGatewayService, ctx context.Context, excluded map[int64]struct{}) *AccountSelectionResult {
	t.Helper()
	selection, _, err := s.selectAccountWithScheduler(ctx, nil, "", "session", "gpt-6-astra", excluded,
		OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityResponses, "", false, PlatformOpenAI, false, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	if selection.ReleaseFunc != nil {
		t.Cleanup(selection.ReleaseFunc)
	}
	return selection
}

func TestOpenAISessionAffinityScheduling(t *testing.T) {
	for _, mode := range []string{"legacy", "load-batch", "advanced"} {
		for _, scenario := range []string{"new-prefers-borrowed", "ordinary-owner", "ordinary-busy", "temporary-busy", "no-route-takeover", "failed-takeover", "excluded-takeover", "admission-capacity-race", "rpm-capacity-race", "both-busy", "cache-failure"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				s, repo, cache, slots := newAffinitySchedulerFixture(t, mode)
				ctx := s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())
				a := openAISessionAffinityFromContext(ctx)
				base := OpenAISessionAffinityBinding{AccountID: 11, Version: "original", Channel: "borrowed"}
				want, wantOwner := int64(11), int64(11)
				excluded := map[int64]struct{}{}
				if scenario != "new-prefers-borrowed" {
					cache.bindings[a.key] = base
				}
				switch scenario {
				case "ordinary-owner":
					base.AccountID, base.Channel = 12, "ordinary"
					cache.bindings[a.key] = base
					want, wantOwner = 12, 12
				case "temporary-busy":
					slots[11] = false
					want = 12
				case "ordinary-busy":
					base.AccountID, base.Channel = 12, "ordinary"
					cache.bindings[a.key] = base
					slots[12] = false
					want, wantOwner = 11, 12
				case "no-route-takeover":
					repo.route = nil
					want, wantOwner = 12, 12
				case "failed-takeover":
					repo.route = nil
					want = 12
				case "excluded-takeover":
					excluded[11] = struct{}{}
					want, wantOwner = 12, 12
				case "admission-capacity-race":
					excluded[11] = struct{}{}
					want = 12
					ObserveOpenAISessionAffinityFailure(ctx, 11, &BorrowRouteAdmissionError{Reason: "account_busy"})
					ObserveOpenAISessionAffinityFailure(ctx, 99, errors.New("another provider failed"))
				case "rpm-capacity-race":
					excluded[11] = struct{}{}
					want = 12
					ObserveOpenAISessionAffinityFailure(ctx, 11, ErrOpenAIRPMExhausted)
				case "both-busy":
					slots[11], slots[12] = false, false
				case "cache-failure":
					cache.err = errors.New("redis unavailable")
				}
				selected := selectAffinityTest(t, s, ctx, excluded)
				require.Equal(t, want, selected.Account.ID)
				if scenario == "both-busy" {
					require.False(t, selected.Acquired)
					require.NotNil(t, selected.WaitPlan)
					return
				}
				require.True(t, selected.Acquired)
				require.NoError(t, s.BindStickySessionAfterProfitAdmission(ctx, nil, "session", want))
				if scenario == "failed-takeover" {
					s.FinishOpenAISessionAffinity(ctx, nil, nil)
				} else {
					s.FinishOpenAISessionAffinity(ctx, selected.Account, &OpenAIForwardResult{})
				}
				if scenario != "cache-failure" {
					require.Equal(t, wantOwner, cache.bindings[a.key].AccountID)
				}
				require.EqualValues(t, 12, cache.sessionBindings["openai:session"], "managed requests must not overwrite unscoped legacy bindings")
				if scenario == "ordinary-owner" || scenario == "no-route-takeover" {
					// Borrowing recovers. An established ordinary owner still wins.
					_, restored := borrowFixture()
					repo.route = restored.route
					next := s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())
					require.EqualValues(t, 12, selectAffinityTest(t, s, next, nil).Account.ID)
				}
				if scenario == "temporary-busy" {
					slots[11] = true
					next := s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())
					require.EqualValues(t, 11, selectAffinityTest(t, s, next, nil).Account.ID)
				}
			})
		}
	}
}

func TestOpenAISessionAffinityLateSuccessCannotUndoTakeover(t *testing.T) {
	s, _, cache, _ := newAffinitySchedulerFixture(t, "legacy")
	oldCtx := s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())
	a := openAISessionAffinityFromContext(oldCtx)
	cache.bindings[a.key] = OpenAISessionAffinityBinding{AccountID: 11, Channel: "borrowed", Version: "old"}
	old := selectAffinityTest(t, s, oldCtx, nil)
	newCtx := s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())
	newSelection := selectAffinityTest(t, s, newCtx, map[int64]struct{}{11: {}})
	s.FinishOpenAISessionAffinity(newCtx, newSelection.Account, &OpenAIForwardResult{})
	require.EqualValues(t, 12, cache.bindings[a.key].AccountID)
	s.FinishOpenAISessionAffinity(oldCtx, old.Account, &OpenAIForwardResult{})
	require.EqualValues(t, 12, cache.bindings[a.key].AccountID)
	guardianCtx := context.WithValue(t.Context(), openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "session", apiKeyID: 2})
	require.EqualValues(t, 12, s.resolveOpenAIGuardianParentAccountID(guardianCtx, nil), "successful takeover retains a scoped parent hint")
	otherTenant := context.WithValue(t.Context(), openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "session", apiKeyID: 3})
	delete(cache.sessionBindings, "openai:session")
	require.Zero(t, s.resolveOpenAIGuardianParentAccountID(otherTenant, nil))
}

func TestOpenAISessionAffinityConcurrentInitialSelectionReleasesLosingSlot(t *testing.T) {
	s, _, cache, _ := newAffinitySchedulerFixture(t, "legacy")
	ctx := s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())
	a := openAISessionAffinityFromContext(ctx)
	released := 0
	selected, _, err := s.selectWithOpenAISessionAffinity(ctx,
		func(ctx context.Context) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
			require.True(t, borrowSchedulingAllowsAccount(ctx, 12))
			require.False(t, borrowSchedulingAllowsAccount(ctx, 11))
			return &AccountSelectionResult{Account: &Account{ID: 12}, Acquired: true}, OpenAIAccountScheduleDecision{}, nil
		},
		func(context.Context) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
			cache.bindings[a.key] = OpenAISessionAffinityBinding{AccountID: 12, Channel: "ordinary", Version: "other-instance", Pending: true}
			return &AccountSelectionResult{Account: &Account{ID: 11}, Acquired: true, ReleaseFunc: func() { released++ }}, OpenAIAccountScheduleDecision{}, nil
		}, "gpt-6-astra", nil)
	require.NoError(t, err)
	require.EqualValues(t, 12, selected.Account.ID)
	require.Equal(t, 1, released)
	s.FinishOpenAISessionAffinity(ctx, selected.Account, &OpenAIForwardResult{})
	require.False(t, cache.bindings[a.key].Pending)
}

func TestOpenAISessionAffinityQueuedClaimCannotOverwriteConcurrentOwner(t *testing.T) {
	s, _, cache, _ := newAffinitySchedulerFixture(t, "legacy")
	ctx := s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())
	a := openAISessionAffinityFromContext(ctx)
	a.loaded = true
	a.mode = "new_session"
	cache.bindings[a.key] = OpenAISessionAffinityBinding{AccountID: 12, Channel: "ordinary", Version: "concurrent-success"}
	require.NoError(t, s.BindStickySessionAfterProfitAdmission(ctx, nil, "session", 11))
	s.FinishOpenAISessionAffinity(ctx, &Account{ID: 11}, &OpenAIForwardResult{})
	require.EqualValues(t, 12, cache.bindings[a.key].AccountID)
	require.Equal(t, "temporary_spillover", a.mode)
}

func TestOpenAISessionAffinityIsolationAndBypass(t *testing.T) {
	s, _, _, _ := newAffinitySchedulerFixture(t, "legacy")
	keys := map[string]bool{}
	for _, change := range []func(*OpenAISessionAffinityRequest){
		func(r *OpenAISessionAffinityRequest) {}, func(r *OpenAISessionAffinityRequest) { r.UserID++ },
		func(r *OpenAISessionAffinityRequest) { r.APIKeyID++ }, func(r *OpenAISessionAffinityRequest) { r.GroupID = new(int64(9)) },
		func(r *OpenAISessionAffinityRequest) { r.Model = "gpt-6.1-sol" }, func(r *OpenAISessionAffinityRequest) { r.SessionHash = "other" },
	} {
		r := affinityTestRequest()
		change(&r)
		a := openAISessionAffinityFromContext(s.BeginOpenAISessionAffinity(t.Context(), r))
		require.NotNil(t, a)
		require.False(t, keys[a.key])
		keys[a.key] = true
	}
	for _, change := range []func(*OpenAISessionAffinityRequest){
		func(r *OpenAISessionAffinityRequest) { r.PreviousResponseID = "resp_owned" }, func(r *OpenAISessionAffinityRequest) { r.Disabled = true },
		func(r *OpenAISessionAffinityRequest) { r.Platform = PlatformGrok }, func(r *OpenAISessionAffinityRequest) { r.APIKeyID = 0 },
		func(r *OpenAISessionAffinityRequest) { r.SessionHash = "" },
	} {
		r := affinityTestRequest()
		change(&r)
		require.Nil(t, openAISessionAffinityFromContext(s.BeginOpenAISessionAffinity(t.Context(), r)))
	}
	guardianCtx := context.WithValue(t.Context(), openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "parent"})
	require.Nil(t, openAISessionAffinityFromContext(s.BeginOpenAISessionAffinity(guardianCtx, affinityTestRequest())))
	borrowCtx := context.WithValue(t.Context(), borrowHTTPContinuationKey{}, &borrowHTTPContinuation{TargetID: 11})
	require.Nil(t, openAISessionAffinityFromContext(s.BeginOpenAISessionAffinity(borrowCtx, affinityTestRequest())))
	s.cfg.Gateway.OpenAISessionAffinityGroupIDs = []int64{9}
	require.Nil(t, openAISessionAffinityFromContext(s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())))
	s.cfg.Gateway.OpenAISessionAffinityGroupIDs = nil
	s.cfg.Gateway.OpenAISessionAffinityEnabled = false
	require.Nil(t, openAISessionAffinityFromContext(s.BeginOpenAISessionAffinity(t.Context(), affinityTestRequest())))
}

func TestOpenAISessionAffinityRejectsUnsuccessfulCompletion(t *testing.T) {
	for _, kind := range []string{"cancelled", "client-disconnect", "partial", "failed-terminal", "nil-result", "abort"} {
		t.Run(kind, func(t *testing.T) {
			s, _, cache, _ := newAffinitySchedulerFixture(t, "legacy")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = s.BeginOpenAISessionAffinity(ctx, affinityTestRequest())
			selected := selectAffinityTest(t, s, ctx, nil)
			result := &OpenAIForwardResult{}
			switch kind {
			case "cancelled":
				cancel()
			case "client-disconnect":
				result.ClientDisconnect = true
			case "partial":
				result.streamReadIncomplete = true
			case "failed-terminal":
				result.OpenAIWSMode = true
				result.UpstreamTerminalEvent = "response.failed"
			case "nil-result":
				result = nil
			case "abort":
				selected.Account = nil
			}
			s.FinishOpenAISessionAffinity(ctx, selected.Account, result)
			a := openAISessionAffinityFromContext(ctx)
			require.True(t, cache.bindings[a.key].Pending, "unsuccessful executions cannot confirm ownership")
		})
	}
}
