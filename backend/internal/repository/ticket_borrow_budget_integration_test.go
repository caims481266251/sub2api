//go:build integration

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowSourceBudgetSharedAcrossSourcesAndIndependentOfTargets(t *testing.T) {
	r, _, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	cfg.Recovery.SourceRequestsPerMinute = 6
	cfg.Recovery.ProbeRequestsPerMinute = 2
	require.NoError(t, r.SaveSettings(ctx, cfg))
	ids := []int64{}
	for range 3 {
		s := &service.BorrowSource{Name: "budget", Model: "gpt-6-luna", Enabled: true, SubjectHash: uuid.NewString(), Secret: service.BorrowSourceSecret{Credentials: map[string]any{"chatgpt_account_id": uuid.NewString()}}}
		require.NoError(t, r.SaveSource(ctx, s))
		ids = append(ids, s.ID)
		ok, err := r.AcquireJob(ctx, "source", s.ID, "source-worker")
		require.NoError(t, err)
		require.True(t, ok)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_source_attempts WHERE source_id=$1`, id)
			_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_sources WHERE id=$1`, id)
		}
	})
	var wg sync.WaitGroup
	results := make(chan error, 30)
	for i := range 30 {
		wg.Add(1)
		go func(id int64) { defer wg.Done(); results <- r.ReserveSourceAttempt(ctx, id, "source-worker") }(ids[i%3])
	}
	wg.Wait()
	close(results)
	allowed, denied := 0, 0
	for err := range results {
		if err == nil {
			allowed++
		} else {
			require.ErrorIs(t, err, service.ErrBorrowRequestBudget)
			var wait *service.BorrowBudgetWaitError
			require.True(t, errors.As(err, &wait))
			require.Equal(t, "source", wait.Scope)
			require.Positive(t, wait.RetryAfter)
			denied++
		}
	}
	require.Equal(t, 6, allowed)
	require.Equal(t, 24, denied)
	status, err := r.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 6, status.SourceBudget.Used)
	require.NotNil(t, status.SourceBudget.AvailableAt)
	for _, phase := range []string{"mint", "continue"} {
		q.Phase = phase
		request := recoveryRequest(t, r, q)
		require.NoError(t, r.EndTargetRequest(ctx, request, 200, "eof"))
	}
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowRequestBudget)
	acquiring := service.WithBorrowAcquisition(ctx, service.BorrowAcquisition{TargetID: q.TargetID, Revision: q.Revision, Lease: q.BatchID})
	err = r.ReserveSourceAttempt(acquiring, ids[0], "source-worker")
	var targetWait *service.BorrowBudgetWaitError
	require.ErrorAs(t, err, &targetWait)
	require.Equal(t, "target", targetWait.Scope, "a blocked target must not be reported as global source exhaustion")
	business := recoveryRequest(t, r, service.BorrowRequest{TargetID: q.TargetID, Kind: "business"})
	require.NoError(t, r.EndTargetRequest(ctx, business, 200, "eof"))
	// Increasing the source limit releases only source capacity. A new repository
	// sees persisted usage; restarts cannot replenish either counter.
	r = NewTicketBorrowRepository(integrationDB, r.(*ticketBorrowRepository).encryptor)
	status.Recovery.SourceRequestsPerMinute = 7
	require.NoError(t, r.SaveSettings(ctx, status))
	require.NoError(t, r.ReserveSourceAttempt(ctx, ids[0], "source-worker"))
	require.ErrorIs(t, r.ReserveSourceAttempt(ctx, ids[1], "source-worker"), service.ErrBorrowRequestBudget)
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowRequestBudget)
}

func TestTicketBorrowBudgetLoweringUsesCorrectReleaseAndPreservesUpstreamWait(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	cfg.Recovery.ProbeRequestsPerMinute = 6
	require.NoError(t, r.SaveSettings(ctx, cfg))
	for range 3 {
		request := recoveryRequest(t, r, q)
		require.NoError(t, r.EndTargetRequest(ctx, request, 200, "eof"))
	}
	// Three distinct reservations: 55, 50, and 5 seconds ago. A limit of two
	// cannot release a slot until the SECOND oldest expires, ten seconds later.
	_, err = integrationDB.ExecContext(ctx, `WITH ranked AS (SELECT id,row_number() OVER(ORDER BY reserved_at,id) n FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe') UPDATE ticket_borrow_requests x SET reserved_at=statement_timestamp()-(CASE r.n WHEN 1 THEN 55 WHEN 2 THEN 50 ELSE 5 END)*INTERVAL '1 second' FROM ranked r WHERE x.id=r.id`, id)
	require.NoError(t, err)
	cfg, err = r.Settings(ctx)
	require.NoError(t, err)
	cfg.Recovery.ProbeRequestsPerMinute = 2
	require.NoError(t, r.SaveSettings(ctx, cfg))
	q.ID = uuid.NewString()
	err = r.BeginTargetRequest(ctx, q)
	var wait *service.BorrowBudgetWaitError
	require.ErrorAs(t, err, &wait)
	require.Equal(t, "target", wait.Scope)
	require.InDelta(t, 10, wait.RetryAfter.Seconds(), 2)
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_rate_limited", 90*time.Second))
	before, err := r.Target(ctx, id)
	require.NoError(t, err)
	cfg, err = r.Settings(ctx)
	require.NoError(t, err)
	cfg.Recovery.ProbeRequestsPerMinute = 10
	require.NoError(t, r.SaveSettings(ctx, cfg))
	after, err := r.Target(ctx, id)
	require.NoError(t, err)
	require.Equal(t, before.UpstreamWaitUntil, after.UpstreamWaitUntil)
	require.Nil(t, after.BudgetWaitUntil)
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowRecoveryWaiting)
	// A stale settings save is rejected atomically and cannot clear that limit.
	require.ErrorIs(t, r.SaveSettings(ctx, cfg), service.ErrBorrowConflict)
	after, err = r.Target(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, before.UpstreamWaitUntil, after.UpstreamWaitUntil)
}
