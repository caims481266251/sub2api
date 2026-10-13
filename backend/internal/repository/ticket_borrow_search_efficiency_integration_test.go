//go:build integration

package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestBorrowPoolStandbyWhileConsumersBusy(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	for range 2 {
		p := f.reserve(t)
		f.enqueue(t, p)
		c, e := f.r.ClaimCandidatePool(t.Context(), a)
		require.NoError(t, e)
		require.NotNil(t, c)
	}
	for range 2 {
		p := f.reserve(t)
		f.enqueue(t, p)
	}
	p, e := f.r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
	require.NoError(t, e)
	require.NotNil(t, p, "free pool capacity is independent of occupied target slots")
	c, e := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, e)
	require.Nil(t, c, "standby never increases target concurrency")
	require.NoError(t, f.r.RemoveCandidatePoolDemand(t.Context(), a))
	p, e = f.r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
	require.NoError(t, e)
	require.NotNil(t, p, "enabled consumers still need inventory between target tasks")
}

func TestBorrowPoolFeedbackChangesSelectionAndAllowsFallback(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	old := f.enqueue(t, f.reserve(t))
	fresh := f.enqueue(t, f.reserve(t))
	scope, e := borrowGatewayScopeForLease(t.Context(), integrationDB, a)
	require.NoError(t, e)
	_, e = integrationDB.Exec(`UPDATE ticket_borrow_candidate_pool SET details=details||jsonb_build_object('gateway',$2::text) WHERE id=$1`, fresh.ID, "failed-gateway")
	require.NoError(t, e)
	_, e = integrationDB.Exec(`INSERT INTO ticket_borrow_gateway_feedback(target_id,scope,gateway,deprioritized_until) VALUES($1,$2,'failed-gateway',clock_timestamp()+interval '30 seconds')`, a.TargetID, scope)
	require.NoError(t, e)
	c, e := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, e)
	require.Equal(t, old.ID, c.ID, "a newer demoted gateway cannot win merely on freshness")
	require.Equal(t, "normal", c.Details.GatewayPriority, "the oldest ordinary candidate already wins FIFO")
	c, e = f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, e)
	require.Equal(t, fresh.ID, c.ID, "all-demoted fallback must remain available")
	require.Equal(t, "recheck", c.Details.GatewayPriority)
}

func TestBorrowSearchCostIncludesFailuresAndResetsAtPublication(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	c := f.enqueue(t, f.reserve(t))
	_, e := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, e)
	_, e = integrationDB.Exec(`UPDATE ticket_borrow_targets SET last_batch_started_at=clock_timestamp()-interval '100 seconds',acquisition_duration_ms=10000 WHERE account_id=$1`, a.TargetID)
	require.NoError(t, e)
	require.NoError(t, f.r.RecordProbeOutcome(t.Context(), a.TargetID, a.Lease, "target_ticket_changed", 0))
	var observed int64
	require.NoError(t, integrationDB.QueryRow(`SELECT acquisition_duration_ms FROM ticket_borrow_targets WHERE account_id=$1`, a.TargetID).Scan(&observed))
	require.GreaterOrEqual(t, observed, int64(100000))
	route := poolCandidateRoute(a, c)
	require.NoError(t, f.r.Publish(service.WithBorrowAcquisition(t.Context(), a), route, a.Lease))
	require.NoError(t, integrationDB.QueryRow(`SELECT acquisition_duration_ms FROM ticket_borrow_targets WHERE account_id=$1`, a.TargetID).Scan(&observed))
	require.GreaterOrEqual(t, observed, int64(100000), "quick successful candidate must not hide failed search time")
	// A late failure from another candidate must start timing at the publication,
	// not keep measuring the old task's entire age.
	_, e = integrationDB.Exec(`UPDATE ticket_borrow_targets SET acquisition_duration_ms=10000 WHERE account_id=$1`, a.TargetID)
	require.NoError(t, e)
	require.NoError(t, f.r.RecordProbeOutcome(t.Context(), a.TargetID, a.Lease, "target_continue_timeout", 0))
	require.NoError(t, integrationDB.QueryRow(`SELECT acquisition_duration_ms FROM ticket_borrow_targets WHERE account_id=$1`, a.TargetID).Scan(&observed))
	require.Equal(t, int64(10000), observed)
	require.ErrorIs(t, f.r.RecordProbeOutcome(t.Context(), a.TargetID, "stale-lease", "target_ticket_changed", 0), service.ErrBorrowConflict)
	require.WithinDuration(t, time.Now(), *route.AcquisitionStartedAt, 5*time.Second)
}

func TestBorrowPoolStandbyReservesTargetBudgetForClaimedWork(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	f.enqueue(t, f.reserve(t))
	c, e := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, e)
	require.NotNil(t, c)
	_, e = integrationDB.Exec(`UPDATE ticket_borrow_settings SET recovery=jsonb_set(recovery,'{probe_requests_per_minute}','2') WHERE id=1`)
	require.NoError(t, e)
	p, e := f.r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
	require.NoError(t, e)
	require.NotNil(t, p, "source inventory is independent of the target HTTP budget")
}
