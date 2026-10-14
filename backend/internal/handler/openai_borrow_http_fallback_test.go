package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const borrowFallbackFullInput = `[{"role":"user","content":"hello"},{"role":"assistant","content":"OK"},{"role":"user","content":"continue"}]`

type borrowHTTPFallbackAccounts struct {
	*openAIWSFailoverHandlerAccountRepoStub
}

func (r borrowHTTPFallbackAccounts) ListModelAvailabilityCandidates(_ context.Context, group *int64, platforms []string, includeGrouped bool) ([]service.Account, error) {
	var result []service.Account
	for _, a := range r.accounts {
		if a.Status == service.StatusActive && a.Schedulable && slices.Contains(platforms, a.Platform) &&
			(includeGrouped || (group != nil && slices.Contains(a.GroupIDs, *group)) || (group == nil && len(a.GroupIDs) == 0)) {
			result = append(result, a)
		}
	}
	return result, nil
}

func newBorrowHTTPFallbackHandler(t *testing.T) (*OpenAIGatewayHandler, *borrowHTTPHandlerRepo, *borrowHTTPHandlerUpstream, *openAIWSFailoverHandlerAccountRepoStub) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	accounts := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{
		{ID: 11, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 10, GroupIDs: []int64{2}, Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-target"}},
		{ID: 12, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 10, GroupIDs: []int64{2}, Credentials: map[string]any{"api_key": "test-key", "base_url": "https://api.example.com", "model_mapping": map[string]any{"gpt-6-astra": "gpt-6-astra"}}, Extra: map[string]any{"openai_responses_supported": true}},
	}}
	upstream := &borrowHTTPHandlerUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple, Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	cfg.Default.RateMultiplier = 1
	var acquired atomic.Int32
	slots := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { acquired.Add(1); return true, nil },
	}
	t.Cleanup(func() {
		require.Equal(t, acquired.Load(), atomic.LoadInt32(&slots.releaseAccountCalled), "all account slots must be released")
	})
	concurrency := service.NewConcurrencyService(slots)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	accountRepo := borrowHTTPFallbackAccounts{accounts}
	gateway := service.NewOpenAIGatewayService(accountRepo, nil, nil, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	repo := &borrowHTTPHandlerRepo{bindings: map[string]string{}, sessions: map[string]string{}, route: &service.BorrowRoute{ID: "original-route", TargetID: 11, ExpiresAt: time.Now().Add(time.Minute), Secret: service.BorrowRouteSecret{Endpoint: "https://chatgpt.com/backend-api/codex/responses", Cookie: http.Cookie{Name: "__oailb", Value: "test-route"}}}}
	repo.route.Secret.Fingerprint, _ = service.NewBorrowFingerprint()
	service.ProvideAccountTestService(accountRepo, nil, nil, nil, nil, upstream, cfg, nil, gateway, &service.SettingService{}, nil, repo, nil)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	return h, repo, upstream, accounts
}

func borrowFallbackRequest(t *testing.T, h *OpenAIGatewayHandler, previous, input string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":%s}`, stream, input)
	if previous != "" {
		body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"previous_response_id":%q,"input":%s}`, stream, previous, input)
	}
	c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("session_id", "original-client-session")
	c.Request.Header.Set("X-Codex-Turn-State", "old-turn")
	c.Request.Header.Set("X-Codex-Turn-Metadata", "old-metadata")
	group := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 101, UserID: 1, GroupID: &group, User: &service.User{ID: 1, Status: service.StatusActive}, Group: &service.Group{ID: group, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	h.Responses(c)
	return w
}

func TestOpenAIResponses_BorrowHTTPContinuationFallsBackBeforeSend(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, failure := range []string{"no_route", "missing_binding", "reservation_race", "before_transport_race"} {
			t.Run(fmt.Sprintf("%s/stream=%v", failure, stream), func(t *testing.T) {
				h, repo, upstream, _ := newBorrowHTTPFallbackHandler(t)
				w := borrowFallbackRequest(t, h, "", `"hello"`, stream)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Equal(t, []int64{11}, upstream.accountIDs)
				switch failure {
				case "no_route":
					repo.unavailable = true
				case "missing_binding":
					clear(repo.bindings)
				case "reservation_race":
					repo.reserveErr = service.ErrBorrowNoEligibleRoute
				case "before_transport_race":
					repo.beforeSendErr = service.ErrBorrowNoEligibleRoute
				}
				start := time.Now()
				w = borrowFallbackRequest(t, h, "resp_borrow_1", borrowFallbackFullInput, stream)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Less(t, time.Since(start), 2*time.Second, "healthy normal account must precede route waiting")
				require.Equal(t, []int64{11, 12}, upstream.accountIDs, "only one business send for the migrated request")
				require.Zero(t, repo.leases)
				require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
				require.JSONEq(t, borrowFallbackFullInput, gjson.GetBytes(upstream.bodies[1], "input").Raw)
				require.NotEqual(t, upstream.headers[0].Get("session_id"), upstream.headers[1].Get("session_id"))
				require.Empty(t, upstream.headers[1].Get("X-Codex-Turn-State"))
				require.Empty(t, upstream.headers[1].Get("X-Codex-Turn-Metadata"))
				w = borrowFallbackRequest(t, h, "resp_borrow_2", `"next increment"`, stream)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.Equal(t, []int64{11, 12, 12}, upstream.accountIDs, "new response ownership follows the actual fallback account")
				require.Equal(t, "resp_borrow_2", gjson.GetBytes(upstream.bodies[2], "previous_response_id").String())
			})
		}
	}
}

func TestOpenAIResponses_BorrowHTTPContinuationRefusesUnsafeMigration(t *testing.T) {
	for _, scenario := range []string{"incremental", "altered_history", "encrypted", "storage_failure", "switch_budget", "normal_account_unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			h, repo, upstream, accounts := newBorrowHTTPFallbackHandler(t)
			w := borrowFallbackRequest(t, h, "", `"hello"`, true)
			require.Equal(t, 200, w.Code, w.Body.String())
			repo.unavailable = true
			input := borrowFallbackFullInput
			switch scenario {
			case "incremental":
				input = `"continue"`
			case "altered_history":
				input = strings.Replace(input, "hello", "different", 1)
			case "encrypted":
				input = strings.TrimSuffix(input, "]") + `,{"type":"reasoning","encrypted_content":"opaque"}]`
			case "storage_failure":
				repo.checkErr = errors.New("storage down")
			case "switch_budget":
				h.maxAccountSwitches = 0
			case "normal_account_unavailable":
				accounts.accounts[1].Schedulable = false
			}
			w = borrowFallbackRequest(t, h, "resp_borrow_1", input, true)
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			require.Equal(t, []int64{11}, upstream.accountIDs)
			require.Zero(t, repo.leases)
			if scenario == "incremental" || scenario == "altered_history" || scenario == "encrypted" {
				require.Contains(t, w.Body.String(), "borrow_continuation_unavailable")
				// Refusal only limits migration; the original target remains usable.
				repo.unavailable = false
				w = borrowFallbackRequest(t, h, "resp_borrow_1", input, true)
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, []int64{11, 11}, upstream.accountIDs)
			}
		})
	}
}

func TestOpenAIResponses_BorrowHTTPContinuationCanSelectAnotherBorrowedAccount(t *testing.T) {
	h, repo, upstream, accounts := newBorrowHTTPFallbackHandler(t)
	w := borrowFallbackRequest(t, h, "", `"hello"`, true)
	require.Equal(t, 200, w.Code, w.Body.String())
	repo.unavailable, repo.secondBorrowed = true, true
	accounts.accounts[1].Type = service.AccountTypeOAuth
	accounts.accounts[1].Credentials = map[string]any{"access_token": "second-token", "chatgpt_account_id": "second-target"}
	w = borrowFallbackRequest(t, h, "resp_borrow_1", borrowFallbackFullInput, true)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, []int64{11, 12}, upstream.accountIDs)
	require.Zero(t, repo.leases)
}

func TestOpenAIResponses_BorrowHTTPContinuationDoesNotGainReplayAfterUpstreamFailure(t *testing.T) {
	h, repo, upstream, _ := newBorrowHTTPFallbackHandler(t)
	w := borrowFallbackRequest(t, h, "", `"hello"`, true)
	require.Equal(t, 200, w.Code, w.Body.String())
	upstream.failureStatus = http.StatusServiceUnavailable
	w = borrowFallbackRequest(t, h, "resp_borrow_1", borrowFallbackFullInput, true)
	require.GreaterOrEqual(t, w.Code, 400, w.Body.String())
	require.GreaterOrEqual(t, len(upstream.accountIDs), 2)
	for _, id := range upstream.accountIDs {
		require.EqualValues(t, 11, id, "a real upstream failure must not acquire new cross-account replay permission")
	}
	require.Zero(t, repo.leases)
}
