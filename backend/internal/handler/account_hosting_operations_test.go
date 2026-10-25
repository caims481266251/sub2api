package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hostingMaintenanceStub struct {
	calls        int
	prompt, mode string
}

func (s *hostingMaintenanceStub) RefreshHostedCredentials(context.Context, *service.Account) error {
	s.calls++
	return nil
}
func (s *hostingMaintenanceStub) SetHostedPrivacy(context.Context, *service.Account) error {
	s.calls++
	return nil
}
func (s *hostingMaintenanceStub) GetAvailableModels(c *gin.Context) {
	s.calls++
	c.JSON(200, []string{"gpt-5"})
}
func (s *hostingMaintenanceStub) SyncUpstreamModels(c *gin.Context) {
	s.calls++
	c.JSON(200, gin.H{"models": []string{"gpt-5"}})
}
func (s *hostingMaintenanceStub) Test(c *gin.Context) {
	s.calls++
	var body map[string]string
	_ = c.ShouldBindJSON(&body)
	s.prompt = body["prompt"]
	s.mode = body["mode"]
	c.Status(200)
}
func (s *hostingMaintenanceStub) GetTempUnschedStatus(context.Context, int64) (*service.TempUnschedState, error) {
	s.calls++
	return &service.TempUnschedState{UntilUnix: time.Now().Add(time.Hour).Unix(), ErrorMessage: "access_token=secret-token"}, nil
}
func operationRequest(h *HostingHandler, userID int64, operation, method string, admin bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, "/hosting/accounts/17/operations/"+operation, strings.NewReader(`{"model_id":"gpt-5","prompt":"arbitrary billable prompt","mode":"image"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "17"}, {Key: "operation", Value: operation}}
	if userID > 0 {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: userID})
	}
	h.accountOperation(c, admin)
	return w
}
func TestHostingOperationsOwnershipAndRestrictedResponses(t *testing.T) {
	for _, op := range []string{"refresh", "set-privacy", "recover-state", "models", "test", "temp-unschedulable", "sync-models"} {
		for _, tt := range []struct {
			name            string
			user            int64
			admin, platform bool
			status          int
		}{
			{"owner", 7, false, false, 200}, {"foreign", 8, false, false, 404}, {"guest", 0, false, false, 401}, {"admin", 1, true, false, 200}, {"platform", 7, false, true, 404},
		} {
			t.Run(op+"/"+tt.name, func(t *testing.T) {
				a := hostedQuotaFixture()
				if tt.platform {
					a.OwnerUserID = nil
				}
				a.ErrorMessage = "invalid_grant access_token=secret-token password=secret-password"
				m := &hostingMaintenanceStub{}
				recovery := &hostingQuotaStub{}
				h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: a}, maintenance: m, tempState: m, quotaRecovery: recovery}
				method := "POST"
				if op == "models" || op == "temp-unschedulable" {
					method = "GET"
				}
				w := operationRequest(h, tt.user, op, method, tt.admin)
				require.Equal(t, tt.status, w.Code, w.Body.String())
				if tt.status != 200 {
					require.Zero(t, m.calls+recovery.recoveryCalls)
				}
				for _, secret := range []string{"secret-token", "secret-password", "secret-proxy", "openai_prism_browser"} {
					require.NotContains(t, w.Body.String(), secret)
				}
			})
		}
	}
}
func TestHostingDiagnosticDoesNotAllowArbitraryPromptsOrGetMutations(t *testing.T) {
	m := &hostingMaintenanceStub{}
	h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: hostedQuotaFixture()}, maintenance: m}
	require.Equal(t, 405, operationRequest(h, 7, "refresh", "GET", false).Code)
	require.Zero(t, m.calls)
	require.Equal(t, 200, operationRequest(h, 7, "test", "POST", false).Code)
	require.Equal(t, "Reply with OK only.", m.prompt)
	require.Equal(t, "default", m.mode)
	require.Equal(t, 429, operationRequest(h, 7, "test", "POST", false).Code)
	require.Equal(t, 1, m.calls)
}
func TestHostingRuntimeNestedAllowlist(t *testing.T) {
	a := hostedQuotaFixture()
	a.Extra["model_rate_limits"] = map[string]any{"gpt-5": map[string]any{"rate_limit_reset_at": "2099-01-01T00:00:00Z", "access_token": "nested-secret"}}
	raw, err := json.Marshal(hostedAccountView(a))
	require.NoError(t, err)
	require.Contains(t, string(raw), "rate_limit_reset_at")
	require.NotContains(t, string(raw), "nested-secret")
	require.NotContains(t, string(raw), "access_token")
}
