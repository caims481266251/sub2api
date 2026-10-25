package handler

import (
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/gin-gonic/gin"
)

type hostingAccountStatsService interface {
	GetHostedAccountUsageStats(context.Context, int64, int64, time.Time, time.Time) (*usagestats.AccountUsageStatsResponse, error)
}

func (h *HostingHandler) AccountStats(c *gin.Context)      { h.accountStats(c, false) }
func (h *HostingHandler) AdminAccountStats(c *gin.Context) { h.accountStats(c, true) }

func (h *HostingHandler) accountStats(c *gin.Context, admin bool) {
	account, ok := h.quotaAccount(c, admin)
	if !ok {
		return
	}
	days := 30
	if raw := c.Query("days"); raw != "" {
		d, err := strconv.Atoi(raw)
		if err != nil || d < 1 || d > 90 {
			response.BadRequest(c, "days must be between 1 and 90")
			return
		}
		days = d
	}
	now := timezone.Now()
	stats, err := h.stats.GetHostedAccountUsageStats(c.Request.Context(), account.ID, *account.OwnerUserID,
		timezone.StartOfDay(now.AddDate(0, 0, 1-days)), timezone.StartOfDay(now.AddDate(0, 0, 1)))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	normalizeHostingStats(stats, days)
	response.Success(c, stats)
}

// Hosted accounts use logged standard usage as the billing base, never the
// procurement cost or the account's current multiplier. UserCost is the
// historical wallet debit and must not be recalculated at today's rate.
func normalizeHostingStats(stats *usagestats.AccountUsageStatsResponse, days int) {
	s := &stats.Summary
	s.Days = days
	s.ActualDaysUsed = len(stats.History)
	s.TotalCost = s.TotalStandardCost
	s.AvgDailyCost = s.TotalCost / float64(max(s.ActualDaysUsed, 1))
	var highest *usagestats.AccountUsageHistory
	for i := range stats.History {
		d := &stats.History[i]
		d.ActualCost = d.Cost
		if s.Today != nil && s.Today.Date == d.Date {
			s.Today.Cost = d.Cost
		}
		if s.HighestRequestDay != nil && s.HighestRequestDay.Date == d.Date {
			s.HighestRequestDay.Cost = d.Cost
		}
		if highest == nil || d.Cost > highest.Cost {
			highest = d
		}
	}
	s.HighestCostDay = nil
	if highest != nil {
		s.HighestCostDay = &struct {
			Date     string  `json:"date"`
			Label    string  `json:"label"`
			Cost     float64 `json:"cost"`
			UserCost float64 `json:"user_cost"`
			Requests int64   `json:"requests"`
		}{highest.Date, highest.Label, highest.Cost, highest.UserCost, highest.Requests}
	}
}
