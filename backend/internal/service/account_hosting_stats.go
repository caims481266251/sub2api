package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
)

// GetHostedAccountUsageStats reuses account history while selecting the wallet
// cost perspective for distributions. An account-only filter intentionally
// selects procurement costs in the shared admin queries; the owner filter
// instead returns historical actual_cost and also scopes each distribution.
func (s *AccountUsageService) GetHostedAccountUsageStats(ctx context.Context, accountID, ownerID int64, start, end time.Time) (*usagestats.AccountUsageStatsResponse, error) {
	stats, err := s.GetAccountUsageStats(ctx, accountID, start, end)
	if err != nil {
		return nil, err
	}
	stats.Models, err = s.usageLogRepo.GetModelStatsWithFilters(ctx, start, end, ownerID, 0, accountID, 0, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	stats.Endpoints, err = s.usageLogRepo.GetEndpointStatsWithFilters(ctx, start, end, ownerID, 0, accountID, 0, "", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	stats.UpstreamEndpoints, err = s.usageLogRepo.GetUpstreamEndpointStatsWithFilters(ctx, start, end, ownerID, 0, accountID, 0, "", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return stats, nil
}
