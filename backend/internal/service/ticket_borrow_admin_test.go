package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type borrowControlsRepo struct {
	TicketBorrowRepository
	settings BorrowSettings
	writes   int
}

func (r *borrowControlsRepo) Settings(context.Context) (BorrowSettings, error) {
	return r.settings, nil
}
func (r *borrowControlsRepo) SaveSettings(_ context.Context, v BorrowSettings) error {
	if v.Revision != r.settings.Revision {
		return ErrBorrowConflict
	}
	r.writes++
	v.Revision++
	r.settings = v
	return nil
}

func TestBorrowControlsPreserveParametersAndFenceConcurrentEdits(t *testing.T) {
	r := &borrowControlsRepo{settings: BorrowSettings{Revision: 7, Running: true, TTLSeconds: 230, MinRouteRemainingSeconds: new(120), ContinueTimeoutSeconds: new(7), Replenishment: &BorrowReplenishmentSettings{Inventory: 3, IntervalSeconds: 20, HoldSeconds: 90}}}
	s := NewTicketBorrowService(r, nil, nil)
	require.NoError(t, s.SaveControls(t.Context(), BorrowControls{Revision: 7, Running: new(false)}))
	require.False(t, r.settings.Running)
	require.Equal(t, 230, r.settings.TTLSeconds)
	require.Equal(t, 120, *r.settings.MinRouteRemainingSeconds)
	require.Equal(t, 3, r.settings.Replenishment.Inventory)
	require.ErrorIs(t, s.SaveControls(t.Context(), BorrowControls{Revision: 7, ReplenishmentEnabled: new(true)}), ErrBorrowConflict)
	require.False(t, r.settings.Replenishment.Enabled)
	require.Equal(t, 1, r.writes)
	require.NoError(t, s.SaveControls(t.Context(), BorrowControls{Revision: 8, ReplenishmentEnabled: new(true)}))
	require.False(t, r.settings.Running, "replenishment must not implicitly resume acquisition")
	require.True(t, r.settings.Replenishment.Enabled)
	require.Equal(t, 20, r.settings.Replenishment.IntervalSeconds)
	require.Error(t, s.SaveControls(t.Context(), BorrowControls{Revision: 9}))
	require.Equal(t, 2, r.writes)
}

func TestBorrowBatchRestoresInheritanceWithoutWritingEffectiveValues(t *testing.T) {
	s, r := probeBatchFixture()
	results, err := s.SaveProbePolicies(t.Context(), BorrowProbeBatchInput{UseGlobal: true, Accounts: []BorrowProbeBatchAccount{{11, new(int64(2))}, {12, new(int64(2))}}})
	require.NoError(t, err)
	require.Equal(t, "saved", results[0].Status)
	require.Equal(t, "failed", results[1].Status)
	require.True(t, r.saved[11].UseGlobal)
	require.NotContains(t, r.saved, int64(12))
	_, err = s.SaveProbePolicies(t.Context(), BorrowProbeBatchInput{UseGlobal: true, Concurrency: new(3), Accounts: []BorrowProbeBatchAccount{{11, new(int64(2))}}})
	require.ErrorIs(t, err, ErrBorrowProbePolicyInvalid)
}
