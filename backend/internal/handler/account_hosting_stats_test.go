package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type hostingStatsStub struct {
	calls      int
	start, end time.Time
}

func (s *hostingStatsStub) GetHostedAccountUsageStats(_ context.Context, id, ownerID int64, start, end time.Time) (*usagestats.AccountUsageStatsResponse, error) {
	s.calls++
	s.start = start
	s.end = end
	return &usagestats.AccountUsageStatsResponse{}, nil
}

func TestHostingStatsAccessAndRange(t *testing.T) {
	for _, tt := range []struct {
		name            string
		user            int64
		admin, platform bool
		days            string
		status          int
	}{
		{"owner", 7, false, false, "", 200}, {"foreign", 8, false, false, "", 404},
		{"unauthenticated", 0, false, false, "", 401}, {"admin", 1, true, false, "", 200},
		{"platform", 7, false, true, "", 404}, {"admin_platform", 1, true, true, "", 404},
		{"invalid_range", 7, false, false, "91", 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := hostedQuotaFixture()
			if tt.platform {
				a.OwnerUserID = nil
			}
			s := &hostingStatsStub{}
			h := &HostingHandler{accounts: &hostingQuotaRepoStub{account: a}, stats: s}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/hosting/accounts/17/stats?days="+tt.days, nil)
			c.Params = gin.Params{{Key: "id", Value: "17"}}
			if tt.user != 0 {
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tt.user})
			}
			h.accountStats(c, tt.admin)
			require.Equal(t, tt.status, w.Code, w.Body.String())
			if tt.status == 200 {
				require.Equal(t, 1, s.calls)
				require.Equal(t, s.start.AddDate(0, 0, 30), s.end)
				require.Equal(t, 0, s.start.Hour())
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
			} else {
				require.Zero(t, s.calls)
			}
			require.NotContains(t, w.Body.String(), "secret-token")
		})
	}
}

func TestHostingStatsUsesStandardBaseAndHistoricalDebit(t *testing.T) {
	s := &usagestats.AccountUsageStatsResponse{
		History: []usagestats.AccountUsageHistory{
			{Date: "2026-10-10", Cost: 40, ActualCost: 400, UserCost: 1.2, Requests: 2},
			{Date: "2026-10-11", Cost: 60, ActualCost: 6, UserCost: 1.80012345, Requests: 1},
		},
		Summary: usagestats.AccountUsageSummary{TotalStandardCost: 100, TotalCost: 406, TotalUserCost: 3.00012345, AvgDailyUserCost: 1.500061725},
	}
	normalizeHostingStats(s, 30)
	require.Equal(t, 30, s.Summary.Days)
	require.Equal(t, 2, s.Summary.ActualDaysUsed)
	require.Equal(t, float64(100), s.Summary.TotalCost)
	require.Equal(t, float64(50), s.Summary.AvgDailyCost)
	require.Equal(t, 3.00012345, s.Summary.TotalUserCost)
	require.Equal(t, 1.500061725, s.Summary.AvgDailyUserCost)
	require.Equal(t, "2026-10-11", s.Summary.HighestCostDay.Date)
	require.Equal(t, 1.80012345, s.Summary.HighestCostDay.UserCost)
	require.Equal(t, float64(60), s.History[1].ActualCost)
	empty := &usagestats.AccountUsageStatsResponse{}
	normalizeHostingStats(empty, 30)
	require.Zero(t, empty.Summary.ActualDaysUsed)
	require.Zero(t, empty.Summary.AvgDailyCost)
	require.Nil(t, empty.Summary.HighestCostDay)
}
