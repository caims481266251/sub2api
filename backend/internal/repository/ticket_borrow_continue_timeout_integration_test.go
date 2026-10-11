//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowContinueTimeoutPersistenceAndRejection(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	settings, err := r.Settings(ctx)
	require.NoError(t, err)
	require.NotNil(t, settings.ContinueTimeoutSeconds)
	require.Equal(t, 7, *settings.ContinueTimeoutSeconds)
	original := *settings.ContinueTimeoutSeconds
	t.Cleanup(func() {
		v, e := r.Settings(context.Background())
		if e == nil {
			v.ContinueTimeoutSeconds = &original
			_ = r.SaveSettings(context.Background(), v)
		}
	})
	seconds := 9
	settings.ContinueTimeoutSeconds = &seconds
	require.NoError(t, r.SaveSettings(ctx, settings))
	require.ErrorIs(t, r.SaveSettings(ctx, settings), service.ErrBorrowConflict)
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 9, *settings.ContinueTimeoutSeconds)
	settings.ContinueTimeoutSeconds = nil
	require.NoError(t, r.SaveSettings(ctx, settings))
	settings, err = r.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 9, *settings.ContinueTimeoutSeconds)
	for _, n := range []int{0, -1, 46} {
		_, err = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_settings SET continue_timeout_seconds=$1 WHERE id=1`, n)
		require.Error(t, err)
	}
	renewBorrowTestJob(t, r, q)
	q.Phase = "continue"
	q.ContinueTimeoutSeconds = 9
	require.NoError(t, r.BeginTargetRequest(ctx, q))
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "target_continue_timeout"))
	var recorded int
	var outcome string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT (details->>'continue_timeout_seconds')::int,outcome FROM ticket_borrow_requests WHERE id=$1`, q.ID).Scan(&recorded, &outcome))
	require.Equal(t, 9, recorded)
	require.Equal(t, "target_continue_timeout", outcome)
	require.NoError(t, r.RecordProbeOutcome(ctx, id, q.BatchID, "target_continue_timeout", 0))
	var failures, active int
	var waiting bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT consecutive_failures,COALESCE(upstream_wait_until>clock_timestamp(),false),(SELECT count(*) FROM ticket_borrow_requests WHERE target_id=$1 AND finished_at IS NULL) FROM ticket_borrow_targets WHERE account_id=$1`, id).Scan(&failures, &waiting, &active))
	require.Equal(t, 1, failures)
	require.False(t, waiting)
	require.Zero(t, active)
	expiry := time.Now().Add(time.Minute)
	require.NoError(t, r.RejectCandidate(ctx, id, "identity", "timed-out-cookie", expiry))
	known, err := r.CandidateKnown(ctx, id, "identity", "timed-out-cookie")
	require.NoError(t, err)
	require.True(t, known)
	known, err = r.CandidateKnown(ctx, id, "different-identity", "timed-out-cookie")
	require.NoError(t, err)
	require.False(t, known)
}
