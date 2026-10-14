package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type probeBatchTestRepo struct {
	TicketBorrowRepository
	targets []BorrowTarget
	saved   map[int64]BorrowProbePolicy
	errors  map[int64]error
}

func (r *probeBatchTestRepo) Targets(context.Context) ([]BorrowTarget, error) { return r.targets, nil }
func (r *probeBatchTestRepo) SaveProbePolicy(_ context.Context, id int64, p BorrowProbePolicy) error {
	r.saved[id] = p
	return r.errors[id]
}
func probeBatchFixture() (*TicketBorrowService, *probeBatchTestRepo) {
	r := &probeBatchTestRepo{saved: map[int64]BorrowProbePolicy{}, errors: map[int64]error{}, targets: []BorrowTarget{
		{AccountID: 11, ProbePolicy: BorrowProbePolicy{Revision: 2, Concurrency: 3, RenewBeforeSeconds: 30}},
		{AccountID: 12, ProbePolicy: BorrowProbePolicy{Revision: 3, Concurrency: 5, RenewBeforeSeconds: 15}},
	}}
	return NewTicketBorrowService(r, nil, nil), r
}
func TestTicketBorrowProbeBatchPreservesDerivedRenewal(t *testing.T) {
	require.Equal(t, BorrowProbePolicy{Concurrency: 2, RenewBeforeSeconds: 15}, DefaultBorrowProbePolicy())
	s, r := probeBatchFixture()
	results, err := s.SaveProbePolicies(t.Context(), BorrowProbeBatchInput{Accounts: []BorrowProbeBatchAccount{{11, new(int64(2))}, {12, new(int64(3))}}, Concurrency: new(2)})
	require.NoError(t, err)
	require.Equal(t, []BorrowProbeBatchResult{{AccountID: 11, Status: "saved"}, {AccountID: 12, Status: "saved"}}, results)
	require.Equal(t, 2, r.saved[11].Concurrency)
	require.Equal(t, 30, r.saved[11].RenewBeforeSeconds)
	require.Equal(t, 15, r.saved[12].RenewBeforeSeconds)
}
func TestTicketBorrowProbeBatchValidatesBeforeWriting(t *testing.T) {
	valid := []BorrowProbeBatchAccount{{11, new(int64(2))}}
	for _, in := range []BorrowProbeBatchInput{
		{Accounts: valid}, {Concurrency: new(2)},
		{Accounts: valid, Concurrency: new(0)}, {Accounts: valid, Concurrency: new(33)},
		{Accounts: []BorrowProbeBatchAccount{{11, nil}}, Concurrency: new(2)},
		{Accounts: []BorrowProbeBatchAccount{{11, new(int64(0))}}, Concurrency: new(2)},
		{Accounts: []BorrowProbeBatchAccount{{0, new(int64(1))}}, Concurrency: new(2)},
		{Accounts: append(valid, valid...), Concurrency: new(2)},
		{Accounts: append(valid, BorrowProbeBatchAccount{12, nil}), Concurrency: new(2)},
		{Accounts: make([]BorrowProbeBatchAccount, 1001), Concurrency: new(2)},
	} {
		s, r := probeBatchFixture()
		_, err := s.SaveProbePolicies(t.Context(), in)
		require.ErrorIs(t, err, ErrBorrowProbePolicyInvalid)
		require.Empty(t, r.saved)
	}
}
func TestTicketBorrowProbeBatchReportsPartialFailuresAndFencesRevisions(t *testing.T) {
	s, r := probeBatchFixture()
	r.errors[12] = ErrBorrowConflict
	r.targets = append(r.targets, BorrowTarget{AccountID: 13, ProbePolicy: BorrowProbePolicy{Revision: 1}}, BorrowTarget{AccountID: 14, ProbePolicy: BorrowProbePolicy{Revision: 1}})
	r.errors[13] = errors.New("SECRET_DATABASE")
	results, err := s.SaveProbePolicies(t.Context(), BorrowProbeBatchInput{
		Accounts:    []BorrowProbeBatchAccount{{11, new(int64(1))}, {12, new(int64(3))}, {13, new(int64(1))}, {14, new(int64(1))}, {99, new(int64(1))}},
		Concurrency: new(2),
	})
	require.NoError(t, err)
	require.Equal(t, []BorrowProbeBatchResult{
		{AccountID: 11, Status: "failed", Reason: "borrow_configuration_conflict"},
		{AccountID: 12, Status: "failed", Reason: "borrow_configuration_conflict"},
		{AccountID: 13, Status: "failed", Reason: "borrow_operation_failed"},
		{AccountID: 14, Status: "saved"},
		{AccountID: 99, Status: "failed", Reason: "borrow_configuration_conflict"},
	}, results)
	require.NotContains(t, r.saved, int64(11))
	require.NotContains(t, r.saved, int64(99))
}
