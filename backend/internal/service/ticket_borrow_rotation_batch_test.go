package service

import (
	"context"
	"errors"
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
func TestTicketBorrowRotationBatchPreservesUnspecifiedFields(t *testing.T) {
	for _, automaticOnly := range []bool{true, false} {
		s, r := rotationBatchFixture()
		in := BorrowRotationBatchInput{Accounts: []BorrowRotationBatchAccount{{11, new(int64(2))}, {12, new(int64(3))}, {13, new(int64(0))}}}
		if automaticOnly {
			in.Automatic = new(false)
		} else {
			in.IntervalSeconds = new(300)
		}
		results, err := s.SaveRotationPolicies(t.Context(), in)
		require.NoError(t, err)
		require.Len(t, results, 3)
		for _, result := range results {
			require.Equal(t, "saved", result.Status)
		}
		if automaticOnly {
			require.Equal(t, BorrowRotationPolicy{Revision: 2, Automatic: false, IntervalSeconds: 30}, r.saved[11])
			require.Equal(t, BorrowRotationPolicy{Revision: 3, Automatic: false, IntervalSeconds: 120}, r.saved[12])
			require.Equal(t, BorrowRotationPolicy{IntervalSeconds: 60}, r.saved[13])
		} else {
			require.Equal(t, BorrowRotationPolicy{Revision: 2, Automatic: true, IntervalSeconds: 300}, r.saved[11])
			require.Equal(t, BorrowRotationPolicy{Revision: 3, Automatic: false, IntervalSeconds: 300}, r.saved[12])
			require.Equal(t, BorrowRotationPolicy{IntervalSeconds: 300}, r.saved[13])
		}
	}
}
func TestTicketBorrowRotationBatchValidatesBeforeWriting(t *testing.T) {
	valid := []BorrowRotationBatchAccount{{11, new(int64(2))}}
	for _, in := range []BorrowRotationBatchInput{
		{Accounts: valid},
		{Automatic: new(true)},
		{Accounts: valid, IntervalSeconds: new(0)},
		{Accounts: valid, IntervalSeconds: new(86401)},
		{Accounts: []BorrowRotationBatchAccount{{11, nil}}, Automatic: new(true)},
		{Accounts: []BorrowRotationBatchAccount{{11, new(int64(-1))}}, Automatic: new(true)},
		{Accounts: []BorrowRotationBatchAccount{{0, new(int64(0))}}, Automatic: new(true)},
		{Accounts: []BorrowRotationBatchAccount{{11, new(int64(2))}, {11, new(int64(2))}}, Automatic: new(true)},
		{Accounts: append(valid, BorrowRotationBatchAccount{12, nil}), Automatic: new(true)},
		{Accounts: make([]BorrowRotationBatchAccount, 1001), Automatic: new(true)},
	} {
		s, r := rotationBatchFixture()
		_, err := s.SaveRotationPolicies(t.Context(), in)
		require.ErrorIs(t, err, ErrBorrowRotationInvalid)
		require.Empty(t, r.saved)
	}
}
func TestTicketBorrowRotationBatchReportsPartialFailuresAndFencesRevisions(t *testing.T) {
	s, r := rotationBatchFixture()
	r.errors[12] = ErrBorrowConflict // Changed again after the initial snapshot.
	r.errors[13] = ErrBorrowRotationInvalid
	r.errors[14] = errors.New("PROXY_SECRET TOKEN_SECRET")
	results, err := s.SaveRotationPolicies(t.Context(), BorrowRotationBatchInput{
		Accounts:  []BorrowRotationBatchAccount{{11, new(int64(1))}, {12, new(int64(3))}, {13, new(int64(0))}, {14, new(int64(0))}, {15, new(int64(0))}},
		Automatic: new(true), IntervalSeconds: new(300),
	})
	require.NoError(t, err)
	require.Equal(t, []BorrowRotationBatchResult{
		{AccountID: 11, Status: "failed", Reason: "borrow_configuration_conflict"},
		{AccountID: 12, Status: "failed", Reason: "borrow_configuration_conflict"},
		{AccountID: 13, Status: "failed", Reason: "borrow_rotation_invalid"},
		{AccountID: 14, Status: "failed", Reason: "borrow_operation_failed"},
		{AccountID: 15, Status: "saved"},
	}, results)
	require.NotContains(t, r.saved, int64(11))
	require.Equal(t, BorrowRotationPolicy{Automatic: true, IntervalSeconds: 300}, r.saved[15])
}
