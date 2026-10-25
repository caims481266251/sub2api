package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type hostedStatsRepo struct {
	UsageLogRepository
	t   *testing.T
	err error
}

func (r *hostedStatsRepo) GetAccountUsageStats(context.Context, int64, time.Time, time.Time) (*usagestats.AccountUsageStatsResponse, error) {
	return &usagestats.AccountUsageStatsResponse{Models: []usagestats.ModelStat{{ActualCost: 999}}}, nil
}
func (r *hostedStatsRepo) GetModelStatsWithFilters(_ context.Context, _, _ time.Time, userID, _, accountID, _ int64, _ *int16, _ *bool, _ *int8) ([]usagestats.ModelStat, error) {
	require.EqualValues(r.t, 7, userID)
	require.EqualValues(r.t, 17, accountID)
	return []usagestats.ModelStat{{Cost: 100, ActualCost: 3.00012345}}, r.err
}
func (r *hostedStatsRepo) GetEndpointStatsWithFilters(_ context.Context, _, _ time.Time, userID, _, accountID, _ int64, _ string, _ *int16, _ *bool, _ *int8) ([]usagestats.EndpointStat, error) {
	require.EqualValues(r.t, 7, userID)
	require.EqualValues(r.t, 17, accountID)
	return []usagestats.EndpointStat{{Cost: 100, ActualCost: 3.00012345}}, nil
}
func (r *hostedStatsRepo) GetUpstreamEndpointStatsWithFilters(ctx context.Context, start, end time.Time, userID, keyID, accountID, groupID int64, model string, rt *int16, stream *bool, bt *int8) ([]usagestats.EndpointStat, error) {
	return r.GetEndpointStatsWithFilters(ctx, start, end, userID, keyID, accountID, groupID, model, rt, stream, bt)
}
func TestHostingStatsDistributionsUseHistoricalWalletCosts(t *testing.T) {
	r := &hostedStatsRepo{t: t}
	s := &AccountUsageService{usageLogRepo: r}
	stats, err := s.GetHostedAccountUsageStats(context.Background(), 17, 7, time.Now().AddDate(0, 0, -30), time.Now())
	require.NoError(t, err)
	require.Equal(t, 3.00012345, stats.Models[0].ActualCost)
	require.Equal(t, 3.00012345, stats.Endpoints[0].ActualCost)
	require.Equal(t, 3.00012345, stats.UpstreamEndpoints[0].ActualCost)
	r.err = errors.New("database unavailable")
	_, err = s.GetHostedAccountUsageStats(context.Background(), 17, 7, time.Now(), time.Now())
	require.ErrorIs(t, err, r.err, "do not silently show procurement costs after a wallet query fails")
}
