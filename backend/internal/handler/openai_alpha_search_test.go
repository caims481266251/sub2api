//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type alphaSearchAdmissionRepo struct{ service.TicketBorrowRepository }

func (alphaSearchAdmissionRepo) Target(_ context.Context, id int64) (service.BorrowTarget, error) {
	return service.BorrowTarget{AccountID: id, Enabled: id == 1}, nil
}

type alphaSearchAdmissionUpstream struct {
	service.HTTPUpstream
	borrow   *service.TicketBorrowService
	attempts []int64
	sent     []int64
}

func (u *alphaSearchAdmissionUpstream) Do(req *http.Request, proxy string, id int64, n int) (*http.Response, error) {
	u.attempts = append(u.attempts, id)
	return u.borrow.ForwardHTTP(req, proxy, id, n, nil, func(req *http.Request, _ string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		u.sent = append(u.sent, id)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":"search result"}`))}, nil
	})
}

func TestAlphaSearchReselectsAfterBorrowingEnabledBeforeSend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, bounded := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary-success", true: "switch-budget"}[bounded], func(t *testing.T) {
			upstream := &alphaSearchAdmissionUpstream{borrow: service.NewTicketBorrowService(alphaSearchAdmissionRepo{}, nil, nil)}
			repo := &initialAdmissionRepo{}
			h := newInitialAdmissionHandler(t, repo, upstream)
			for i := range repo.accounts {
				repo.accounts[i].Type = service.AccountTypeAPIKey
				repo.accounts[i].Credentials = map[string]any{"api_key": "test-only", "base_url": "https://example.test/v1"}
				repo.accounts[i].Extra = nil
			}
			usage := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 3)}
			h.cfg.RunMode = config.RunModeSimple
			h.gatewayService = service.NewOpenAIGatewayService(repo, nil, usage, nil, nil, nil, nil, nil, h.cfg,
				nil, service.NewConcurrencyService(repo.slots), service.NewBillingService(h.cfg, nil), nil, h.billingCacheService,
				upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			if bounded {
				h.maxAccountSwitches = 0
			}
			c, rec := newExcelBPSFailoverTestContext(t.Context(), false)
			body := `{"model":"gpt-6-astra","commands":{"search_query":[{"q":"news"}]}}`
			c.Request.Body = io.NopCloser(strings.NewReader(body))
			c.Request.ContentLength = int64(len(body))
			c.Request.URL.Path = "/v1/alpha/search"
			h.AlphaSearch(c)
			if bounded {
				require.Empty(t, upstream.sent)
				require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
				require.Empty(t, usage.created)
			} else {
				require.Equal(t, []int64{1, 2}, upstream.attempts)
				require.Equal(t, []int64{2}, upstream.sent)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "search result")
				require.Len(t, usage.created, 1, "only the successful search is charged")
			}
			require.Equal(t, repo.slots.accountAcquireCalls, repo.slots.accountReleaseCalls)
			_, hasUpstreamError := c.Get(service.OpsUpstreamErrorsKey)
			require.False(t, hasUpstreamError, "local admission must not damage upstream health")
		})
	}
}
