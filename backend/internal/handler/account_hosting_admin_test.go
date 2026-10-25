package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hostingAdminRepo struct {
	service.HostingRepository
	calls int
}

func (r *hostingAdminRepo) ListAllHosted(_ context.Context, _ string, _, _ int) ([]service.HostingAdminAccount, int, error) {
	r.calls++
	owner := int64(7)
	return []service.HostingAdminAccount{{UserID: 7, UserEmail: "owner@example.com", PoolName: "Work", Account: service.Account{ID: 17, OwnerUserID: &owner, Name: "Hosted", Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"email": "account@example.com", "refresh_token": "refresh-canary", "access_token": "access-canary", "password": "password-canary"}, Extra: map[string]any{"openai_prism_browser": true, "openai_excel_bps_proxy_source": "private-proxy-canary"}}}}, 1, nil
}

func TestHostingAdminListSafeDTOAndPagination(t *testing.T) {
	for _, test := range []struct {
		query  string
		status int
		calls  int
	}{
		{"?page=1&page_size=20", http.StatusOK, 1}, {"?page=0", http.StatusBadRequest, 0}, {"?page_size=101", http.StatusBadRequest, 0}, {"?page=invalid", http.StatusBadRequest, 0},
	} {
		repo := &hostingAdminRepo{}
		h := &HostingHandler{hosting: service.NewHostingService(repo, nil, nil, nil, nil, nil)}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/admin/hosting/accounts"+test.query, nil)
		h.AdminAccounts(c)
		require.Equal(t, test.status, w.Code)
		require.Equal(t, test.calls, repo.calls)
		for _, secret := range []string{"refresh-canary", "access-canary", "password-canary", "private-proxy-canary", "openai_prism_browser"} {
			require.NotContains(t, w.Body.String(), secret)
		}
		if test.status == http.StatusOK {
			require.Contains(t, w.Body.String(), "owner@example.com")
			require.Contains(t, w.Body.String(), "Work")
		}
	}
}
