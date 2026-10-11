package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBorrowUnavailableSelectionPreservesExistingScheduling(t *testing.T) {
	for _, mode := range []string{"legacy", "load_batch", "advanced"} {
		for _, available := range []bool{false, true} {
			t.Run(mode+"/available="+map[bool]string{false: "false", true: "true"}[available], func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
				borrow, repo := borrowFixture()
				if !available {
					repo.route = nil
				}
				a, err := borrow.owner.accountRepo.GetByID(t.Context(), 11)
				require.NoError(t, err)
				a.Concurrency = 1
				cfg := &config.Config{}
				cfg.Gateway.Scheduling.LoadBatchEnabled = mode == "load_batch"
				svc := &OpenAIGatewayService{
					accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*a}},
					cache:       &schedulerTestGatewayCache{}, cfg: cfg, borrowRoutes: borrow,
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
				}
				if mode == "advanced" {
					svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
				}
				selection, _, err := svc.SelectAccountWithScheduler(t.Context(), nil, "", "", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
				if available {
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, int64(11), selection.Account.ID)
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
				} else {
					require.Nil(t, selection)
					require.ErrorIs(t, err, ErrNoAvailableAccounts)
					require.True(t, IsBorrowRouteUnavailable(err), "%v", err)
				}
				require.Empty(t, repo.events, "selection must not start borrowing or write borrow events")
			})
		}
	}
}

func TestBorrowSelectionFailureOnlyAnnotatesNoAccountResult(t *testing.T) {
	ctx, finish := withBorrowSelectionFailure(t.Context())
	observeBorrowSelectionFailure(ctx, ErrBorrowUnavailable)
	require.NoError(t, finish(nil), "a later successful selection stays successful")
	other := errors.New("database unavailable")
	require.Same(t, other, finish(other))
	require.ErrorIs(t, finish(ErrNoAvailableCompactAccounts), ErrNoAvailableCompactAccounts)
	fresh, freshFinish := withBorrowSelectionFailure(t.Context())
	observeBorrowSelectionFailure(fresh, other)
	require.False(t, IsBorrowRouteUnavailable(freshFinish(ErrNoAvailableAccounts)), "no cross-request state")
}

type borrowAdmissionTestUpstream struct {
	HTTPUpstream
	borrow *TicketBorrowService
	send   func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error)
}

func (u *borrowAdmissionTestUpstream) Do(req *http.Request, proxy string, id int64, n int) (*http.Response, error) {
	return u.borrow.ForwardHTTP(req, proxy, id, n, nil, u.send)
}

func TestBorrowAdmissionAfterEarlierHTTPAttemptCannotAuthorizeReplay(t *testing.T) {
	for _, firstResponse := range []bool{false, true} {
		t.Run(map[bool]string{false: "inconclusive_transport", true: "http_error_response"}[firstResponse], func(t *testing.T) {
			borrow, repo := borrowFixture()
			upstream := &borrowAdmissionTestUpstream{borrow: borrow, send: func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.NoError(t, BorrowBeforeTransport(req))
				if firstResponse {
					return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("first failure"))}, nil
				}
				return nil, errors.New("connection closed after a possible write")
			}}
			gateway := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account, err := borrow.owner.accountRepo.GetByID(t.Context(), 11)
			require.NoError(t, err)
			ctx := withOpenAIInitialAdmission(t.Context())
			req := borrowRequest(t).WithContext(ctx)
			resp, err := gateway.doOpenAIUpstream(req, "", account)
			if firstResponse {
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			} else {
				require.Error(t, err)
			}
			repo.mu.Lock()
			repo.route = nil
			repo.mu.Unlock()
			_, err = gateway.doOpenAIUpstream(borrowRequest(t).WithContext(withOpenAIInitialAdmission(ctx)), "", account)
			require.ErrorIs(t, err, ErrBorrowUnavailable)
			require.False(t, IsOpenAIInitialAdmissionRejection(err), "a later reservation failure cannot erase the first upstream attempt")
			require.Zero(t, activeBorrowLeases(repo))
		})
	}
}

func TestBorrowBeforeTransportOnlyProvesTheFirstUnsentAttempt(t *testing.T) {
	for _, enteredTransport := range []bool{false, true} {
		t.Run(map[bool]string{false: "first_preflight", true: "transport_retry"}[enteredTransport], func(t *testing.T) {
			borrow, repo := borrowFixture()
			_, err := borrow.ForwardHTTP(borrowRequest(t), "", 11, 1, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				if enteredTransport {
					require.NoError(t, BorrowBeforeTransport(req))
				}
				repo.mu.Lock()
				repo.route = nil
				repo.mu.Unlock()
				return nil, BorrowBeforeTransport(req)
			})
			require.ErrorIs(t, err, ErrBorrowUnavailable)
			marked := markOpenAIBorrowPreSendRejection(err, true)
			require.Equal(t, !enteredTransport, IsOpenAIInitialAdmissionRejection(marked))
			require.Zero(t, activeBorrowLeases(repo))
		})
	}
}

func TestBorrowInvalidRoutePreservesUpstreamErrorBodyAndLease(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			borrow, repo := borrowFixture()
			var transport context.Context
			resp, err := borrow.ForwardHTTP(borrowRequest(t), "", 11, 1, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				transport = req.Context()
				return &http.Response{StatusCode: status, Header: http.Header{"Set-Cookie": {"__oailb=changed; Path=/"}, "Retry-After": {"12"}},
					Body: io.NopCloser(strings.NewReader(`{"error":{"message":"provider failure"}}`))}, nil
			})
			require.NoError(t, err)
			require.Equal(t, status, resp.StatusCode)
			require.Equal(t, "12", resp.Header.Get("Retry-After"))
			require.NoError(t, transport.Err(), "lease ownership must survive until ordinary error parsing finishes")
			require.Equal(t, 1, activeBorrowLeases(repo))
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Contains(t, string(body), "provider failure")
			require.NoError(t, resp.Body.Close())
			require.Zero(t, activeBorrowLeases(repo))
			require.Equal(t, []string{"route-a"}, repo.revoked)
			require.Equal(t, 1, repo.releases)
		})
	}
}

func TestBorrowRejectionIsNotOverwrittenByResponseHeaderTimeout(t *testing.T) {
	for _, responseReceived := range []bool{false, true} {
		t.Run(map[bool]string{false: "local_rejection", true: "response_identity_changed"}[responseReceived], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				borrow, repo := borrowFixture()
				upstream := &borrowAdmissionTestUpstream{borrow: borrow, send: func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
					<-req.Context().Done()
					if !responseReceived {
						return nil, ErrBorrowUnavailable
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Set-Cookie": {"__oailb=changed; Path=/"}}, Body: http.NoBody}, nil
				}}
				gateway := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
					OpenAIFirstOutputTimeoutSeconds: 1, MaxLineSize: defaultMaxLineSize,
				}}, httpUpstream: upstream, borrowRoutes: borrow, accountRepo: borrow.owner.accountRepo}
				account, err := borrow.owner.accountRepo.GetByID(t.Context(), 11)
				require.NoError(t, err)
				body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				_, err = gateway.Forward(t.Context(), c, account, body)
				if responseReceived {
					require.ErrorIs(t, err, ErrBorrowResponseRejected)
				} else {
					require.ErrorIs(t, err, ErrBorrowUnavailable)
				}
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "header timeout cannot turn an unproven or already-sent request into failover")
				require.False(t, IsOpenAIInitialAdmissionRejection(err))
				require.Empty(t, rec.Body.String())
				require.Zero(t, activeBorrowLeases(repo))
			})
		})
	}
}
