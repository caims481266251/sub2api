package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hostingQuotaRepoStub struct {
	service.AccountRepository
	account *service.Account
	calls   int
}

func (r *hostingQuotaRepoStub) GetByID(context.Context, int64) (*service.Account, error) {
	r.calls++
	return r.account, nil
}

type hostingQuotaStub struct {
	usageCalls, queryCalls, resetCalls, recoveryCalls int
	force                                             bool
	cacheErr, resetErr, recoveryErr                   error
}

func (s *hostingQuotaStub) GetUsageForAccount(_ context.Context, account *service.Account, force ...bool) (*service.UsageInfo, error) {
	s.usageCalls++
	s.force = len(force) > 0 && force[0]
	return service.HostedAccountUsage(account), nil
}
func (s *hostingQuotaStub) QueryUsage(context.Context, int64) (*service.OpenAIQuotaUsage, error) {
	s.queryCalls++
	return &service.OpenAIQuotaUsage{RateLimitResetCredits: &service.OpenAIRateLimitResetCredits{AvailableCount: 2}}, nil
}
func (s *hostingQuotaStub) ResetCredit(context.Context, int64) (*service.OpenAIQuotaResetResult, error) {
	s.resetCalls++
	return &service.OpenAIQuotaResetResult{Code: "ok", WindowsReset: 2}, s.resetErr
}
func (s *hostingQuotaStub) CacheCreditsSnapshot(context.Context, int64, *service.OpenAIQuotaUsage) error {
	return s.cacheErr
}
func (s *hostingQuotaStub) CacheResetCreditsSnapshot(context.Context, int64, *service.OpenAIRateLimitResetCredits) error {
	return s.cacheErr
}
func (s *hostingQuotaStub) CachePostResetSnapshot(context.Context, int64, *service.OpenAIQuotaUsage) error {
	return s.cacheErr
}
func (s *hostingQuotaStub) RecoverAccountState(context.Context, int64, service.AccountRecoveryOptions) (*service.SuccessfulTestRecoveryResult, error) {
	s.recoveryCalls++
	return &service.SuccessfulTestRecoveryResult{}, s.recoveryErr
}

func hostedQuotaFixture() *service.Account {
	owner := int64(7)
	return &service.Account{ID: 17, OwnerUserID: &owner, Name: "Codex", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "secret-token", "password": "secret-password"},
		Extra: map[string]any{
			"codex_5h_used_percent": 25.0, "codex_5h_reset_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"codex_7d_used_percent": 85.0, "codex_7d_reset_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339),
			"openai_prism_browser": true, "openai_excel_bps_proxy_source": "secret-proxy",
			"codex_reset_credit_snapshot": map[string]any{"available_count": 2, "access_token": "nested-secret", "credits": []map[string]any{{"expires_at": "2099-01-01T00:00:00Z", "id": "hidden-credit-id"}}},
		},
	}
}

func quotaRequest(h *HostingHandler, userID int64, accountID, action string, admin bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/hosting/accounts/"+accountID+"/"+action+"?force=true", nil)
	c.Params = gin.Params{{Key: "id", Value: accountID}}
	if userID > 0 {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
	}
	switch action {
	case "usage":
		h.accountUsage(c, admin)
	case "refresh":
		h.refreshQuota(c, admin)
	case "reset":
		h.resetQuota(c, admin)
	}
	return w
}

func TestHostingQuotaAccessIsolation(t *testing.T) {
	for _, action := range []string{"usage", "refresh", "reset"} {
		for _, test := range []struct {
			name                   string
			userID                 int64
			platformAccount, admin bool
			status                 int
		}{
			{"owner", 7, false, false, http.StatusOK},
			{"other_user", 8, false, false, http.StatusNotFound},
			{"platform", 7, true, false, http.StatusNotFound},
			{"unauthenticated", 0, false, false, http.StatusUnauthorized},
			{"admin_hosted", 1, false, true, http.StatusOK},
			{"admin_platform", 1, true, true, http.StatusNotFound},
		} {
			t.Run(action+"/"+test.name, func(t *testing.T) {
				account := hostedQuotaFixture()
				if test.platformAccount {
					account.OwnerUserID = nil
				}
				repo, operations := &hostingQuotaRepoStub{account: account}, &hostingQuotaStub{}
				h := &HostingHandler{accounts: repo, usage: operations, quota: operations, quotaRecovery: operations}
				w := quotaRequest(h, test.userID, "17", action, test.admin)
				require.Equal(t, test.status, w.Code, w.Body.String())
				if test.status != http.StatusOK {
					require.Zero(t, operations.usageCalls+operations.queryCalls+operations.resetCalls+operations.recoveryCalls)
				} else {
					require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
					if action == "usage" {
						require.True(t, operations.force)
					}
				}
				for _, secret := range []string{"secret-token", "secret-password", "secret-proxy", "nested-secret", "hidden-credit-id", "openai_prism_browser"} {
					require.NotContains(t, w.Body.String(), secret)
				}
			})
		}
	}
}

func TestHostingQuotaInvalidIDsAndUnsupportedAccount(t *testing.T) {
	for _, id := range []string{"0", "-1", "bad"} {
		repo := &hostingQuotaRepoStub{account: hostedQuotaFixture()}
		w := quotaRequest(&HostingHandler{accounts: repo}, 7, id, "usage", false)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Zero(t, repo.calls)
	}
	for _, kind := range []string{"missing", "shadow", "apikey", "anthropic"} {
		account := hostedQuotaFixture()
		switch kind {
		case "missing":
			account = nil
		case "shadow":
			parent := int64(18)
			account.ParentAccountID = &parent
		case "apikey":
			account.Type = service.AccountTypeAPIKey
		case "anthropic":
			account.Platform = service.PlatformAnthropic
		}
		w := quotaRequest(&HostingHandler{accounts: &hostingQuotaRepoStub{account: account}}, 7, "17", "reset", false)
		require.Equal(t, http.StatusNotFound, w.Code)
	}
}

func TestHostingQuotaPartialFailuresDoNotRepeatConsumption(t *testing.T) {
	for _, stage := range []string{"reset", "recovery", "cache"} {
		t.Run(stage, func(t *testing.T) {
			ops := &hostingQuotaStub{}
			switch stage {
			case "reset":
				ops.resetErr = errors.New("reset failed")
			case "recovery":
				ops.recoveryErr = errors.New("recovery failed")
			case "cache":
				ops.cacheErr = errors.New("cache failed")
			}
			h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: hostedQuotaFixture()}, usage: ops, quota: ops, quotaRecovery: ops}
			w := quotaRequest(h, 7, "17", "reset", false)
			require.Equal(t, 1, ops.resetCalls)
			if stage == "reset" {
				require.Equal(t, http.StatusInternalServerError, w.Code)
				require.Zero(t, ops.recoveryCalls)
			} else {
				require.Equal(t, http.StatusOK, w.Code)
				require.Contains(t, w.Body.String(), "warning_code")
				require.Contains(t, w.Body.String(), `"windows_reset":2`)
			}
		})
	}
	operations := &hostingQuotaStub{cacheErr: errors.New("cache failed")}
	h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: hostedQuotaFixture()}, quota: operations}
	w := quotaRequest(h, 7, "17", "refresh", false)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"available_count":2`)
	require.Contains(t, w.Body.String(), `"cache_persisted":false`)
	require.Contains(t, w.Body.String(), `"credits_cache_persisted":false`)
}

func TestHostingQuotaSnapshotUsesAdminWindowsAndSafeFields(t *testing.T) {
	account := hostedQuotaFixture()
	view := hostedAccountView(account)
	require.Equal(t, 25.0, view.Usage.FiveHour.Utilization)
	require.Equal(t, 85.0, view.Usage.SevenDay.Utilization)
	require.NotEqual(t, view.Usage.FiveHour.ResetsAt, view.Usage.SevenDay.ResetsAt)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	for _, secret := range []string{"secret-token", "secret-password", "secret-proxy", "nested-secret", "hidden-credit-id"} {
		require.NotContains(t, string(raw), secret)
	}
	account.Extra["codex_5h_reset_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	view = hostedAccountView(account)
	require.Zero(t, view.Usage.FiveHour.Utilization)
	delete(account.Extra, "codex_5h_used_percent")
	view = hostedAccountView(account)
	require.Nil(t, view.Usage.FiveHour)
}
