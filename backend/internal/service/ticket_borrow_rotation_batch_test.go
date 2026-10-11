package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type rotationBatchTestRepo struct {
	*rotationTestRepo
	states []BorrowRotationState
	saved  map[int64]BorrowRotationPolicy
	errors map[int64]error
}

func (r *rotationBatchTestRepo) RotationStates(context.Context) ([]BorrowRotationState, error) {
	return r.states, nil
}
func (r *rotationBatchTestRepo) SaveRotationPolicy(_ context.Context, id int64, p BorrowRotationPolicy) error {
	r.saved[id] = p
	return r.errors[id]
}
func rotationBatchFixture() (*TicketBorrowService, *rotationBatchTestRepo) {
	s, base := rotationServiceFixture()
	r := &rotationBatchTestRepo{rotationTestRepo: base, saved: map[int64]BorrowRotationPolicy{}, errors: map[int64]error{}, states: []BorrowRotationState{
		{AccountID: 11, BorrowRotationPolicy: BorrowRotationPolicy{Revision: 2, Automatic: true, IntervalSeconds: 30}},
		{AccountID: 12, BorrowRotationPolicy: BorrowRotationPolicy{Revision: 3, Automatic: false, IntervalSeconds: 120}},
	}}
	s.repo = r
	return s, r
}
func TestTicketBorrowRotationBatchCannotRestoreLegacyEnvironments(t *testing.T) {
	s, r := rotationBatchFixture()
	for _, in := range []BorrowRotationBatchInput{
		{Accounts: []BorrowRotationBatchAccount{{11, new(int64(2))}}, Automatic: new(true)},
		{Accounts: []BorrowRotationBatchAccount{{12, new(int64(3))}}, IntervalSeconds: new(1)},
		{},
	} {
		result, err := s.SaveRotationPolicies(t.Context(), in)
		require.ErrorIs(t, err, ErrBorrowRotationRetired)
		require.Empty(t, result)
		require.Empty(t, r.saved)
	}
}
