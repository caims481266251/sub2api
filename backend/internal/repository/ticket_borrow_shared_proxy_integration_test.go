//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowCandidateRecoveredProxyJoinsCurrentRound(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	first, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "recovered-subject")
	require.NoError(t, err)
	require.NoError(t, r.ReleaseCandidateEnvironment(ctx, first.ID))
	pool := NewProxyPoolRepository(integrationDB, r.encryptor)
	state, err := pool.Load(ctx)
	require.NoError(t, err)
	state, err = pool.SetEnabled(ctx, state.Revision, first.EntryID, false)
	require.NoError(t, err)
	// This target performs no allocations while the entry is disabled.
	rotationExec(t, `UPDATE ticket_borrow_candidate_proxy_usage SET uses=100 WHERE identity_key='recovered-subject' AND entry_id<>$1`, first.EntryID)
	_, err = pool.SetEnabled(ctx, state.Revision, first.EntryID, true)
	require.NoError(t, err)
	seen := map[int64]bool{}
	for range 4 {
		env, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "recovered-subject")
		require.NoError(t, err)
		require.EqualValues(t, 101, env.AllocationRound)
		require.False(t, seen[env.EntryID], "a recovered entry must not monopolize sends to catch up with history")
		seen[env.EntryID] = true
		_, _, err = r.StartCandidateEnvironment(ctx, env.ID)
		require.NoError(t, err)
		require.NoError(t, r.MarkCandidateEnvironmentSent(ctx, env.ID))
		require.NoError(t, r.ReleaseCandidateEnvironment(ctx, env.ID))
	}
	var uses int
	require.NoError(t, integrationDB.QueryRow(`SELECT uses FROM ticket_borrow_candidate_proxy_usage WHERE identity_key='recovered-subject' AND entry_id=$1`, first.EntryID).Scan(&uses))
	require.Equal(t, 1, uses, "round offset cannot inflate actual send statistics")
}
