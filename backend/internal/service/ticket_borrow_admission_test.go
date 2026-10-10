package service

import (
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBorrowUnavailableSelectionPreservesExistingScheduling(t *testing.T) {
	for _, mode := range []string{"legacy", "load_batch", "advanced"} {
		for _, available := range []bool{false, true} {
			t.Run(mode+"/available="+map[bool]string{false: "false", true: "true"}[available], func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
				borrow, repo := borrowFixture()
				if !available {
					repo.route = nil
				}
				a, err := borrow.owner.accountRepo.GetByID(t.Context(), 11)
				require.NoError(t, err)
				a.Concurrency = 1
				cfg := &config.Config{}
				cfg.Gateway.Scheduling.LoadBatchEnabled = mode == "load_batch"
				svc := &OpenAIGatewayService{
					accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{*a}},
					cache:       &schedulerTestGatewayCache{}, cfg: cfg, borrowRoutes: borrow,
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
				}
				if mode == "advanced" {
					svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true")
				}
				selection, _, err := svc.SelectAccountWithScheduler(t.Context(), nil, "", "", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
				if available {
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, int64(11), selection.Account.ID)
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
				} else {
					require.Nil(t, selection)
					require.ErrorIs(t, err, ErrNoAvailableAccounts)
					require.True(t, IsBorrowRouteUnavailable(err), "%v", err)
				}
				require.Empty(t, repo.events, "selection must not start borrowing or write borrow events")
			})
		}
	}
}

func TestBorrowSelectionFailureOnlyAnnotatesNoAccountResult(t *testing.T) {
	ctx, finish := withBorrowSelectionFailure(t.Context())
	observeBorrowSelectionFailure(ctx, ErrBorrowUnavailable)
	require.NoError(t, finish(nil), "a later successful selection stays successful")
	other := errors.New("database unavailable")
	require.Same(t, other, finish(other))
	require.ErrorIs(t, finish(ErrNoAvailableCompactAccounts), ErrNoAvailableCompactAccounts)
	fresh, freshFinish := withBorrowSelectionFailure(t.Context())
	observeBorrowSelectionFailure(fresh, other)
	require.False(t, IsBorrowRouteUnavailable(freshFinish(ErrNoAvailableAccounts)), "no cross-request state")
}
