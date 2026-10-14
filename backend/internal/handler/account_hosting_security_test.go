package handler

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHostingSecurityStrictSmallRequests(t *testing.T) {
	for _, endpoint := range []string{"oauth/start", "pools", "test"} {
		for _, body := range []string{`{"owner_user_id":99}`, `{} {}`, `{"name":"` + strings.Repeat("x", 16384) + `"}`} {
			t.Run(endpoint+"/"+body[:min(len(body), 24)], func(t *testing.T) {
				m := &hostingMaintenanceStub{}
				h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: hostedQuotaFixture()}, maintenance: m}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/hosting/"+endpoint, strings.NewReader(body))
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
				c.Params = gin.Params{{Key: "id", Value: "17"}, {Key: "operation", Value: "test"}}
				switch endpoint {
				case "oauth/start":
					h.OAuthStart(c)
				case "pools":
					h.SavePool(c)
				case "test":
					h.AccountOperation(c)
				}
				require.Equal(t, 400, w.Code, w.Body.String())
				require.Zero(t, m.calls)
			})
		}
	}
}

type hostingDisabledSettings struct{ service.SettingRepository }

func (*hostingDisabledSettings) GetValue(context.Context, string) (string, error) {
	return `{"enabled":false}`, nil
}

func TestHostingSecurityDisabledFeatureBlocksUpstream(t *testing.T) {
	h := &HostingHandler{hosting: service.NewHostingService(&hostedSettingsRepo{}, &hostingDisabledSettings{}, nil, nil, nil, nil)}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7}) })
	called := false
	r.POST("/test", h.RequireEnabled, func(c *gin.Context) { called = true; c.Status(200) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/test", nil))
	require.Equal(t, 403, w.Code)
	require.False(t, called)
}

type hostingSensitiveUsage struct{ cached *service.UsageInfo }

func (s *hostingSensitiveUsage) GetUsageForAccount(context.Context, *service.Account, ...bool) (*service.UsageInfo, error) {
	return s.cached, nil
}

func TestHostingSecurityUsageRedactsCredentialsWithoutMutatingCache(t *testing.T) {
	usage := &hostingSensitiveUsage{cached: &service.UsageInfo{Error: "request failed secret-token", ForbiddenReason: "access_token=secret-token"}}
	h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: hostedQuotaFixture()}, usage: usage}
	w := quotaRequest(h, 7, "17", "usage", false)
	require.Equal(t, 200, w.Code)
	require.NotContains(t, w.Body.String(), "secret-token")
	require.Contains(t, usage.cached.Error, "secret-token")
}
