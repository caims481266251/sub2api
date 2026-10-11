//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowGlobalProbeInheritanceOverrideAndLiveLimits(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	_, other, otherQ := borrowRecoveryFixture(t)
	renewBorrowTestJob(t, r, q)
	ctx := t.Context()
	settings, err := r.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, *settings.ProbeConcurrency)
	require.Equal(t, 15, *settings.RenewBeforeSeconds)
	for _, targetID := range []int64{id, other} {
		target, e := r.Target(ctx, targetID)
		require.NoError(t, e)
		require.True(t, target.ProbePolicy.UseGlobal)
		require.Equal(t, 2, target.ProbePolicy.Concurrency)
	}
	// Each inheriting account gets its own two slots, not a shared system limit.
	var first []service.BorrowRequest
	for _, request := range []service.BorrowRequest{q, otherQ} {
		for range 2 {
			request.ID = uuid.NewString()
			require.NoError(t, r.BeginTargetRequest(ctx, request))
			if request.TargetID == id {
				first = append(first, request)
			}
		}
		request.ID = uuid.NewString()
		require.ErrorIs(t, r.BeginTargetRequest(ctx, request), service.ErrBorrowProbeBusy)
	}
	settings.ProbeConcurrency, settings.RenewBeforeSeconds = new(1), new(180)
	require.NoError(t, r.SaveSettings(ctx, settings))
	stale := settings
	stale.ProbeConcurrency = new(8)
	require.ErrorIs(t, r.SaveSettings(ctx, stale), service.ErrBorrowConflict)
	target, err := r.Target(ctx, id)
	require.NoError(t, err)
	require.True(t, target.ProbePolicy.UseGlobal)
	require.Equal(t, 1, target.ProbePolicy.Concurrency)
	require.Equal(t, 15, target.ProbePolicy.RenewBeforeSeconds)
	require.Equal(t, 2, target.Inflight, "lowering limits does not cancel existing requests")
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowUnavailable, "configuration edit fences old batch")
	require.NoError(t, r.EndTargetRequest(ctx, first[0], 200, "eof"))
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowUnavailable, "configuration edit fences old batch")
	require.NoError(t, r.EndTargetRequest(ctx, first[1], 200, "eof"))
	renewBorrowTestJob(t, r, q)
	require.NoError(t, r.BeginTargetRequest(ctx, q))
	// Explicit account values remain independent of subsequent global edits.
	otherTarget, err := r.Target(ctx, other)
	require.NoError(t, err)
	policy := otherTarget.ProbePolicy
	policy.UseGlobal, policy.Concurrency, policy.RenewBeforeSeconds = false, 3, 90
	require.NoError(t, r.(service.BorrowProbePolicyRepository).SaveProbePolicy(ctx, other, policy))
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	settings.ProbeConcurrency, settings.RenewBeforeSeconds = new(4), new(150)
	require.NoError(t, r.SaveSettings(ctx, settings))
	otherTarget, err = r.Target(ctx, other)
	require.NoError(t, err)
	require.False(t, otherTarget.ProbePolicy.UseGlobal)
	require.Equal(t, 3, otherTarget.ProbePolicy.Concurrency)
	require.Equal(t, 15, otherTarget.ProbePolicy.RenewBeforeSeconds)
	require.Equal(t, 4, otherTarget.ProbePolicy.GlobalConcurrency)
	require.Equal(t, 15, otherTarget.ProbePolicy.GlobalRenewBeforeSeconds)
	rotationExec(t, `UPDATE ticket_borrow_requests SET finished_at=clock_timestamp(),outcome='configuration_changed' WHERE target_id=$1 AND finished_at IS NULL`, other)
	renewBorrowTestJob(t, r, otherQ)
	for range 3 {
		otherQ.ID = uuid.NewString()
		require.NoError(t, r.BeginTargetRequest(ctx, otherQ))
	}

	otherQ.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, otherQ), service.ErrBorrowProbeBusy)
	// Restore inheritance with only the explicit flag and revision.
	policy = service.BorrowProbePolicy{Revision: otherTarget.ProbePolicy.Revision, UseGlobal: true}
	require.NoError(t, r.(service.BorrowProbePolicyRepository).SaveProbePolicy(ctx, other, policy))
	otherTarget, err = r.Target(ctx, other)
	require.NoError(t, err)
	require.True(t, otherTarget.ProbePolicy.UseGlobal)
	require.Equal(t, 4, otherTarget.ProbePolicy.Concurrency)
	require.Equal(t, 15, otherTarget.ProbePolicy.RenewBeforeSeconds)
	require.ErrorIs(t, r.BeginTargetRequest(ctx, otherQ), service.ErrBorrowUnavailable, "restoring inheritance fences the prior batch")
	// Older clients omitting the new globals preserve administrator settings.
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	settings.ProbeConcurrency, settings.RenewBeforeSeconds = nil, nil
	require.NoError(t, r.SaveSettings(ctx, settings))
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, *settings.ProbeConcurrency)
	require.Equal(t, 15, *settings.RenewBeforeSeconds)
}

func TestTicketBorrowGlobalProbeMigrationPreservesAccountChoices(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA global_probe_migration_fixture; SET LOCAL search_path TO global_probe_migration_fixture;
 CREATE TABLE ticket_borrow_settings(id INTEGER PRIMARY KEY); INSERT INTO ticket_borrow_settings VALUES(1);
 CREATE TABLE ticket_borrow_targets(account_id BIGINT PRIMARY KEY,probe_policy_revision BIGINT DEFAULT 1,probe_concurrency INTEGER DEFAULT 2,renew_before_seconds INTEGER DEFAULT 15);
 INSERT INTO ticket_borrow_targets VALUES(1,1,2,15),(2,3,2,150),(3,2,2,15),(4,1,3,15);`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("282_ticket_borrow_global_probe_policy.sql")
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO ticket_borrow_targets(account_id) VALUES(5)`)
	require.NoError(t, err)
	for id, want := range map[int]bool{1: false, 2: true, 3: true, 4: true, 5: false} {
		var override bool
		require.NoError(t, tx.QueryRow(`SELECT probe_policy_override FROM ticket_borrow_targets WHERE account_id=$1`, id).Scan(&override))
		require.Equal(t, want, override)
	}
	var concurrency, renew int
	require.NoError(t, tx.QueryRow(`SELECT probe_concurrency,renew_before_seconds FROM ticket_borrow_settings WHERE id=1`).Scan(&concurrency, &renew))
	require.Equal(t, 2, concurrency)
	require.Equal(t, 15, renew)
	_, err = tx.Exec(`UPDATE ticket_borrow_targets SET probe_policy_override=false WHERE account_id=2`)
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	var override bool
	require.NoError(t, tx.QueryRow(`SELECT probe_policy_override FROM ticket_borrow_targets WHERE account_id=2`).Scan(&override))
	require.False(t, override, "rerunning migration must not overwrite a restored inheritance choice")
	require.NoError(t, tx.QueryRow(`SELECT probe_concurrency,renew_before_seconds FROM ticket_borrow_targets WHERE account_id=2`).Scan(&concurrency, &renew))
	require.Equal(t, 2, concurrency)
	require.Equal(t, 150, renew)
}
