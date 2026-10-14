package handler

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
)

type hostingBillingRepoStub struct {
	service.HostingRepository
	calls          int
	owner, account int64
}

func (r *hostingBillingRepoStub) Billing(_ context.Context, owner, account int64, _ string, page, size int) (service.HostingBilling, error) {
	r.calls++
	r.owner = owner
	r.account = account
	return service.HostingBilling{Requests: 2, StandardCost: 100, HostingCost: 3, Items: []service.HostingBillingEntry{}, Page: page, PageSize: size}, nil
}
func TestHostingBillingScopeAndValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		user    int64
		account string
		query   string
		admin   bool
		status  int
		owner   int64
	}{
		{"owner", 7, "17", "", false, 200, 7}, {"foreign", 8, "17", "", false, 404, 0},
		{"anonymous", 0, "17", "", false, 401, 0}, {"admin", 1, "17", "", true, 200, 7},
		{"summary", 8, "", "?user_id=7", false, 200, 8}, {"invalid-page", 7, "17", "?page=0", false, 400, 0},
		{"invalid-size", 7, "17", "?page_size=101", false, 400, 0}, {"invalid-route", 7, "17", "?route=group", false, 400, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &hostingBillingRepoStub{}
			h := &HostingHandler{hosting: service.NewHostingService(repo, nil, nil, nil, nil, nil), accounts: &hostingQuotaRepoStub{account: hostedQuotaFixture()}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/hosting/billing"+test.query, nil)
			if test.account != "" {
				c.Params = gin.Params{{Key: "id", Value: test.account}}
			}
			if test.user > 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: test.user})
			}
			h.hostingBilling(c, test.admin)
			require.Equal(t, test.status, w.Code, w.Body.String())
			if test.status == 200 {
				require.Equal(t, test.owner, repo.owner)
				require.Equal(t, 1, repo.calls)
				require.Contains(t, w.Body.String(), `"hosting_cost":3`)
			} else {
				require.Zero(t, repo.calls)
			}
		})
	}
}
