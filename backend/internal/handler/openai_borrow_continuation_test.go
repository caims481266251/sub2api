package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type borrowHTTPHandlerRepo struct {
	service.TicketBorrowRepository
	bindings       map[string]string
	sessions       map[string]string
	route          *service.BorrowRoute
	restart        bool
	leases         int
	unavailable    bool
	checkErr       error
	reserveErr     error
	beforeSendErr  error
	secondBorrowed bool
}

func (r *borrowHTTPHandlerRepo) Target(_ context.Context, id int64) (service.BorrowTarget, error) {
	return service.BorrowTarget{AccountID: id, Enabled: id == 11 || (id == 12 && r.secondBorrowed)}, nil
}
func (r *borrowHTTPHandlerRepo) Targets(context.Context) ([]service.BorrowTarget, error) {
	if r.secondBorrowed {
		return []service.BorrowTarget{{AccountID: 11, Enabled: true}, {AccountID: 12, Enabled: true}}, nil
	}
	return []service.BorrowTarget{{AccountID: 11, Enabled: true}}, nil
}
func (r *borrowHTTPHandlerRepo) Route(context.Context, int64, string, string) (*service.BorrowRoute, error) {
	if r.beforeSendErr != nil {
		return nil, r.beforeSendErr
	}
	return r.route, nil
}
func (r *borrowHTTPHandlerRepo) LookupSession(_ context.Context, _ int64, key string) (string, bool, error) {
	v, ok := r.bindings[key]
	return v, ok, nil
}
func (r *borrowHTTPHandlerRepo) BindSession(_ context.Context, _ int64, key, route string) (string, error) {
	r.bindings[key] = route
	return route, nil
}
func (r *borrowHTTPHandlerRepo) ReserveRoute(context.Context, int64, string, string, string, string) (*service.BorrowRoute, error) {
	r.leases++
	return r.route, nil
}
func (r *borrowHTTPHandlerRepo) RenewRoute(context.Context, string) error   { return nil }
func (r *borrowHTTPHandlerRepo) ReleaseRoute(context.Context, string) error { r.leases--; return nil }
func (r *borrowHTTPHandlerRepo) RouteStatuses(context.Context, int64, string) ([]service.BorrowRouteStatus, error) {
	return nil, nil
}
func (r *borrowHTTPHandlerRepo) CheckLatestRoute(_ context.Context, id int64, _, _ string) error {
	if r.unavailable && id == 11 {
		return service.ErrBorrowNoEligibleRoute
	}
	return nil
}
func (r *borrowHTTPHandlerRepo) ReserveLatestRoute(_ context.Context, id int64, _, session, fresh, _ string) (*service.BorrowContinuationRoute, error) {
	effective := r.sessions[session]
	bound := r.bindings[session]
	restarted := bound != "" && bound != r.route.ID
	if restarted {
		effective = fresh
	}
	if session != "" {
		r.bindings[session] = r.route.ID
		r.sessions[session] = effective
	}
	r.leases++
	route := *r.route
	route.TargetID = id
	return &service.BorrowContinuationRoute{Route: &route, SessionID: effective, Restarted: restarted}, nil
}
func (r *borrowHTTPHandlerRepo) CheckHTTPContinuation(_ context.Context, _ int64, _ string, response, _ string) error {
	if r.checkErr != nil {
		return r.checkErr
	}
	if r.bindings[response] == "" {
		return service.ErrBorrowContinuationBindingMissing
	}
	if r.unavailable {
		return service.ErrBorrowNoEligibleRoute
	}
	return nil
}
func (r *borrowHTTPHandlerRepo) ReserveHTTPContinuation(ctx context.Context, id int64, identity, response, session, fresh, lease string) (*service.BorrowContinuationRoute, error) {
	if r.reserveErr != nil {
		return nil, r.reserveErr
	}
	if err := r.CheckHTTPContinuation(ctx, id, identity, response, session); err != nil {
		return nil, err
	}
	effective := r.sessions[response]
	if r.restart {
		effective = fresh
	}
	r.leases++
	return &service.BorrowContinuationRoute{Route: r.route, SessionID: effective, Restarted: r.restart}, nil
}
func (r *borrowHTTPHandlerRepo) BindHTTPContinuationResponse(_ context.Context, _ int64, response, route, effective string) error {
	r.bindings[response] = route
	r.sessions[response] = effective
	return nil
}

type borrowHTTPHandlerUpstream struct {
	borrow        *service.TicketBorrowService
	bodies        [][]byte
	headers       []http.Header
	accountIDs    []int64
	failureStatus int
}

func (u *borrowHTTPHandlerUpstream) SetTicketBorrowService(s *service.TicketBorrowService) {
	u.borrow = s
}
func (u *borrowHTTPHandlerUpstream) Do(req *http.Request, proxy string, id int64, n int) (*http.Response, error) {
	return u.DoWithTLS(req, proxy, id, n, nil)
}
func (u *borrowHTTPHandlerUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, p *tlsfingerprint.Profile) (*http.Response, error) {
	return u.borrow.ForwardHTTP(req, proxy, id, n, p, func(r *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		if err := service.BorrowBeforeTransport(r); err != nil {
			return nil, err
		}
		recovery := service.BorrowBusinessRecoveryFromContext(r.Context())
		if err := recovery.BeginTransport(r.Context()); err != nil {
			return nil, err
		}
		if err := recovery.WritePossible(); err != nil {
			return nil, err
		}
		recovery.WroteHeaders(r.Context())
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		u.bodies = append(u.bodies, body)
		u.accountIDs = append(u.accountIDs, accountID)
		u.headers = append(u.headers, r.Header.Clone())
		if u.failureStatus != 0 && accountID == 11 {
			return &http.Response{StatusCode: u.failureStatus, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"upstream unavailable"}}`))}, nil
		}
		responseID := fmt.Sprintf("resp_borrow_%d", len(u.bodies))
		sse := fmt.Sprintf("data: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", responseID, responseID)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}, nil
	})
}

func TestOpenAIResponses_BorrowHTTPContinuationPublicLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(2)
	account := service.Account{ID: 11, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 10, GroupIDs: []int64{groupID}, Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-target"}}
	accounts := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}
	upstream := &borrowHTTPHandlerUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple, Runtime: config.RuntimeConfig{Role: config.RuntimeRoleGateway}}
	cfg.Default.RateMultiplier = 1
	concurrency := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	})
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, concurrency, service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	repo := &borrowHTTPHandlerRepo{bindings: map[string]string{}, sessions: map[string]string{}, route: &service.BorrowRoute{ID: "original-route", TargetID: 11, ExpiresAt: time.Now().Add(time.Minute), Secret: service.BorrowRouteSecret{Endpoint: "https://chatgpt.com/backend-api/codex/responses", Cookie: http.Cookie{Name: "__oailb", Value: "test-route"}, Headers: http.Header{"Authorization": []string{"Bearer test-token"}}}}}
	repo.route.Secret.Fingerprint, _ = service.NewBorrowFingerprint()
	service.ProvideAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil, gateway, &service.SettingService{}, nil, repo, nil)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	request := func(previous string, user int64, input string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"input":%s}`, input)
		if previous != "" {
			body = fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"previous_response_id":%q,"input":%s}`, previous, input)
		}
		c.Request = httptest.NewRequest("POST", "/openai/v1/responses", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("session_id", "original-client-session")
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 101, UserID: user, GroupID: &groupID, User: &service.User{ID: user, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: user, Concurrency: 0})
		h.Responses(c)
		return w
	}
	for _, previous := range []string{"", "resp_borrow_1"} {
		w := request(previous, 1, `"full current input"`)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "response.completed")
	}
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, upstream.headers[0].Get("session_id"), upstream.headers[1].Get("session_id"))
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
	repo.restart = true
	repo.route.ID = "replacement-route"
	w := request("resp_borrow_2", 1, `"current input after newer publication"`)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Len(t, upstream.bodies, 3)
	require.NotEqual(t, upstream.headers[1].Get("session_id"), upstream.headers[2].Get("session_id"))
	repo.restart = false
	w = request("resp_borrow_3", 1, `"next current input"`)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Len(t, upstream.bodies, 4)
	require.Equal(t, upstream.headers[2].Get("session_id"), upstream.headers[3].Get("session_id"))
	for _, tc := range []struct {
		response string
		user     int64
		input    string
	}{
		{"resp_borrow_4", 2, `"foreign user"`},
		{"resp_missing", 1, `"unknown response"`},
		{"resp_borrow_4", 1, `[{"type":"function_call_output","call_id":"missing","output":"not portable"}]`},
	} {
		w = request(tc.response, tc.user, tc.input)
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	require.Len(t, upstream.bodies, 4, "rejected requests must not reach upstream")
	require.Zero(t, repo.leases)
}
