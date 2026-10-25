package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type hostingTestSettings struct {
	SettingRepository
	value string
}

func (r *hostingTestSettings) GetValue(context.Context, string) (string, error) {
	if r.value == "" {
		return "", ErrSettingNotFound
	}
	return r.value, nil
}
func (r *hostingTestSettings) Set(_ context.Context, _ string, value string) error {
	r.value = value
	return nil
}

type hostingTestRepository struct {
	HostingRepository
	policy        HostingUserPolicy
	accounts      []Account
	saved         *Account
	savedSettings *HostingAccountSettings
	pools         map[int64]int64
	poolName      string
}

func (r *hostingTestRepository) GetOwnedPool(_ context.Context, userID, poolID int64) (*HostingPool, error) {
	if r.pools[poolID] != userID {
		return nil, ErrHostingPoolNotFound
	}
	name := r.poolName
	if name == "" {
		name = "Pool"
	}
	return &HostingPool{ID: poolID, Name: name}, nil
}

func (r *hostingTestRepository) GetUserPolicy(context.Context, int64) (HostingUserPolicy, error) {
	return r.policy, nil
}
func (r *hostingTestRepository) ListOwned(_ context.Context, id int64) ([]Account, error) {
	var out []Account
	for _, a := range r.accounts {
		if a.OwnerUserID != nil && *a.OwnerUserID == id {
			out = append(out, a)
		}
	}
	return out, nil
}
func (r *hostingTestRepository) SaveOwned(_ context.Context, _ int64, a *Account, _ int64, _ int, settings ...*HostingAccountSettings) (*Account, error) {
	r.saved = a
	if len(settings) > 0 {
		r.savedSettings = settings[0]
	}
	return a, nil
}

func TestHostingCreateSettingsValidatedBeforeUpstreamAndOAuthConsumption(t *testing.T) {
	for _, raw := range []string{
		`{"credentials":{"access_token":"forbidden"}}`,
		`{"extra":{"openai_prism_browser":true}}`,
		`{"extra":{"openai_excel_bps_proxy_source":"ip_pool"}}`,
		`{"concurrency":0}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s, repo, _ := hostingTestService()
			s.sessions.Store("bound", hostingOAuthSession{userID: 7, expiresAt: time.Now().Add(time.Minute)})
			var input HostingAccountSettings
			require.NoError(t, json.Unmarshal([]byte(raw), &input))
			_, err := s.ImportWithSettings(context.Background(), 7, 0, "Codex", `{"refresh_token":"test-only"}`, &input)
			require.Error(t, err) // OAuth provider is nil: validation must precede refresh.
			_, err = s.CompleteOAuth(context.Background(), 7, "bound", "code", "state", "Codex", &input)
			require.Error(t, err)
			_, exists := s.sessions.Load("bound")
			require.True(t, exists, "invalid configuration must not consume an authorization session")
			require.Nil(t, repo.saved)
		})
	}
}

func TestHostingCreateSettingsForwardedWithVerifiedCredentials(t *testing.T) {
	s, repo, _ := hostingTestService()
	var input HostingAccountSettings
	require.NoError(t, json.Unmarshal([]byte(`{"name":" Configured Codex ","concurrency":8,"extra":{"openai_oauth_responses_websockets_v2_mode":"ctx_pool"},"credentials":{"model_mapping":{"client":"gpt-5.4"}}}`), &input))
	_, err := s.saveTokenWithSettings(context.Background(), 7, 0, "Codex", &OpenAITokenInfo{
		ChatGPTAccountID: "upstream", ChatGPTUserID: "user", RefreshToken: "verified-refresh", AccessToken: "verified-access", PlanType: "plus",
	}, &input)
	require.NoError(t, err)
	require.Equal(t, "Configured Codex", repo.saved.Name)
	require.Equal(t, "verified-refresh", repo.saved.GetCredential("refresh_token"))
	require.Same(t, &input, repo.savedSettings)
	require.Equal(t, true, repo.savedSettings.Extra["openai_oauth_responses_websockets_v2_enabled"])
}

func TestHostingOAuthCreationRejectsPoolChangeWithoutConsumingSession(t *testing.T) {
	s, repo, _ := hostingTestService()
	repo.pools = map[int64]int64{31: 7}
	s.sessions.Store("bound", hostingOAuthSession{userID: 7, expiresAt: time.Now().Add(time.Minute)})
	pool := int64(31)
	_, err := s.CompleteOAuth(context.Background(), 7, "bound", "code", "state", "Codex", &HostingAccountSettings{HostingPoolID: &pool})
	require.Error(t, err)
	_, exists := s.sessions.Load("bound")
	require.True(t, exists)
	require.Nil(t, repo.saved)
}

type hostingTestGroups struct {
	GroupRepository
	group *Group
}

func (r *hostingTestGroups) GetByID(_ context.Context, id int64) (*Group, error) {
	if r.group == nil || r.group.ID != id {
		return nil, ErrGroupNotFound
	}
	return r.group, nil
}

type hostingTestUsers struct {
	UserRepository
	user *User
}

func (r *hostingTestUsers) GetByID(_ context.Context, id int64) (*User, error) {
	if r.user == nil || r.user.ID != id {
		return nil, ErrUserNotFound
	}
	return r.user, nil
}

func hostingTestService() (*HostingService, *hostingTestRepository, *hostingTestSettings) {
	settings := &hostingTestSettings{value: `{"enabled":true,"default_seats":1,"rate_multiplier":0.03,"fallback_group_id":9}`}
	repo := &hostingTestRepository{}
	return NewHostingService(repo, settings, nil, &hostingTestGroups{group: &Group{ID: 9, Platform: PlatformOpenAI, Status: StatusActive}}, &hostingTestUsers{user: &User{ID: 7}}, nil), repo, settings
}

func TestHostingRequestSnapshotsPolicyAndRecoversPrimary(t *testing.T) {
	s, repo, settings := hostingTestService()
	owner := int64(7)
	repo.accounts = []Account{{ID: 1, OwnerUserID: &owner, Status: StatusActive, Schedulable: true}}
	saved := &APIKey{ID: 11, UserID: owner, User: &User{ID: owner}, RouteMode: RouteModeHosted, HostingFallbackGroupID: func() *int64 { v := int64(9); return &v }()}
	ctx, key, err := s.PrepareRequest(context.Background(), saved)
	require.NoError(t, err)
	require.NotSame(t, saved, key)
	require.Nil(t, saved.HostingRate)
	require.Nil(t, saved.HostingPoolName)
	require.Equal(t, "", *key.HostingPoolName)
	require.Equal(t, .03, *key.HostingRate)
	require.True(t, HostingAccountAllowed(ctx, &repo.accounts[0]))
	require.False(t, HostingAccountAllowed(ctx, &Account{ID: 2}))
	settings.value = `{"enabled":true,"default_seats":1,"rate_multiplier":0.08,"fallback_group_id":12}`
	require.Equal(t, .03, *key.HostingRate, "ongoing requests retain their rate")
	require.NoError(t, s.SwitchFallback(ctx))
	require.Equal(t, int64(9), *key.GroupID, "saved fallback does not change with defaults")
	require.False(t, HostingAccountAllowed(ctx, &repo.accounts[0]))
	require.True(t, HostingAccountAllowed(ctx, &Account{}))
	_, next, err := s.PrepareRequest(context.Background(), saved)
	require.NoError(t, err)
	require.False(t, next.HostingFallback)
	require.Equal(t, .08, *next.HostingRate)
	override := .02
	seats := 0
	repo.policy = HostingUserPolicy{RateMultiplier: &override, SeatLimit: &seats}
	cfg, _, err := s.Policy(ctx, owner)
	require.NoError(t, err)
	require.Equal(t, 0, cfg.DefaultSeats)
	require.Equal(t, .02, cfg.RateMultiplier)
}

func TestHostingUnavailableAndFallbackAuthorization(t *testing.T) {
	s, repo, _ := hostingTestService()
	owner := int64(7)
	fallback := int64(9)
	saved := &APIKey{ID: 12, UserID: owner, User: &User{ID: owner}, RouteMode: RouteModeHosted, HostingFallbackGroupID: &fallback}
	reset := time.Now().Add(time.Hour)
	repo.accounts = []Account{{ID: 1, OwnerUserID: &owner, Status: StatusActive, Schedulable: true, RateLimitResetAt: &reset}}
	_, key, err := s.PrepareRequest(context.Background(), saved)
	require.NoError(t, err)
	require.True(t, key.HostingFallback)
	saved.HostingFallbackGroupID = nil
	ctx, key, err := s.PrepareRequest(context.Background(), saved)
	require.NoError(t, err)
	require.False(t, key.HostingFallback)
	require.Error(t, s.SwitchFallback(ctx))
	saved.HostingFallbackGroupID = &fallback
	groups := s.groups.(*hostingTestGroups)
	for _, kind := range []string{"disabled", "subscription", "other_platform", "revoked"} {
		t.Run(kind, func(t *testing.T) {
			groups.group = &Group{ID: 9, Platform: PlatformOpenAI, Status: StatusActive}
			s.users.(*hostingTestUsers).user = &User{ID: owner}
			switch kind {
			case "disabled":
				groups.group.Status = StatusDisabled
			case "subscription":
				groups.group.SubscriptionType = SubscriptionTypeSubscription
			case "other_platform":
				groups.group.Platform = PlatformAnthropic
			case "revoked":
				s.users.(*hostingTestUsers).user.RestrictPublicGroups = true
			}
			_, _, err := s.PrepareRequest(context.Background(), saved)
			require.ErrorIs(t, err, ErrHostingFallbackInvalid)
		})
	}
}

func TestHostingTurnRateAndFeatureSwitch(t *testing.T) {
	s, repo, settings := hostingTestService()
	owner := int64(7)
	repo.accounts = []Account{{ID: 1, OwnerUserID: &owner, Status: StatusActive, Schedulable: true}}
	ctx, key, err := s.PrepareRequest(context.Background(), &APIKey{ID: 1, UserID: owner, RouteMode: RouteModeHosted})
	require.NoError(t, err)
	oldRate := key.HostingRate
	keys := &APIKeyService{hostingService: s}
	settings.value = `{"enabled":true,"rate_multiplier":0.1}`
	require.NoError(t, keys.RefreshHostedTurnRate(ctx, key))
	require.Equal(t, .03, *oldRate)
	require.Equal(t, .1, *key.HostingRate)
	settings.value = `{"enabled":false}`
	require.ErrorIs(t, keys.RefreshHostedTurnRate(ctx, key), ErrHostingDisabled)
	_, ordinary, err := keys.PrepareHostedRequest(ctx, &APIKey{ID: 2})
	require.NoError(t, err)
	require.Equal(t, int64(2), ordinary.ID)
}

func TestHostingAccountQuotaResetAndLoginState(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	a := &Account{Status: StatusActive, Schedulable: true, Extra: map[string]any{"codex_5h_used_percent": 100, "codex_5h_reset_at": reset.Format(time.RFC3339)}}
	state, until := HostedAccountAvailability(a, now)
	require.Equal(t, "quota_exhausted", state)
	require.WithinDuration(t, reset, *until, time.Second)
	state, until = HostedAccountAvailability(a, reset.Add(time.Second))
	require.Equal(t, "active", state)
	require.Nil(t, until)
	a.Status, a.ErrorMessage = StatusError, "refresh token revoked"
	state, _ = HostedAccountAvailability(a, now)
	require.Equal(t, "login_required", state)
	a.ErrorMessage = "upstream service temporarily unavailable"
	state, _ = HostedAccountAvailability(a, now)
	require.Equal(t, "temporarily_unavailable", state)
	a.Status = StatusDisabled
	state, _ = HostedAccountAvailability(a, now)
	require.Equal(t, "paused", state)
}

func TestHostingOAuthSessionOwnershipAndInvalidImport(t *testing.T) {
	s, _, _ := hostingTestService()
	s.sessions.Store("bound", hostingOAuthSession{userID: 7, replaceID: 21, expiresAt: time.Now().Add(time.Minute)})
	_, err := s.CompleteOAuth(context.Background(), 8, "bound", "code", "state", "name")
	require.Error(t, err)
	_, exists := s.sessions.Load("bound")
	require.True(t, exists, "another user cannot consume the rightful owner's session")
	s.sessions.Store("expired", hostingOAuthSession{userID: 7, expiresAt: time.Now().Add(-time.Minute)})
	_, err = s.CompleteOAuth(context.Background(), 7, "expired", "code", "state", "name")
	require.Error(t, err)
	for _, content := range []string{"not-json", `{}`, `{"tokens":{"access_token":"unverified"}}`} {
		_, err = s.Import(context.Background(), 7, 0, "name", content)
		require.Error(t, err)
	}
	_, err = s.Import(context.Background(), 8, 21, "name", `{}`)
	require.ErrorIs(t, err, ErrHostingAccountNotFound)
}

func TestHostingRequiresVerifiedSubscription(t *testing.T) {
	for _, plan := range []string{"", "free", "abnormal", "plus", "pro", "team"} {
		t.Run("plan="+plan, func(t *testing.T) {
			s, repo, _ := hostingTestService()
			s.oauth = &OpenAIOAuthService{}
			a, err := s.saveToken(context.Background(), 7, 0, "Codex", &OpenAITokenInfo{
				ChatGPTAccountID: "account", ChatGPTUserID: "user", RefreshToken: "refresh", PlanType: plan,
			})
			if plan == "" || plan == "free" || plan == "abnormal" {
				require.Error(t, err)
				require.Nil(t, a)
				require.Nil(t, repo.saved, "unsupported plans must not consume a seat")
			} else {
				require.NoError(t, err)
				require.Same(t, repo.saved, a)
				require.True(t, a.IsOpenAIChatGPTSubscription())
			}
		})
	}
}

func TestHostingFinalAdmissionAndStateIsolation(t *testing.T) {
	owner, other := int64(7), int64(8)
	account := &Account{ID: 2, OwnerUserID: &owner, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	s := &OpenAIGatewayService{accountRepo: &turnAdmissionRepo{account: account}}
	ctx := context.WithValue(context.Background(), hostingRequestContextKey{}, &APIKey{ID: 1, UserID: other, RouteMode: RouteModeHosted})
	_, err := s.AdmitOpenAITurn(ctx, nil, account, "gpt-5.1")
	require.True(t, IsOpenAITurnAdmissionError(err))
	_, err = s.AdmitOpenAITurn(context.Background(), nil, account, "gpt-5.1")
	require.True(t, IsOpenAITurnAdmissionError(err))
	key := &APIKey{ID: 1, UserID: owner, RouteMode: RouteModeHosted}
	ctx = context.WithValue(context.Background(), hostingRequestContextKey{}, key)
	_, err = s.AdmitOpenAITurn(ctx, nil, account, "gpt-5.1")
	require.NoError(t, err)
	store := s.getOpenAIWSStateStoreForContext(ctx)
	require.NoError(t, store.BindResponseAccount(ctx, 0, "resp_same", 2, time.Minute))
	store.BindResponseConn("resp_same", "private_conn", time.Minute)
	otherCtx := context.WithValue(context.Background(), hostingRequestContextKey{}, &APIKey{ID: 2, UserID: other, RouteMode: RouteModeHosted})
	otherStore := s.getOpenAIWSStateStoreForContext(otherCtx)
	id, err := otherStore.GetResponseAccount(otherCtx, 0, "resp_same")
	require.NoError(t, err)
	require.Zero(t, id)
	_, found := otherStore.GetResponseConn("resp_same")
	require.False(t, found)
	key.HostingFallback = true
	id, err = s.getOpenAIWSStateStoreForContext(ctx).GetResponseAccount(ctx, 0, "resp_same")
	require.NoError(t, err)
	require.Zero(t, id)
	require.NotEqual(t, scopeHostingSessionHash(ctx, "same"), scopeHostingSessionHash(otherCtx, "same"))
}

func TestHostedBillingUsesOnlyCapturedRate(t *testing.T) {
	for _, rate := range []float64{.03, .02, 0} {
		t.Run(string(mustHostingJSON(rate)), func(t *testing.T) {
			logs := &openAIRecordUsageLogRepoStub{inserted: true}
			users := &openAIRecordUsageUserRepoStub{}
			s := newOpenAIRecordUsageServiceForTest(logs, users, &openAIRecordUsageSubRepoStub{}, nil)
			s.billingService.fallbackPrices["gpt-5.2"] = &ModelPricing{InputPricePerToken: 1}
			owner := int64(7)
			accountRate := 50.0
			poolID, poolName := int64(31), "Request pool"
			key := &APIKey{ID: 1, UserID: owner, RouteMode: RouteModeHosted, HostingRate: &rate, HostingPoolID: &poolID, HostingPoolName: &poolName}
			in := &OpenAIRecordUsageInput{APIKey: key, User: &User{ID: owner}, Account: &Account{ID: 2, OwnerUserID: &owner, RateMultiplier: &accountRate, GroupRateMultiplier: func() *float64 { v := 99.0; return &v }()}, Result: &OpenAIForwardResult{RequestID: "hosted-billing", Model: "gpt-5.2", Usage: OpenAIUsage{InputTokens: 100}}}
			require.NoError(t, s.RecordUsage(context.Background(), in))
			require.Equal(t, 100.0, logs.lastLog.TotalCost)
			require.Equal(t, 100*rate, logs.lastLog.ActualCost)
			require.Equal(t, rate, logs.lastLog.RateMultiplier)
			require.Equal(t, "hosted", *logs.lastLog.HostingRoute)
			require.Equal(t, poolID, *logs.lastLog.HostingPoolID)
			require.Equal(t, "Request pool", *logs.lastLog.HostingPoolName)
			poolName = "Changed after request"
			require.Equal(t, "Request pool", *logs.lastLog.HostingPoolName)
			in.Result.Usage = OpenAIUsage{}
			require.NoError(t, s.RecordUsage(context.Background(), in))
			require.Zero(t, logs.lastLog.ActualCost)
			in.Result.UsageUnavailable = true
			calls := logs.calls
			require.Error(t, s.RecordUsage(context.Background(), in))
			require.Equal(t, calls, logs.calls)
		})
	}
}
func mustHostingJSON(v float64) []byte { b, _ := json.Marshal(v); return b }

// A user may configure compact mappings, but must not select the price of a
// cheaper alias while actually invoking a more expensive upstream model.
func TestHostedBillingCannotUseCheapAliasForExpensiveUpstream(t *testing.T) {
	for _, source := range []string{"", BillingModelSourceRequested, BillingModelSourceChannelMapped} {
		t.Run(source, func(t *testing.T) {
			logs := &openAIRecordUsageLogRepoStub{inserted: true}
			s := newOpenAIRecordUsageServiceForTest(logs, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			s.billingService.fallbackPrices["gpt-5.1"] = &ModelPricing{InputPricePerToken: .01}
			s.billingService.fallbackPrices["gpt-5.2"] = &ModelPricing{InputPricePerToken: 1}
			owner, rate := int64(7), .03
			in := &OpenAIRecordUsageInput{
				APIKey: &APIKey{ID: 1, UserID: owner, RouteMode: RouteModeHosted, HostingRate: &rate},
				User:   &User{ID: owner}, Account: &Account{ID: 2, OwnerUserID: &owner},
				OriginalModel: "gpt-5.1", ChannelMappedModel: "gpt-5.1", BillingModelSource: source,
				Result: &OpenAIForwardResult{RequestID: "hosted-alias", Model: "gpt-5.1", BillingModel: "gpt-5.1", UpstreamModel: "gpt-5.2", Usage: OpenAIUsage{InputTokens: 100}},
			}
			require.NoError(t, s.RecordUsage(context.Background(), in))
			require.Equal(t, 100.0, logs.lastLog.TotalCost)
			require.Equal(t, 3.0, logs.lastLog.ActualCost)
		})
	}
}

func TestHostingPoolsScopeSelectionAndSessionState(t *testing.T) {
	s, repo, _ := hostingTestService()
	owner, pool, otherPool, fallback := int64(7), int64(31), int64(32), int64(9)
	repo.pools = map[int64]int64{pool: owner, otherPool: owner, 33: 8}
	repo.accounts = []Account{{ID: 1, OwnerUserID: &owner, HostingPoolID: &otherPool, Status: StatusActive, Schedulable: true}}
	key := &APIKey{ID: 11, UserID: owner, RouteMode: RouteModeHosted, HostingPoolID: &pool, HostingFallbackGroupID: &fallback}
	ctx, prepared, err := s.PrepareRequest(context.Background(), key)
	require.NoError(t, err)
	require.True(t, prepared.HostingFallback, "an available account in another pool must not suppress fallback")
	require.Equal(t, "Pool", *prepared.HostingPoolName)
	require.Nil(t, key.HostingPoolName)
	repo.poolName = "Renamed"
	require.Equal(t, "Pool", *prepared.HostingPoolName, "ongoing fallback keeps its original pool name")
	require.False(t, HostingAccountAllowed(ctx, &repo.accounts[0]))
	repo.accounts = append(repo.accounts, Account{ID: 2, OwnerUserID: &owner, HostingPoolID: &pool, Status: StatusActive, Schedulable: true})
	ctx, prepared, err = s.PrepareRequest(context.Background(), key)
	require.NoError(t, err)
	require.False(t, prepared.HostingFallback)
	require.Equal(t, "Renamed", *prepared.HostingPoolName)
	require.False(t, HostingAccountAllowed(ctx, &repo.accounts[0]))
	require.True(t, HostingAccountAllowed(ctx, &repo.accounts[1]))
	gateway := &OpenAIGatewayService{hostingService: s}
	candidates, err := gateway.listSchedulableAccounts(ctx, nil, PlatformOpenAI)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, int64(2), candidates[0].ID)
	store := gateway.getOpenAIWSStateStoreForContext(ctx)
	require.NoError(t, store.BindResponseAccount(ctx, 0, "resp_pool", 2, time.Minute))
	store.BindResponseConn("resp_pool", "conn", time.Minute)
	clone := *prepared
	clone.HostingPoolID = &otherPool
	otherCtx := context.WithValue(context.Background(), hostingRequestContextKey{}, &clone)
	require.NotEqual(t, scopeHostingSessionHash(ctx, "same"), scopeHostingSessionHash(otherCtx, "same"))
	id, err := gateway.getOpenAIWSStateStoreForContext(otherCtx).GetResponseAccount(otherCtx, 0, "resp_pool")
	require.NoError(t, err)
	require.Zero(t, id)
	_, found := gateway.getOpenAIWSStateStoreForContext(otherCtx).GetResponseConn("resp_pool")
	require.False(t, found)
	key.HostingPoolID = nil
	ctx, prepared, err = s.PrepareRequest(context.Background(), key)
	require.NoError(t, err)
	require.True(t, prepared.HostingFallback, "default pool does not include explicitly assigned accounts")
	key.HostingPoolID = &pool
	key.HostingFallbackGroupID = nil
	repo.accounts = repo.accounts[:1]
	ctx, _, err = s.PrepareRequest(context.Background(), key)
	require.NoError(t, err)
	require.Error(t, s.SwitchFallback(ctx))
	foreignPool := int64(33)
	key.HostingPoolID = &foreignPool
	_, _, err = s.PrepareRequest(context.Background(), key)
	require.ErrorIs(t, err, ErrHostingPoolNotFound)
}
