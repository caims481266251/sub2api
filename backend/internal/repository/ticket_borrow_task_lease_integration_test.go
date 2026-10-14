//go:build integration

package repository

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func targetLeaseTimes(t *testing.T, id int64) (time.Time, time.Time) {
	t.Helper()
	var until, now time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT lease_until,clock_timestamp() FROM ticket_borrow_targets WHERE account_id=$1`, id).Scan(&until, &now))
	return until, now
}

func TestBorrowTargetLeaseShortRenewableOwnershipAndSourceWait(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	until, now := targetLeaseTimes(t, a.TargetID)
	require.InDelta(t, 30, until.Sub(now).Seconds(), 2, "initial ownership must not retain the old 180-second lease")
	ok, err := r.AcquireJob(ctx, "target", a.TargetID, "competing-owner")
	require.NoError(t, err)
	require.False(t, ok)
	// A long-lived self/scan task must still be allowed to queue acquisition.
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET last_batch_started_at=clock_timestamp()-INTERVAL '5 minutes',lease_until=clock_timestamp()+INTERVAL '5 seconds' WHERE account_id=$1`, a.TargetID)
	require.NoError(t, err)
	intent := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, 0, intent))
	var before, after time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT expires_at FROM ticket_borrow_source_intents WHERE id=$1`, intent).Scan(&before))
	require.NoError(t, r.RenewTargetLease(ctx, a))
	until, now = targetLeaseTimes(t, a.TargetID)
	require.InDelta(t, 30, until.Sub(now).Seconds(), 1)
	require.NoError(t, integrationDB.QueryRow(`SELECT expires_at FROM ticket_borrow_source_intents WHERE id=$1`, intent).Scan(&after))
	require.True(t, after.After(before))
	require.Equal(t, until, after, "pending source admission shares the current task lease")
	ok, err = r.GrantSourceAttempt(ctx, 0, intent)
	require.NoError(t, err)
	require.True(t, ok, "no hidden 150-second source queue cutoff may remain")
	require.NoError(t, r.SourceAttemptCompleted(ctx, intent))

	// Keeping ownership while draining must not clear or bypass admission waits.
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET upstream_wait_until=clock_timestamp()+INTERVAL '1 minute',budget_wait_until=clock_timestamp()+INTERVAL '1 minute' WHERE account_id=$1`, a.TargetID)
	require.NoError(t, err)
	require.NoError(t, r.RenewTargetLease(ctx, a))
	require.ErrorIs(t, r.CheckAcquisition(ctx, a), service.ErrBorrowRecoveryWaiting)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET upstream_wait_until=NULL WHERE account_id=$1`, a.TargetID)
	require.NoError(t, err)
	require.NoError(t, r.RenewTargetLease(ctx, a))
	require.ErrorIs(t, r.CheckAcquisition(ctx, a), service.ErrBorrowRequestBudget)
}

func TestBorrowTargetLeaseSatisfiedInventoryDoesNotKeepInitialDemand(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	for range 2 {
		require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
		passBorrowFixtureQuality(t)
	}
	for _, initial := range []string{"renew", "healthy", "refill"} {
		_, err := integrationDB.Exec(`UPDATE ticket_borrow_targets SET last_inventory_state=$2 WHERE account_id=$1`, a.TargetID, initial)
		require.NoError(t, err)
		need, err := r.NeedsCandidate(ctx, a, nil)
		require.NoError(t, err)
		require.False(t, need, "a renewable task must stop on current inventory, not its initial state")
	}
}

func TestBorrowTargetLeaseRenewalFences(t *testing.T) {
	for _, change := range []string{"missing_token", "replaced", "expired", "revision", "policy", "identity", "settings", "credentials", "disabled", "paused", "account"} {
		t.Run(change, func(t *testing.T) {
			r, a := candidateFixture(t)
			statement := ""
			switch change {
			case "missing_token":
				a.Lease = ""
			case "replaced":
				statement = `UPDATE ticket_borrow_targets SET lease_token='another-owner' WHERE account_id=$1`
			case "expired":
				statement = `UPDATE ticket_borrow_targets SET lease_until=clock_timestamp()-INTERVAL '1 millisecond' WHERE account_id=$1`
			case "revision":
				statement = `UPDATE ticket_borrow_targets SET revision=revision+1 WHERE account_id=$1`
			case "policy":
				statement = `UPDATE ticket_borrow_targets SET probe_policy_revision=probe_policy_revision+1 WHERE account_id=$1`
			case "identity":
				statement = `UPDATE ticket_borrow_targets SET acquisition_revision=acquisition_revision+1 WHERE account_id=$1`
			case "settings":
				_, err := integrationDB.Exec(`UPDATE ticket_borrow_settings SET revision=revision+1 WHERE id=1`)
				require.NoError(t, err)
			case "credentials":
				statement = `UPDATE accounts SET credentials=jsonb_set(credentials,'{access_token}','"changed"') WHERE id=$1`
			case "disabled":
				statement = `UPDATE ticket_borrow_targets SET enabled=false WHERE account_id=$1`
			case "paused":
				_, err := integrationDB.Exec(`UPDATE ticket_borrow_settings SET running=false WHERE id=1`)
				require.NoError(t, err)
			case "account":
				statement = `UPDATE accounts SET schedulable=false WHERE id=$1`
			}
			if statement != "" {
				_, err := integrationDB.Exec(statement, a.TargetID)
				require.NoError(t, err)
			}
			before, _ := targetLeaseTimes(t, a.TargetID)
			require.ErrorIs(t, r.RenewTargetLease(t.Context(), a), service.ErrBorrowUnavailable)
			after, _ := targetLeaseTimes(t, a.TargetID)
			require.Equal(t, before, after, "rejected renewal cannot revive or lengthen ownership")
		})
	}
}

func TestBorrowTargetLeaseClaimAndPublicationKeepIndependentDeadlines(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 1)
	c := f.enqueue(t, f.reserve(t))
	a := f.targets[0]
	before, _ := targetLeaseTimes(t, a.TargetID)
	claimed, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	after, _ := targetLeaseTimes(t, a.TargetID)
	require.Equal(t, before, after, "claiming cannot install a second target lease policy")
	var claimUntil time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT lease_until FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID).Scan(&claimUntil))
	require.True(t, claimUntil.After(after), "a candidate may validly outlive one ownership interval")
	require.NoError(t, f.r.RemoveCandidatePoolDemand(t.Context(), a))
	require.NoError(t, f.r.RenewTargetLease(t.Context(), a), "closing demand retains ownership while results drain")
	route := poolCandidateRoute(a, claimed)
	require.NoError(t, f.r.Publish(service.WithBorrowAcquisition(t.Context(), a), route, a.Lease))
	passBorrowFixtureQuality(t)
	var deadline time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT admission_until FROM ticket_borrow_routes WHERE id=$1`, route.ID).Scan(&deadline))
	require.WithinDuration(t, c.Expires, deadline, time.Microsecond)
	require.NoError(t, f.r.FinishJob(t.Context(), "target", a.TargetID, a.Lease, "target_probe_passed", 0))
	require.ErrorIs(t, f.r.RenewTargetLease(t.Context(), a), service.ErrBorrowUnavailable)
}

func TestBorrowTargetLeaseExpiredOwnerCannotPublishOrReplaceActiveHTTP(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 1)
	f.enqueue(t, f.reserve(t))
	a := f.targets[0]
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	claimed, err := f.r.ClaimCandidatePool(ctx, a)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	q := service.BorrowRequest{ID: uuid.NewString(), TargetID: a.TargetID, Kind: "probe", BatchID: a.Lease, Revision: a.Revision, Identity: a.Identity, Phase: "mint", CandidateID: claimed.ID}
	require.NoError(t, f.r.BeginTargetRequest(ctx, q))
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE account_id=$1`, a.TargetID)
	require.NoError(t, err)
	require.ErrorIs(t, f.r.RenewTargetLease(ctx, a), service.ErrBorrowUnavailable)
	require.Error(t, f.r.Publish(ctx, poolCandidateRoute(a, claimed), a.Lease))
	ok, err := f.r.AcquireJob(ctx, "target", a.TargetID, "new-owner")
	require.NoError(t, err)
	require.False(t, ok, "short task leases must not let a second owner overlap surviving HTTP work")
	require.NoError(t, f.r.EndTargetRequest(ctx, q, 0, "cancelled"))
}

func TestBorrowTargetLeaseRenewAndReleaseRaceCannotResurrectOwner(t *testing.T) {
	r, a := candidateFixture(t)
	var workers sync.WaitGroup
	for range 10 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := r.RenewTargetLease(t.Context(), a)
			if err != nil {
				require.ErrorIs(t, err, service.ErrBorrowUnavailable)
			}
		}()
	}
	require.NoError(t, r.FinishJob(t.Context(), "target", a.TargetID, a.Lease, "done", 0))
	workers.Wait()
	var token string
	var until sql.NullTime
	require.NoError(t, integrationDB.QueryRow(`SELECT lease_token,lease_until FROM ticket_borrow_targets WHERE account_id=$1`, a.TargetID).Scan(&token, &until))
	require.Empty(t, token)
	require.False(t, until.Valid)
	require.ErrorIs(t, r.RenewTargetLease(t.Context(), a), service.ErrBorrowUnavailable)
}
