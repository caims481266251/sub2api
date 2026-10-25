package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hostedGatewaySettings struct{ service.SettingRepository }

func (*hostedGatewaySettings) GetValue(context.Context, string) (string, error) {
	return `{"enabled":true,"rate_multiplier":0.03}`, nil
}

type hostedGatewayAccounts struct{ service.HostingRepository }

func (*hostedGatewayAccounts) GetUserPolicy(context.Context, int64) (service.HostingUserPolicy, error) {
	return service.HostingUserPolicy{}, nil
}
func (*hostedGatewayAccounts) ListOwned(_ context.Context, id int64) ([]service.Account, error) {
	return []service.Account{{ID: 21, OwnerUserID: &id, Status: service.StatusActive, Schedulable: true}}, nil
}

type hostedGatewayGroups struct {
	service.GroupRepository
	group *service.Group
}

func (r *hostedGatewayGroups) GetByID(context.Context, int64) (*service.Group, error) {
	return r.group, nil
}

type hostedGatewayUsers struct {
	service.UserRepository
	user *service.User
}

func (r *hostedGatewayUsers) GetByID(context.Context, int64) (*service.User, error) {
	return r.user, nil
}

type hostedGatewayKeys struct {
	service.APIKeyRepository
	data *service.APIKeyRateLimitData
}

func (r *hostedGatewayKeys) GetRateLimitData(context.Context, int64) (*service.APIKeyRateLimitData, error) {
	return r.data, nil
}

func hostedGatewayFixture(t *testing.T, path string) (*OpenAIGatewayHandler, *gin.Context, *service.APIKey, *service.Group, *hostedGatewayUsers, *hostedGatewayKeys) {
	t.Helper()
	group := &service.Group{ID: 9, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true}
	users := &hostedGatewayUsers{user: &service.User{ID: 7, Balance: 10}}
	keys := &hostedGatewayKeys{}
	hosting := service.NewHostingService(&hostedGatewayAccounts{}, &hostedGatewaySettings{}, nil, &hostedGatewayGroups{group: group}, users, nil)
	keySvc := &service.APIKeyService{}
	keySvc.SetHostingService(hosting)
	fallback := int64(9)
	ctx, key, err := hosting.PrepareRequest(context.Background(), &service.APIKey{ID: 11, UserID: 7, User: users.user, RouteMode: service.RouteModeHosted, HostingFallbackGroupID: &fallback})
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", path, nil).WithContext(ctx)
	c.Set(string(middleware.ContextKeyAPIKey), key)
	billing := service.NewBillingCacheService(nil, users, nil, keys, nil, nil, &config.Config{}, nil)
	t.Cleanup(billing.Stop)
	return &OpenAIGatewayHandler{apiKeyService: keySvc, gatewayService: &service.OpenAIGatewayService{}, billingCacheService: billing}, c, key, group, users, keys
}

func TestHostingFallbackSafeReplayAndProtocolAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, path                           string
		stream, started, ws, written, cancel bool
		selection                            error
		mutate                               func(*service.Group)
		reason                               string
		switched                             bool
	}{
		{name: "nonstream", path: "/v1/responses", selection: service.ErrNoAvailableAccounts, switched: true},
		{name: "SSE before output", path: "/v1/responses", stream: true, selection: service.ErrNoAvailableAccounts, switched: true},
		{name: "SSE started", path: "/v1/responses", stream: true, started: true, selection: service.ErrNoAvailableAccounts},
		{name: "headers committed", path: "/v1/responses", stream: true, written: true, selection: service.ErrNoAvailableAccounts},
		{name: "WS before output", path: "/v1/responses", stream: true, ws: true, selection: service.ErrNoAvailableAccounts, switched: true},
		{name: "WS cancelled", path: "/v1/responses", stream: true, ws: true, cancel: true, selection: service.ErrNoAvailableAccounts, reason: "cancelled"},
		{name: "parameters", path: "/v1/responses", selection: errors.New("invalid request parameters")},
		{name: "stream only", path: "/v1/responses", selection: service.ErrNoAvailableAccounts, mutate: func(g *service.Group) { g.StreamOnly = true }, reason: "STREAM_REQUIRED"},
		{name: "Claude only", path: "/v1/responses", selection: service.ErrNoAvailableAccounts, mutate: func(g *service.Group) { g.ClaudeCodeOnly = true }, reason: "CLAUDE_CODE_ONLY"},
		{name: "Messages revoked", path: "/v1/messages", stream: true, selection: service.ErrNoAvailableAccounts, mutate: func(g *service.Group) { g.AllowMessagesDispatch = false }, reason: "MESSAGES_DISPATCH_NOT_ALLOWED"},
		{name: "model permission", path: "/v1/chat/completions", selection: service.ErrNoAvailableAccounts, mutate: func(g *service.Group) {
			g.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"other"}}
		}, reason: "MODEL_NOT_ALLOWED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c, key, group, _, _ := hostedGatewayFixture(t, tc.path)
			if tc.mutate != nil {
				tc.mutate(group)
			}
			if tc.written {
				c.Writer.WriteHeaderNow()
			}
			if tc.cancel {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			switched, err := h.tryHostedFallback(c, c.Request.Context(), tc.selection, "gpt-5.2", tc.stream, tc.started, tc.ws)
			require.Equal(t, tc.switched, switched)
			if tc.reason == "cancelled" {
				require.ErrorIs(t, err, context.Canceled)
			} else if tc.reason != "" {
				require.Equal(t, tc.reason, infraerrors.Reason(err))
			} else {
				require.NoError(t, err)
			}
			if switched {
				require.True(t, key.HostingFallback)
				require.Equal(t, int64(9), *key.GroupID)
				require.NotEmpty(t, key.HostingFallbackReason)
			}
			if tc.started || tc.written || tc.cancel || tc.selection.Error() == "invalid request parameters" {
				require.False(t, key.HostingFallback)
			}
		})
	}
}

func TestHostingFallbackAndTurnsPreserveMonetaryLimits(t *testing.T) {
	h, c, key, _, users, keys := hostedGatewayFixture(t, "/v1/responses")
	users.user.Balance = 0
	switched, err := h.tryHostedFallback(c, c.Request.Context(), service.ErrNoAvailableAccounts, "gpt-5.2", false, false, false)
	require.False(t, switched)
	require.ErrorIs(t, err, service.ErrInsufficientBalance)
	users.user.Balance = 10
	now := time.Now()
	key.RateLimit5h = 1
	keys.data = &service.APIKeyRateLimitData{Usage5h: 1, Window5hStart: &now, Window1dStart: &now, Window7dStart: &now}
	require.ErrorIs(t, h.billingCacheService.CheckHostingTurnEligibility(c.Request.Context(), key.User, key, key.Group), service.ErrAPIKeyRateLimit5hExceeded)
}

func TestHostingPrimaryRetryBudgetEndsBeforeAnotherSelection(t *testing.T) {
	h, c, key, _, _, _ := hostedGatewayFixture(t, "/v1/responses")
	switches := 3
	require.False(t, markHostedPoolExhausted(key, &switches, 3))
	selection, _, err := h.selectHostedAccount(key, switches, 3, c.Request.Context(), nil, "", "", "gpt-5.2", nil, service.OpenAIUpstreamTransportAny, service.OpenAIEndpointCapabilityResponses, false, false, true)
	require.Nil(t, selection)
	require.ErrorIs(t, err, service.ErrNoAvailableAccounts)
	switched, err := h.tryHostedFallback(c, c.Request.Context(), err, "gpt-5.2", false, false, false)
	require.NoError(t, err)
	require.True(t, switched)
	require.True(t, markHostedPoolExhausted(key, &switches, 3), "fallback retains ordinary retry limits")
}

func TestHostingPartialUsageKeepsCapturedPathAndRate(t *testing.T) {
	rate := .03
	key := &service.APIKey{RouteMode: service.RouteModeHosted, HostingRate: &rate}
	snapshot := snapshotHostedKey(key)
	rate = .08
	key.HostingFallback = true
	require.Equal(t, .03, *snapshot.HostingRate)
	require.False(t, snapshot.HostingFallback)
	result := &service.OpenAIForwardResult{}
	require.False(t, shouldRecordHostedPartialUsage(snapshot, result))
	result.Usage.InputTokens = 100
	require.True(t, shouldRecordHostedPartialUsage(snapshot, result))
	result.UsageUnavailable = true
	require.False(t, shouldRecordHostedPartialUsage(snapshot, result))
}
