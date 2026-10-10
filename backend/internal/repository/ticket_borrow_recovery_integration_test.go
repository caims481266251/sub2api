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

func borrowRecoveryFixture(t *testing.T) (service.TicketBorrowRepository, int64, service.BorrowRequest) {
	t.Helper()
	r, id := borrowIntegrationFixture(t)
	cfg, e := r.Settings(t.Context())
	require.NoError(t, e)
	original := cfg
	t.Cleanup(func() {
		latest, _ := r.Settings(context.Background())
		original.Revision = latest.Revision
		_ = r.SaveSettings(context.Background(), original)
	})
	cfg.Running = true
	cfg.Recovery = service.DefaultBorrowRecoverySettings()
	cfg.Recovery.FailureThreshold = 2
	require.NoError(t, r.SaveSettings(t.Context(), cfg))
	target, e := r.Target(t.Context(), id)
	require.NoError(t, e)
	ok, e := r.AcquireJob(t.Context(), "target", id, "worker")
	require.NoError(t, e)
	require.True(t, ok)
	return r, id, service.BorrowRequest{TargetID: id, Kind: "probe", BatchID: "worker", Revision: target.Revision, Phase: "mint"}
}
func recoveryRequest(t *testing.T, r service.TicketBorrowRepository, q service.BorrowRequest) service.BorrowRequest {
	t.Helper()
	q.ID = uuid.NewString()
	require.NoError(t, r.BeginTargetRequest(t.Context(), q))
	require.NoError(t, r.MarkTargetRequestSent(t.Context(), q))
	return q
}
func TestTicketBorrowRecoveryPauseAtEachPhaseAndBusinessAllowed(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	q = recoveryRequest(t, r, q)
	status, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, 1, status.Inflight)
	require.NotNil(t, status.LastRequestStartedAt)
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	cfg.Running = false
	require.NoError(t, r.SaveSettings(ctx, cfg))
	require.Error(t, r.CheckTargetRequest(ctx, q))
	require.Error(t, r.MarkTargetRequestSent(ctx, q))
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "eof"))
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "closed"))
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowPaused)
	business := recoveryRequest(t, r, service.BorrowRequest{TargetID: id, Kind: "business"})
	require.NoError(t, r.EndTargetRequest(ctx, business, 200, "eof"))
	status, e = r.Target(ctx, id)
	require.NoError(t, e)
	require.Zero(t, status.Inflight)
	require.NotNil(t, status.LastRequestFinishedAt)
}
func TestTicketBorrowOrdinaryFailuresNeverCooldownAndHardWaitCannotBeBypassed(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	for range 12 {
		require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_ticket_changed", 0))
	}
	status, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Nil(t, status.UpstreamWaitUntil)
	require.Nil(t, status.BudgetWaitUntil)
	require.Zero(t, status.RecoveryLevel)
	require.Equal(t, 12, status.ConsecutiveFailures)
	request := recoveryRequest(t, r, q)
	require.NoError(t, r.EndTargetRequest(ctx, request, 200, "eof"))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_rate_limited", 60*time.Second))
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowRecoveryWaiting)
	require.NoError(t, r.FinishJob(ctx, "target", id, "worker", "target_rate_limited", 0))
	require.NoError(t, r.RefreshTarget(ctx, id))
	require.NoError(t, r.ScanTarget(ctx, id, false))
	ok, e := r.AcquireJob(ctx, "target", id, "bypass")
	require.NoError(t, e)
	require.False(t, ok)
}
func TestTicketBorrowBusinessDoesNotExtendUpstreamWaitOrRouteExpiry(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, e := r.Now(ctx)
	require.NoError(t, e)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, SourceID: 1, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(220 * time.Second)}
	require.NoError(t, r.Publish(ctx, route, "worker"))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_rate_limited", 60500*time.Millisecond))
	before, e := r.Target(ctx, id)
	require.NoError(t, e)
	business := recoveryRequest(t, r, service.BorrowRequest{TargetID: id, Kind: "business"})
	require.NoError(t, r.EndTargetRequest(ctx, business, 200, "eof"))
	after, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, before.UpstreamWaitUntil, after.UpstreamWaitUntil)
	got, e := r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, e)
	require.WithinDuration(t, route.ExpiresAt, got.ExpiresAt, time.Millisecond)
	require.NoError(t, r.FinishJob(ctx, "target", id, "worker", "target_probe_passed", 0))
	_, e = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_targets SET upstream_wait_until=clock_timestamp()-INTERVAL '1 second' WHERE account_id=$1`, id)
	require.NoError(t, e)
	ok, e := r.AcquireJob(ctx, "target", id, "still-ready")
	require.NoError(t, e)
	require.False(t, ok, "a valid route prevents acquisition even when the upstream wait ended")
	_, e = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, route.ID)
	require.NoError(t, e)
	ok, e = r.AcquireJob(ctx, "target", id, "expired")
	require.NoError(t, e)
	require.True(t, ok)
}
func TestTicketBorrowBudgetIndependentOfLegacySwitchAndCrashDoesNotExtendWait(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	cfg.Recovery.Enabled = false
	cfg.Recovery.ProbeRequestsPerMinute = 2
	require.NoError(t, r.SaveSettings(ctx, cfg))
	for range 2 {
		request := recoveryRequest(t, r, q)
		require.NoError(t, r.EndTargetRequest(ctx, request, 200, "eof"))
	}
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowRequestBudget)
	status, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, 2, status.ProbeRequestsLastMinute)
	require.NotNil(t, status.BudgetWaitUntil)
	require.Nil(t, status.UpstreamWaitUntil)
	business := recoveryRequest(t, r, service.BorrowRequest{TargetID: id, Kind: "business"})
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_rate_limited", 120*time.Second))
	before, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.NoError(t, r.FinishJob(ctx, "target", id, "worker", "target_rate_limited", 0))
	_, e = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_requests SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, business.ID)
	require.NoError(t, e)
	ok, e := r.AcquireJob(ctx, "target", id, "after-crash")
	require.NoError(t, e)
	require.False(t, ok)
	status, e = r.Target(ctx, id)
	require.NoError(t, e)
	require.Zero(t, status.Inflight)
	require.Equal(t, before.UpstreamWaitUntil, status.UpstreamWaitUntil)
}

func TestTicketBorrowRecoveryFiveBusinessRequestsDoNotSerializeOrLoseActivity(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, err := r.Now(ctx)
	require.NoError(t, err)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, SourceID: 1, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(3 * time.Minute)}
	require.NoError(t, r.Publish(ctx, route, "worker"))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_ticket_changed", 0))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_ticket_changed", 0))
	var wg sync.WaitGroup
	errors := make(chan error, 10)
	requests := make([]service.BorrowRequest, 5)
	// Valid-route business remains concurrent while acquisition is stopped.
	for i := range requests {
		requests[i] = service.BorrowRequest{ID: uuid.NewString(), TargetID: id, Kind: "business"}
		wg.Add(1)
		go func(i int) { defer wg.Done(); errors <- r.BeginTargetRequest(ctx, requests[i]) }(i)
	}
	wg.Wait()
	for range requests {
		require.NoError(t, <-errors)
	}
	status, err := r.Target(ctx, id)
	require.NoError(t, err)
	require.Zero(t, status.Inflight, "business requests do not enter probe tracking")
	_, err = r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := r.MarkTargetRequestSent(ctx, requests[i]); err != nil {
				errors <- err
				return
			}
			if err := r.EndTargetRequest(ctx, requests[i], 200, "eof"); err != nil {
				errors <- err
				return
			}
			errors <- r.EndTargetRequest(ctx, requests[i], 200, "closed")
		}(i)
	}
	wg.Wait()
	for range requests {
		require.NoError(t, <-errors)
	}
	status, err = r.Target(ctx, id)
	require.NoError(t, err)
	require.Zero(t, status.Inflight)
	require.Nil(t, status.UpstreamWaitUntil)
	got, err := r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err)
	require.WithinDuration(t, route.ExpiresAt, got.ExpiresAt, time.Millisecond)
}
