//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func gatewayOutcome(reason string, started time.Time) service.BorrowGatewayOutcome {
	return service.BorrowGatewayOutcome{CandidateID: uuid.NewString(), CookieHash: uuid.NewString(), Gateway: "chat.gateway.unified-1.api.openai.com", StartedAt: started, Reason: reason}
}

func TestBorrowGatewayFeedbackUniqueOutcomesAndExpiry(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := t.Context()
	record := func(o service.BorrowGatewayOutcome) { require.NoError(t, r.RecordBorrowGatewayOutcome(ctx, a, o)) }
	priorities := func() map[string]time.Time {
		m, e := r.BorrowGatewayPriorities(ctx, a)
		require.NoError(t, e)
		return m
	}
	start := time.Now()
	one := gatewayOutcome("target_ticket_changed", start)
	record(one)
	record(one)
	duplicateCookie := one
	duplicateCookie.CandidateID = uuid.NewString()
	record(duplicateCookie)
	for _, reason := range []string{"target_continue_timeout", "target_probe_inconclusive", "candidate_publish_failed", "candidate_unused_after_stop"} {
		record(gatewayOutcome(reason, start))
	}
	record(gatewayOutcome("target_route_changed", start))
	require.Empty(t, priorities(), "duplicate Cookie and inconclusive results must not inflate failures")
	record(gatewayOutcome("target_ticket_changed", start))
	first := priorities()[one.Gateway]
	require.WithinDuration(t, time.Now().Add(30*time.Second), first, 3*time.Second)
	record(gatewayOutcome("target_ticket_changed", start))
	require.Equal(t, first, priorities()[one.Gateway], "demotion is not extended by more failures")
	// Advance only the feedback boundary, preserving immutable outcome history.
	_, err := integrationDB.Exec(`UPDATE ticket_borrow_gateway_feedback SET deprioritized_until=clock_timestamp()-interval '1 second',reset_after=clock_timestamp() WHERE target_id=$1`, a.TargetID)
	require.NoError(t, err)
	require.Empty(t, priorities())
	record(gatewayOutcome("target_ticket_changed", start))
	record(gatewayOutcome("target_ticket_changed", start))
	require.Empty(t, priorities(), "old failures cannot trigger immediate re-demotion")
	record(gatewayOutcome("target_ticket_changed", start))
	require.Len(t, priorities(), 1)
}

func TestBorrowGatewaySuccessLateFailureAndConfigFences(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := t.Context()
	old := time.Now().Add(-time.Second)
	for range 3 {
		require.NoError(t, r.RecordBorrowGatewayOutcome(ctx, a, gatewayOutcome("target_ticket_changed", old)))
	}
	seed := time.Now()
	require.NoError(t, r.RecordBorrowGatewayOutcome(ctx, a, gatewayOutcome("target_probe_passed", seed)))
	for range 4 {
		require.NoError(t, r.RecordBorrowGatewayOutcome(ctx, a, gatewayOutcome("target_ticket_changed", old)))
	}
	priorities, err := r.BorrowGatewayPriorities(ctx, a)
	require.NoError(t, err)
	require.Empty(t, priorities)
	for range 3 {
		require.NoError(t, r.RecordBorrowGatewayOutcome(ctx, a, gatewayOutcome("target_ticket_changed", seed.Add(time.Millisecond))))
	}
	priorities, err = r.BorrowGatewayPriorities(ctx, a)
	require.NoError(t, err)
	require.Len(t, priorities, 1)
	// A fresh batch with unchanged config continues to see the same feedback.
	next := inventoryRestart(t, r, a)
	priorities, err = r.BorrowGatewayPriorities(ctx, next)
	require.NoError(t, err)
	require.Len(t, priorities, 1)
	require.ErrorIs(t, r.RecordBorrowGatewayOutcome(ctx, a, gatewayOutcome("target_ticket_changed", time.Now())), service.ErrBorrowConflict)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET acquisition_revision=acquisition_revision+1 WHERE account_id=$1`, a.TargetID)
	require.NoError(t, err)
	_, err = r.BorrowGatewayPriorities(ctx, next)
	require.ErrorIs(t, err, service.ErrBorrowConflict)
	next = inventoryRestart(t, r, next)
	priorities, err = r.BorrowGatewayPriorities(ctx, next)
	require.NoError(t, err)
	require.Empty(t, priorities, "new acquisition generation cannot inherit old feedback")
}

func TestBorrowGatewayConcurrentFeedbackIsAtomic(t *testing.T) {
	r, a := candidateFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	outcomes := []service.BorrowGatewayOutcome{}
	for range 3 {
		outcomes = append(outcomes, gatewayOutcome("target_ticket_changed", time.Now()))
	}
	for _, o := range outcomes {
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- r.RecordBorrowGatewayOutcome(t.Context(), a, o) }()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	m, err := r.BorrowGatewayPriorities(t.Context(), a)
	require.NoError(t, err)
	require.Len(t, m, 1)
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_gateway_outcomes WHERE target_id=$1`, a.TargetID).Scan(&count))
	require.Equal(t, 3, count)
}

func probeRequest(a service.BorrowAcquisition) service.BorrowRequest {
	return service.BorrowRequest{ID: uuid.NewString(), TargetID: a.TargetID, Kind: "probe", BatchID: a.Lease, Revision: a.Revision, Identity: a.Identity, Phase: "mint"}
}
func setSharedCapacity(t *testing.T, r *ticketBorrowRepository, a service.BorrowAcquisition, c int) {
	t.Helper()
	_, err := integrationDB.Exec(`UPDATE ticket_borrow_targets SET probe_policy_override=true,probe_concurrency=$2 WHERE account_id=$1`, a.TargetID, c)
	require.NoError(t, err)
}
func TestBorrowSharedBackgroundCapacityAtomicAcrossStages(t *testing.T) {
	r, a := candidateFixture(t)
	setSharedCapacity(t, r, a, 2)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	intents := []string{uuid.NewString(), uuid.NewString()}
	for _, id := range intents {
		require.NoError(t, r.QueueSourceAttempt(ctx, 0, id))
	}
	type grant struct {
		request *service.BorrowRequest
		intent  string
		ok      bool
		err     error
	}
	results := make(chan grant, 4)
	var wg sync.WaitGroup
	for _, id := range intents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, e := r.GrantSourceAttempt(ctx, 0, id)
			results <- grant{intent: id, ok: ok, err: e}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := probeRequest(a)
			e := r.BeginTargetRequest(ctx, q)
			results <- grant{request: &q, ok: e == nil, err: e}
		}()
	}
	wg.Wait()
	close(results)
	allowed := []grant{}
	for g := range results {
		if g.ok {
			allowed = append(allowed, g)
		} else if g.err != nil {
			require.ErrorIs(t, g.err, service.ErrBorrowProbeBusy)
		}
	}
	require.Len(t, allowed, 2)
	status, err := r.Target(ctx, a.TargetID)
	require.NoError(t, err)
	require.Equal(t, 2, status.Inflight)
	stats, err := r.BorrowStatistics(ctx, a.TargetID, 2)
	require.NoError(t, err)
	require.Equal(t, 2, stats.SharedTargetInflight)
	require.Equal(t, 1, stats.SharedTargetCount)
	for _, g := range allowed {
		if g.request != nil {
			require.NoError(t, r.EndTargetRequest(ctx, *g.request, 200, "eof"))
		} else {
			require.NoError(t, r.SourceAttemptSent(ctx, g.intent))
			require.NoError(t, r.SourceAttemptCompleted(ctx, g.intent))
		}
	}
	status, err = r.Target(ctx, a.TargetID)
	require.NoError(t, err)
	require.Zero(t, status.Inflight)
}

func TestBorrowSelfFullTargetDoesNotBlockOtherTargetAndBatchFence(t *testing.T) {
	r, a := candidateFixture(t)
	setSharedCapacity(t, r, a, 1)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	first := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, 0, first))
	ok, err := r.GrantSourceAttempt(ctx, 0, first)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.SourceAttemptSent(ctx, first))
	q := probeRequest(a)
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowProbeBusy)
	blocked := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, 0, blocked))
	_, second := borrowIntegrationFixture(t)
	target, err := r.Target(ctx, second)
	require.NoError(t, err)
	b := service.BorrowAcquisition{TargetID: second, Revision: target.Revision, Lease: uuid.NewString(), Identity: "second"}
	ctxB := service.WithBorrowAcquisition(t.Context(), b)
	ok, err = r.AcquireJob(ctxB, "target", second, b.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	other := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctxB, 0, other))
	ok, err = r.GrantSourceAttempt(ctxB, 0, other)
	require.NoError(t, err)
	require.True(t, ok, "an older intent on a full target must not monopolize arbiter")
	require.NoError(t, r.SourceAttemptCompleted(ctxB, other))
	// An expired batch lease must not erase real HTTP occupancy.
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "test", 0))
	require.NoError(t, r.RefreshTarget(ctx, a.TargetID))
	ok, err = r.AcquireJob(ctx, "target", a.TargetID, "next")
	require.NoError(t, err)
	require.False(t, ok)
	require.NoError(t, r.SourceAttemptCompleted(ctx, first))
	ok, err = r.AcquireJob(ctx, "target", a.TargetID, "next")
	require.NoError(t, err)
	require.True(t, ok)
	// Simulate a crashed acquisition. Its bounded lease is the only automatic release.
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_source_attempts SET completed_at=NULL,lease_until=clock_timestamp()-interval '1 second' WHERE intent_id=$1`, first)
	require.NoError(t, err)
	q.BatchID = "next"
	require.NoError(t, r.BeginTargetRequest(context.Background(), q))
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "eof"))
}
