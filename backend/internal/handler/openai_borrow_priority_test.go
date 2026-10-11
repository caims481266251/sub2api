package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type disappearingBorrowAccountRepo struct {
	*openAIWSFailoverHandlerAccountRepoStub
	borrow     *borrowHTTPHandlerRepo
	admissions []int64
}

func (r *disappearingBorrowAccountRepo) GetOpenAITurnAdmission(ctx context.Context, id int64) (*service.Account, *service.Account, error) {
	r.admissions = append(r.admissions, id)
	if id == 11 {
		r.borrow.unavailable = true
	}
	return r.openAIWSFailoverHandlerAccountRepoStub.GetOpenAITurnAdmission(ctx, id)
}

func TestOpenAIBorrowPriorityReselectsRouteLostBeforeForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(2)
	borrowRepo := &borrowHTTPHandlerRepo{}
	accounts := &disappearingBorrowAccountRepo{borrow: borrowRepo, openAIWSFailoverHandlerAccountRepoStub: &openAIWSFailoverHandlerAccountRepoStub{}}
	for _, id := range []int64{11, 12} {
		accounts.accounts = append(accounts.accounts, service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 5, Priority: int(20 - id), GroupIDs: []int64{groupID}, Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-target"}})
	}
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
	service.ProvideAccountTestService(accounts, nil, nil, nil, nil, upstream, cfg, nil, gateway, &service.SettingService{}, nil, borrowRepo, nil)
	t.Cleanup(gateway.CloseOpenAIWSPool)
	h := NewOpenAIGatewayHandler(gateway, concurrency, billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	h.maxAccountSwitches = 2
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`))
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 101, UserID: 1, GroupID: &groupID, User: &service.User{ID: 1, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	h.Responses(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "response.completed")
	require.Equal(t, int64(11), accounts.admissions[0], "borrowing wins over ordinary priority")
	require.Equal(t, []int64{12}, upstream.accountIDs, "no send to stale route, exactly one business execution")
	require.Zero(t, borrowRepo.leases)
}
