//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowLegacyRotationDoesNotGateNewAcquisition(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	input := rotationInput("source", sources[0].AccountID)
	rotationExec(t, `INSERT INTO ticket_borrow_rotation_jobs(id,role,trigger_kind,account_ids,requested_account_ids) VALUES($1,'source','automatic',jsonb_build_array($2::bigint),jsonb_build_array($2::bigint))`, input.ID, sources[0].AccountID)
	rotationExec(t, `INSERT INTO ticket_borrow_rotation_accounts(account_id,role,automatic,active_job,phase) VALUES($1,'source',true,$2,'draining')`, sources[0].AccountID, input.ID)
	require.NoError(t, r.ProcessRotations(t.Context()))
	require.Equal(t, "draining", rotationPhase(t, input.ID), "retired processing cannot apply an old environment")
	source, err := r.Source(t.Context(), sources[0].ID)
	require.NoError(t, err)
	require.True(t, source.Enabled)
	require.Empty(t, source.Secret.Proxies)
	require.Nil(t, source.Secret.Fingerprint)
	ok, err := r.AcquireJob(t.Context(), "source", source.ID, "compatibility-worker")
	require.NoError(t, err)
	require.True(t, ok, "legacy rotation state is not a send gate")
	require.NoError(t, r.FinishJob(t.Context(), "source", source.ID, "compatibility-worker", "completed", 0))
	require.ErrorIs(t, r.SaveRotationPolicy(t.Context(), source.AccountID, service.BorrowRotationPolicy{}), service.ErrBorrowRotationRetired)
}
