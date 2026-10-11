//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowExcludedGroupMembersAndLegacyWrites(t *testing.T) {
	_, pool, _ := poolFixture(t)
	_, err := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "scope-test.example:80"})
	require.NoError(t, err)
	r, _ := borrowIntegrationFixture(t)
	ctx := t.Context()
	selection := borrowSelectionFixture(t, r)
	a, b := borrowNormalAccountFixture(t), borrowNormalAccountFixture(t)
	g := mustCreateGroup(t, testEntClient(t), &service.Group{Name: uuid.NewString(), Platform: service.PlatformOpenAI})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM account_groups WHERE group_id=$1`, g.ID)
		_, _ = integrationDB.Exec(`DELETE FROM groups WHERE id=$1`, g.ID)
	})
	_, err = integrationDB.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$3),($2,$3)`, a.ID, b.ID, g.ID)
	require.NoError(t, err)
	selection.GroupIDs = []int64{g.ID}
	selection.AccountIDs = []int64{a.ID}
	selection.ExcludedAccountIDs = []int64{}
	require.NoError(t, r.SaveSelection(ctx, selection))
	sources, err := r.Sources(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	var excludedSource int64
	for _, source := range sources {
		if source.AccountID == a.ID {
			excludedSource = source.ID
		}
	}
	selection, err = r.Selection(ctx)
	require.NoError(t, err)
	selection.ExcludedAccountIDs = []int64{a.ID}
	require.NoError(t, r.SaveSelection(ctx, selection))
	require.ErrorIs(t, r.SaveSelection(ctx, selection), service.ErrBorrowConflict)
	sources, err = r.Sources(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.Equal(t, b.ID, sources[0].AccountID)
	_, err = r.Source(ctx, excludedSource)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	var bindings int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, a.ID).Scan(&bindings))
	require.Zero(t, bindings)
	selection, err = r.Selection(ctx)
	require.NoError(t, err)
	require.Empty(t, selection.AccountIDs)
	// An old client editing other fields must not clear the exception or restore
	// a now-excluded explicit ID from its earlier representation.
	selection.ExcludedAccountIDs = nil
	selection.AccountIDs = []int64{a.ID}
	selection.DefaultModel = "updated-model"
	require.NoError(t, r.SaveSelection(ctx, selection))
	selection, err = r.Selection(ctx)
	require.NoError(t, err)
	require.Equal(t, []int64{a.ID}, selection.ExcludedAccountIDs)
	require.Empty(t, selection.AccountIDs)
	// Newly joined members remain dynamic while the exception persists.
	c := borrowNormalAccountFixture(t)
	_, err = integrationDB.Exec(`INSERT INTO account_groups(account_id,group_id) VALUES($1,$2)`, c.ID, g.ID)
	require.NoError(t, err)
	sources, err = r.Sources(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 2)
	selection.ExcludedAccountIDs = []int64{}
	require.NoError(t, r.SaveSelection(ctx, selection))
	sources, err = r.Sources(ctx)
	require.NoError(t, err)
	require.Len(t, sources, 3)
}
