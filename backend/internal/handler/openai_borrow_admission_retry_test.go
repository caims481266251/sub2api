//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type borrowAdmissionRetryRepo struct {
	service.TicketBorrowRepository
	mu            sync.Mutex
	enabled       map[int64]bool
	checkErr      map[int64]error
	reserveErr    map[int64]error
	beforeSendErr map[int64]error
	reserved      []int64
	leases        map[string]int64
	released      int
	revoked       []int64
}

func newBorrowAdmissionRetryRepo() *borrowAdmissionRetryRepo {
	return &borrowAdmissionRetryRepo{
		enabled: map[int64]bool{1: true}, checkErr: map[int64]error{}, reserveErr: map[int64]error{},
		beforeSendErr: map[int64]error{}, leases: map[string]int64{},
	}
}

func (r *borrowAdmissionRetryRepo) Target(_ context.Context, id int64) (service.BorrowTarget, error) {
	return service.BorrowTarget{AccountID: id, Enabled: r.enabled[id]}, nil
}
func (r *borrowAdmissionRetryRepo) Targets(context.Context) ([]service.BorrowTarget, error) {
	var targets []service.BorrowTarget
	for id, enabled := range r.enabled {
		if enabled {
			targets = append(targets, service.BorrowTarget{AccountID: id, Enabled: true})
		}
	}
	return targets, nil
}
func (r *borrowAdmissionRetryRepo) CheckLatestRoute(_ context.Context, id int64, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkErr[id]
}
func (r *borrowAdmissionRetryRepo) route(id int64) *service.BorrowRoute {
	return &service.BorrowRoute{ID: fmt.Sprintf("route-%d", id), TargetID: id, ExpiresAt: time.Now().Add(time.Minute),
		Secret: service.BorrowRouteSecret{Endpoint: "https://chatgpt.com/backend-api/codex/responses", Cookie: http.Cookie{Name: "__oailb", Value: "borrowed-route"}}}
}
func (r *borrowAdmissionRetryRepo) ReserveLatestRoute(_ context.Context, id int64, _, _, _, lease string) (*service.BorrowContinuationRoute, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reserved = append(r.reserved, id)
	if err := r.reserveErr[id]; err != nil {
		return nil, err
	}
	r.leases[lease] = id
	return &service.BorrowContinuationRoute{Route: r.route(id)}, nil
}
func (r *borrowAdmissionRetryRepo) ReserveRoute(ctx context.Context, id int64, identity, _, session, lease string) (*service.BorrowRoute, error) {
	choice, err := r.ReserveLatestRoute(ctx, id, identity, session, "", lease)
	if err != nil {
		return nil, err
	}
	return choice.Route, nil
}
func (r *borrowAdmissionRetryRepo) Route(_ context.Context, id int64, _, _ string) (*service.BorrowRoute, error) {
	if err := r.beforeSendErr[id]; err != nil {
		return nil, err
	}
	return r.route(id), nil
}
func (r *borrowAdmissionRetryRepo) RenewRoute(context.Context, string) error { return nil }
func (r *borrowAdmissionRetryRepo) ReleaseRoute(_ context.Context, lease string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.leases, lease)
	r.released++
	return nil
}
func (r *borrowAdmissionRetryRepo) RouteStatuses(context.Context, int64, string) ([]service.BorrowRouteStatus, error) {
	return nil, nil
}
func (r *borrowAdmissionRetryRepo) CheckHTTPContinuation(context.Context, int64, string, string, string) error {
	return service.ErrBorrowUnavailable
}
func (r *borrowAdmissionRetryRepo) ReserveHTTPContinuation(context.Context, int64, string, string, string, string, string) (*service.BorrowContinuationRoute, error) {
	return nil, service.ErrBorrowUnavailable
}
func (r *borrowAdmissionRetryRepo) BindHTTPContinuationResponse(context.Context, int64, string, string, string) error {
	return nil
}
func (r *borrowAdmissionRetryRepo) Revoke(_ context.Context, id int64, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked = append(r.revoked, id)
	r.checkErr[id] = service.ErrBorrowUnavailable
	return nil
}
func (r *borrowAdmissionRetryRepo) RefreshTarget(context.Context, int64) error       { return nil }
func (r *borrowAdmissionRetryRepo) Event(context.Context, service.BorrowEvent) error { return nil }

type borrowAdmissionRetryUpstream struct {
	service.HTTPUpstream
	borrow          *service.TicketBorrowService
	sent            []int64
	status          int
	changedResponse bool
}

func (u *borrowAdmissionRetryUpstream) SetTicketBorrowService(s *service.TicketBorrowService) {
	u.borrow = s
}
func (u *borrowAdmissionRetryUpstream) Do(req *http.Request, proxy string, id int64, n int) (*http.Response, error) {
	return u.borrow.ForwardHTTP(req, proxy, id, n, nil, func(sent *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		if err := service.BorrowBeforeTransport(sent); err != nil {
			return nil, err
		}
		u.sent = append(u.sent, id)
		if id == 1 && u.status != 0 {
			header := http.Header{"Content-Type": {"application/json"}}
			if u.changedResponse {
				header.Set("Set-Cookie", "__oailb=changed; Path=/")
			}
			return &http.Response{StatusCode: u.status, Header: header,
				Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"temporary upstream failure"}}`))}, nil
		}
		resp := excelBPSCompleted()
		wire, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		delta := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"done\",\"output_index\":0,\"content_index\":0}\n\n"
		resp.Body = io.NopCloser(strings.NewReader(delta + string(wire)))
		return resp, nil
	})
}

func newBorrowAdmissionRetryHandler(t *testing.T, borrow *borrowAdmissionRetryRepo, upstream *borrowAdmissionRetryUpstream) (*OpenAIGatewayHandler, *initialAdmissionRepo) {
	t.Helper()
	accounts := &initialAdmissionRepo{}
	h := newInitialAdmissionHandler(t, accounts, upstream)
	h.cfg.Runtime.Role = config.RuntimeRoleGateway
	for i := range accounts.accounts {
		accounts.accounts[i].Extra = nil
	}
	service.ProvideAccountTestService(accounts, nil, nil, nil, nil, upstream, h.cfg, nil, h.gatewayService, &service.SettingService{}, nil, borrow, nil)
	t.Cleanup(h.gatewayService.CloseOpenAIWSPool)
	return h, accounts
}

func runBorrowAdmissionRequest(t *testing.T, h *OpenAIGatewayHandler, endpoint string, stream bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, rec := newExcelBPSFailoverTestContext(t.Context(), stream)
	key, _ := middleware.GetAPIKeyFromContext(c)
	key.Group.AllowMessagesDispatch = true
	c.Request.URL.Path = "/v1/" + endpoint
	if endpoint != "responses" {
		body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, stream)
		c.Request.Body = io.NopCloser(strings.NewReader(body))
		c.Request.ContentLength = int64(len(body))
	}
	switch endpoint {
	case "responses":
		h.Responses(c)
	case "chat/completions":
		h.ChatCompletions(c)
	case "messages":
		h.Messages(c)
	}
	return c, rec
}

func TestBorrowAdmissionUsesOrdinaryAccountReselection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, stage := range []string{"entry", "reserve", "before_transport"} {
				for _, borrowedFallback := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t/%s/borrowed_fallback=%t", endpoint, stream, stage, borrowedFallback), func(t *testing.T) {
						borrow := newBorrowAdmissionRetryRepo()
						borrow.enabled[2] = borrowedFallback
						upstream := &borrowAdmissionRetryUpstream{}
						h, accounts := newBorrowAdmissionRetryHandler(t, borrow, upstream)
						switch stage {
						case "entry":
							accounts.onRead = func() {
								borrow.mu.Lock()
								borrow.checkErr[1] = service.ErrBorrowUnavailable
								borrow.mu.Unlock()
							}
						case "reserve":
							borrow.reserveErr[1] = service.ErrBorrowUnavailable
						case "before_transport":
							borrow.beforeSendErr[1] = service.ErrBorrowUnavailable
						}
						c, rec := runBorrowAdmissionRequest(t, h, endpoint, stream)
						require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
						require.Contains(t, rec.Body.String(), "done")
						require.Equal(t, []int64{2}, upstream.sent, "only the eligible account executes the request")
						require.Equal(t, int64(1), accounts.initialReads[0])
						_, recorded := c.Get(service.OpsUpstreamErrorsKey)
						require.False(t, recorded, "local route capacity is not an upstream failure")
						accounts.slots.mu.Lock()
						require.Equal(t, 2, accounts.slots.accountAcquireCalls)
						require.Equal(t, accounts.slots.accountAcquireCalls, accounts.slots.accountReleaseCalls)
						accounts.slots.mu.Unlock()
						borrow.mu.Lock()
						require.Empty(t, borrow.leases)
						borrow.mu.Unlock()
					})
				}
			}
		}
	}
}

func TestBorrowAdmissionReselectionHonorsBudgetAndControlFailures(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, scenario := range []string{"budget", "all_unavailable", "database"} {
			t.Run(endpoint+"/"+scenario, func(t *testing.T) {
				borrow := newBorrowAdmissionRetryRepo()
				borrow.enabled[2] = true
				borrow.reserveErr[1] = service.ErrBorrowUnavailable
				upstream := &borrowAdmissionRetryUpstream{}
				h, accounts := newBorrowAdmissionRetryHandler(t, borrow, upstream)
				expected := []int64{1}
				switch scenario {
				case "budget":
					h.maxAccountSwitches = 0
				case "all_unavailable":
					borrow.reserveErr[2] = service.ErrBorrowUnavailable
					expected = []int64{1, 2}
				case "database":
					borrow.reserveErr[1] = errors.New("database unavailable")
				}
				_, rec := runBorrowAdmissionRequest(t, h, endpoint, false)
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				require.NotContains(t, rec.Body.String(), "rate_limit_error")
				require.Empty(t, upstream.sent)
				require.Equal(t, expected, borrow.reserved)
				accounts.slots.mu.Lock()
				require.Equal(t, len(expected), accounts.slots.accountAcquireCalls)
				require.Equal(t, accounts.slots.accountAcquireCalls, accounts.slots.accountReleaseCalls)
				accounts.slots.mu.Unlock()
				require.Empty(t, borrow.leases)
			})
		}
	}
}

func TestBorrowUpstreamErrorsKeepOrdinaryFailoverPolicy(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat/completions", "messages"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusOK} {
			t.Run(fmt.Sprintf("%s/%d", endpoint, status), func(t *testing.T) {
				borrow := newBorrowAdmissionRetryRepo()
				upstream := &borrowAdmissionRetryUpstream{status: status, changedResponse: true}
				h, accounts := newBorrowAdmissionRetryHandler(t, borrow, upstream)
				_, rec := runBorrowAdmissionRequest(t, h, endpoint, false)
				if status == http.StatusOK {
					require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
					require.Equal(t, []int64{1}, upstream.sent, "a successful request with changed routing cannot be executed twice")
				} else {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					require.Equal(t, []int64{1, 2}, upstream.sent)
				}
				require.Equal(t, []int64{1}, borrow.revoked)
				require.Empty(t, borrow.leases)
				require.Equal(t, 1, borrow.released)
				accounts.slots.mu.Lock()
				require.Equal(t, accounts.slots.accountAcquireCalls, accounts.slots.accountReleaseCalls)
				accounts.slots.mu.Unlock()
			})
		}
	}
}
