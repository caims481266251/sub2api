//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func rotationExec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, e := integrationDB.ExecContext(context.Background(), query, args...)
	require.NoError(t, e)
}
func rotationInput(role string, ids ...int64) service.BorrowRotationInput {
	return service.BorrowRotationInput{ID: uuid.NewString(), Role: role, AccountIDs: ids}
}
func rotationPhase(t *testing.T, id string) string {
	t.Helper()
	var phase string
	require.NoError(t, integrationDB.QueryRow(`SELECT phase FROM ticket_borrow_rotation_jobs WHERE id=$1`, id).Scan(&phase))
	return phase
}
func rotationSourceFixture(t *testing.T) (*ticketBorrowRepository, *service.ProxyPoolService, []service.BorrowSource) {
	t.Helper()
	_, pool, _ := poolFixture(t)
	repo, _ := borrowIntegrationFixture(t)
	r := repo.(*ticketBorrowRepository)
	a, b := borrowNormalAccountFixture(t), borrowNormalAccountFixture(t)
	selection := borrowSelectionFixture(t, r)
	selection.AccountIDs = []int64{a.ID, b.ID}
	selection.UseProxyPool = true
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	_, e := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "a.example:80:user:secret-password\nb.example:81"})
	require.NoError(t, e)
	sources, e := r.Sources(t.Context())
	require.NoError(t, e)
	require.Len(t, sources, 2)
	return r, pool, sources
}
func rotationAppend(t *testing.T, pool *service.ProxyPoolService, text string) map[string]int64 {
	t.Helper()
	state, e := pool.State(t.Context())
	require.NoError(t, e)
	state, e = pool.Import(t.Context(), service.ProxyPoolImport{Revision: state.Revision, Mode: "append", Text: text})
	require.NoError(t, e)
	result := map[string]int64{}
	for _, entry := range state.Entries {
		result[entry.Host] = entry.ID
	}
	return result
}

func TestTicketBorrowRotationRetiredAtRepositoryBoundary(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	for _, trigger := range []string{"manual", "automatic"} {
		job, err := r.QueueRotation(t.Context(), rotationInput("source", sources[0].AccountID), trigger)
		require.ErrorIs(t, err, service.ErrBorrowRotationRetired)
		require.Nil(t, job)
	}
	for _, automatic := range []bool{false, true} {
		require.ErrorIs(t, r.SaveRotationPolicy(t.Context(), sources[0].AccountID, service.BorrowRotationPolicy{Automatic: automatic, IntervalSeconds: 30}), service.ErrBorrowRotationRetired)
	}
	require.NoError(t, r.ProcessRotations(t.Context()))
	var bindings, jobs int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_bindings WHERE account_id IN ($1,$2)`, sources[0].AccountID, sources[1].AccountID).Scan(&bindings))
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_jobs WHERE finished_at IS NULL`).Scan(&jobs))
	require.Zero(t, bindings)
	require.Zero(t, jobs)
}
