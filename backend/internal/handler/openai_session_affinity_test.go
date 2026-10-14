package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAISessionAffinityResponsesPublicLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(2)
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cache := repository.NewGatewayCache(client)
	accounts := &openAIWSFailoverHandlerAccountRepoStub{}
	for _, id := range []int64{11, 12} {
		accounts.accounts = append(accounts.accounts, service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Status: service.StatusActive, Schedulable: true, Concurrency: 5, Priority: int(20 - id), GroupIDs: []int64{groupID},
			Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": fmt.Sprintf("test-account-%d", id)}})
	}
	upstream := &borrowHTTPHandlerUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple, Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.OpenAISessionAffinityEnabled = true
	busy := map[int64]bool{}
	concurrency := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(_ context.Context, id int64, _ int, _ string) (bool, error) { return !busy[id], nil },
	})
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, cache, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	borrow := &borrowHTTPHandlerRepo{bindings: map[string]string{}, sessions: map[string]string{}, route: &service.BorrowRoute{
		ID: "route-original", TargetID: 11, ExpiresAt: time.Now().Add(time.Hour), Secret: service.BorrowRouteSecret{
			Endpoint: "https://chatgpt.com/backend-api/codex/responses", Cookie: http.Cookie{Name: "__oailb", Value: "test-cookie"}, Headers: http.Header{"Authorization": {"Bearer test-token"}},
		}}}
	borrow.route.Secret.Fingerprint, _ = service.NewBorrowFingerprint()
	service.ProvideAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil, gateway, &service.SettingService{}, nil, borrow, nil)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 2
	request := func(session, previous string, user, apiKey int64) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"prompt_cache_key":%q,"input":[{"role":"user","content":[{"type":"input_text","text":"full context"}]}]}`, session)
		if previous != "" {
			body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"previous_response_id":%q,"input":"full context"}`, previous)
		}
		c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(body))
		c.Request.Header.Set("session_id", session)
		c.Request.Header.Set("x-request-id", fmt.Sprintf("request-%d", len(upstream.bodies)))
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: apiKey, UserID: user, GroupID: &groupID, User: &service.User{ID: user, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: user})
		h.Responses(c)
		return w
	}
	check := func(session string, user, key, want int64) {
		t.Helper()
		w := request(session, "", user, key)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "response.completed")
		require.Equal(t, want, upstream.accountIDs[len(upstream.accountIDs)-1])
		require.Zero(t, borrow.leases)
	}
	// First turn establishes a borrowed owner; a missing route transfers it only
	// after the ordinary account succeeds. Route recovery cannot steal it back.
	check("conversation-a", 1, 101, 11)
	borrow.unavailable = true
	check("conversation-a", 1, 101, 12)
	borrow.unavailable = false
	check("conversation-a", 1, 101, 12)
	require.Equal(t, upstream.headers[1].Get("session_id"), upstream.headers[2].Get("session_id"))
	require.Equal(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").String(), gjson.GetBytes(upstream.bodies[2], "prompt_cache_key").String())
	// Identically named sessions of a different tenant, and new sessions, still
	// start in the borrowed tier rather than inheriting ordinary ownership.
	check("conversation-a", 2, 102, 11)
	check("conversation-b", 1, 101, 11)
	busy[11] = true
	check("conversation-b", 1, 101, 12)
	busy[11] = false
	check("conversation-b", 1, 101, 11)
	// Historical response ownership is independent of the cache-affinity owner.
	w := request("conversation-a", "resp_borrow_1", 2, 102)
	require.GreaterOrEqual(t, w.Code, 400, "another user cannot reuse the original response")
	require.Len(t, upstream.accountIDs, 7, "ownership rejection must happen before upstream transmission")
	// The switch restores previous borrowing preference without deleting response
	// ownership or cached affinity. Turning it back on recovers the healthy owner.
	cfg.Gateway.OpenAISessionAffinityEnabled = false
	check("conversation-a", 1, 101, 11)
	cfg.Gateway.OpenAISessionAffinityEnabled = true
	check("conversation-a", 1, 101, 12)
}
