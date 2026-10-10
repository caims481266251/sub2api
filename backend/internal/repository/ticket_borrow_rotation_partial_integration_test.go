//go:build integration

package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func rotationBindingSnapshot(t *testing.T, id int64) string {
	t.Helper()
	var value string
	require.NoError(t, integrationDB.QueryRow(`SELECT entry_id::text||':'||device_id||':'||environment_version::text FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, id).Scan(&value))
	return value
}

func TestTicketBorrowRotationPartialSkipsUnhealthyAndKeepsCooldown(t *testing.T) {
	r, pool, sources := rotationSourceFixture(t)
	rotationAppend(t, pool, "spare.example:82")
	good, bad := sources[0].AccountID, sources[1].AccountID
	rotationExec(t, `UPDATE accounts SET status='error',schedulable=false WHERE id=$1`, bad)
	rotationExec(t, `UPDATE accounts SET rate_limit_reset_at=clock_timestamp()+INTERVAL '1 hour' WHERE id=$1`, good)
	before := rotationBindingSnapshot(t, bad)
	listed, err := r.Sources(t.Context())
	require.NoError(t, err)
	for _, s := range listed {
		require.Equal(t, s.AccountID == good, s.CanRotate)
		if s.AccountID == bad {
			require.Equal(t, "source_account_unavailable", s.RotationUnavailableReason)
		}
		if s.AccountID == good {
			require.False(t, s.Enabled)
			require.Equal(t, "source_rate_limited", s.UnavailableReason)
		}
	}
	in := rotationInput("source", good, bad)
	job, err := r.QueueRotation(t.Context(), in, "manual")
	require.NoError(t, err)
	require.Equal(t, []int64{good}, job.AccountIDs)
	require.Equal(t, []service.BorrowRotationSkipped{{AccountID: bad, Reason: "source_account_unavailable"}}, job.Skipped)
	again, err := r.QueueRotation(t.Context(), in, "manual")
	require.NoError(t, err)
	require.Equal(t, job, again)
	require.NoError(t, r.processRotation(t.Context(), in.ID))
	require.Equal(t, "completed", rotationPhase(t, in.ID))
	require.Equal(t, before, rotationBindingSnapshot(t, bad))
	var stillCooling bool
	require.NoError(t, integrationDB.QueryRow(`SELECT rate_limit_reset_at>clock_timestamp() FROM accounts WHERE id=$1`, good).Scan(&stillCooling))
	require.True(t, stillCooling)
	// A lost response after completion still returns the original subset; it
	// cannot silently rotate a recovered source or accept a different request.
	rotationExec(t, `UPDATE accounts SET status='active',schedulable=true WHERE id=$1`, bad)
	again, err = r.QueueRotation(t.Context(), in, "manual")
	require.NoError(t, err)
	require.Equal(t, job.AccountIDs, again.AccountIDs)
	require.Equal(t, job.Skipped, again.Skipped)
	in.AccountIDs = []int64{good}
	_, err = r.QueueRotation(t.Context(), in, "manual")
	require.ErrorIs(t, err, service.ErrBorrowConflict)
}

func TestTicketBorrowRotationPartialAllSkipped(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	id := sources[0].AccountID
	rotationExec(t, `UPDATE accounts SET expires_at=clock_timestamp()-INTERVAL '1 second',auto_pause_on_expired=true WHERE id=$1`, id)
	in := rotationInput("source", id)
	before := rotationBindingSnapshot(t, id)
	job, err := r.QueueRotation(t.Context(), in, "manual")
	require.NoError(t, err)
	require.Empty(t, job.AccountIDs)
	require.Equal(t, "rotation_no_eligible_sources", job.Reason)
	require.Equal(t, []service.BorrowRotationSkipped{{AccountID: id, Reason: "source_account_expired"}}, job.Skipped)
	require.Equal(t, "completed", rotationPhase(t, in.ID))
	again, err := r.QueueRotation(t.Context(), in, "manual")
	require.NoError(t, err)
	require.Equal(t, job, again)
	require.Equal(t, before, rotationBindingSnapshot(t, id))
}

func TestTicketBorrowRotationPartialReplansAfterAccountFailsDuringDrain(t *testing.T) {
	for _, spare := range []bool{true, false} {
		t.Run(map[bool]string{true: "available_alternative", false: "full_pool_cannot_steal_skipped_binding"}[spare], func(t *testing.T) {
			r, pool, sources := rotationSourceFixture(t)
			if spare {
				rotationAppend(t, pool, "spare.example:82")
			}
			good, bad := sources[0].AccountID, sources[1].AccountID
			beforeGood, beforeBad := rotationBindingSnapshot(t, good), rotationBindingSnapshot(t, bad)
			ok, err := r.AcquireJob(t.Context(), "source", sources[0].ID, "inflight")
			require.NoError(t, err)
			require.True(t, ok)
			in := rotationInput("source", good, bad)
			_, err = r.QueueRotation(t.Context(), in, "manual")
			require.NoError(t, err)
			rotationExec(t, `UPDATE accounts SET status='error',schedulable=false WHERE id=$1`, bad)
			require.NoError(t, r.processRotation(t.Context(), in.ID))
			if spare {
				draining, err := r.QueueRotation(t.Context(), in, "manual")
				require.NoError(t, err)
				require.Equal(t, "draining", draining.Phase)
				require.Equal(t, []int64{good}, draining.AccountIDs)
				require.Len(t, draining.Skipped, 1)
			}
			require.Equal(t, beforeGood, rotationBindingSnapshot(t, good))
			require.Equal(t, beforeBad, rotationBindingSnapshot(t, bad))
			require.NoError(t, r.FinishJob(t.Context(), "source", sources[0].ID, "inflight", "done", 0))
			require.NoError(t, r.processRotation(t.Context(), in.ID))
			job, err := r.QueueRotation(t.Context(), in, "manual")
			require.NoError(t, err)
			require.Equal(t, []int64{good}, job.AccountIDs)
			require.Equal(t, []service.BorrowRotationSkipped{{AccountID: bad, Reason: "source_account_unavailable"}}, job.Skipped)
			if spare {
				require.Equal(t, "completed", job.Phase)
				require.NotEqual(t, beforeGood, rotationBindingSnapshot(t, good))
			} else {
				require.Equal(t, "failed", job.Phase)
				require.Equal(t, service.ErrBorrowRotationNoProxy.Error(), job.Reason)
			}
			require.Equal(t, beforeBad, rotationBindingSnapshot(t, bad))
			var active, assignments, skips int
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_accounts WHERE active_job=$1`, in.ID).Scan(&active))
			require.Zero(t, active)
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_assignments WHERE job_id=$1`, in.ID).Scan(&assignments))
			require.Zero(t, assignments)
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_events WHERE details->'rotation'->>'job_id'=$1 AND details->'rotation'->>'skipped'='true'`, in.ID).Scan(&skips))
			require.Equal(t, 1, skips)
		})
	}
}

func TestTicketBorrowRotationPartialKeepsOtherJobsReservations(t *testing.T) {
	r, pool, sources := rotationSourceFixture(t)
	rotationAppend(t, pool, "spare-a.example:82\nspare-b.example:83")
	a, b := sources[0].AccountID, sources[1].AccountID
	first := rotationInput("source", a)
	_, err := r.QueueRotation(t.Context(), first, "manual")
	require.NoError(t, err)
	next := rotationInput("source", a, b)
	job, err := r.QueueRotation(t.Context(), next, "manual")
	require.NoError(t, err)
	require.Equal(t, []int64{b}, job.AccountIDs)
	require.Equal(t, []service.BorrowRotationSkipped{{AccountID: a, Reason: service.ErrBorrowRotationPending.Error()}}, job.Skipped)
	var active string
	require.NoError(t, integrationDB.QueryRow(`SELECT active_job FROM ticket_borrow_rotation_accounts WHERE account_id=$1`, a).Scan(&active))
	require.Equal(t, first.ID, active)
	require.NoError(t, r.processRotation(t.Context(), next.ID))
	require.NoError(t, r.processRotation(t.Context(), first.ID))
	require.Equal(t, "completed", rotationPhase(t, next.ID))
	require.Equal(t, "completed", rotationPhase(t, first.ID))
	var distinct int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(DISTINCT entry_id) FROM ticket_borrow_proxy_bindings WHERE account_id IN ($1,$2)`, a, b).Scan(&distinct))
	require.Equal(t, 2, distinct)
}
