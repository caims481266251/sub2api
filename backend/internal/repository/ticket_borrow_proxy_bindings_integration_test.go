//go:build integration

package repository

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowProxyBindingsStableSharedAndFailClosed(t *testing.T) {
	_, pool, enc := poolFixture(t)
	r, target := borrowIntegrationFixture(t)
	selection := borrowSelectionFixture(t, r)
	a, b := borrowNormalAccountFixture(t), borrowNormalAccountFixture(t)
	selection.AccountIDs = []int64{a.ID, b.ID, target}
	selection.UseProxyPool, selection.RotateIdentity = true, true
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	saved, err := r.Selection(t.Context())
	require.NoError(t, err)
	require.False(t, saved.RotateIdentity)
	state, err := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "a.example:80:user:private-password"})
	require.NoError(t, err)
	sources, err := r.Sources(t.Context())
	require.NoError(t, err)
	byAccount := map[int64]service.BorrowSource{}
	for _, s := range sources {
		byAccount[s.AccountID] = s
	}
	first := byAccount[a.ID]
	require.True(t, first.Enabled)
	require.NotEmpty(t, first.FingerprintID)
	require.Equal(t, state.Entries[0].ID, first.PoolEntryID)
	require.Empty(t, byAccount[b.ID].UnavailableReason)
	require.True(t, byAccount[b.ID].Enabled)
	require.Equal(t, first.PoolEntryID, byAccount[b.ID].PoolEntryID)
	require.NotEqual(t, first.FingerprintID, byAccount[b.ID].FingerprintID)
	require.Equal(t, "source_is_target", byAccount[target].UnavailableReason)
	full, err := r.Source(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"http://user:private-password@a.example:80"}, full.Secret.Proxies)
	public, err := json.Marshal(full)
	require.NoError(t, err)
	require.NotContains(t, string(public), "private-password")
	require.NotContains(t, string(public), "user:")
	require.NotContains(t, string(public), full.Secret.DeviceID)
	var normalProxy *int64
	require.NoError(t, integrationDB.QueryRow(`SELECT proxy_id FROM accounts WHERE id=$1`, a.ID).Scan(&normalProxy))
	require.Nil(t, normalProxy, "borrow binding must not change the normal account proxy")

	// Reconstructed repositories and concurrent reconcilers retain the same binding.
	restarted := NewTicketBorrowRepository(integrationDB, enc)
	var wg sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := restarted.Sources(t.Context()); errors <- e }()
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		require.NoError(t, e)
	}
	again, err := restarted.Source(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, full.Secret, again.Secret)
	require.Equal(t, full.FingerprintID, again.FingerprintID)

	// An administrative disable immediately fences new sends, before reconciliation.
	state, err = pool.SetEnabled(t.Context(), state.Revision, first.PoolEntryID, false)
	require.NoError(t, err)
	_, err = restarted.Source(t.Context(), first.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	state, err = pool.Import(t.Context(), service.ProxyPoolImport{Revision: state.Revision, Mode: "append", Text: "b.example:81\nc.example:82"})
	require.NoError(t, err)
	sources, err = restarted.Sources(t.Context())
	require.NoError(t, err)
	for _, s := range sources {
		byAccount[s.AccountID] = s
	}
	require.True(t, byAccount[a.ID].Enabled)
	require.True(t, byAccount[b.ID].Enabled)
	require.NotEqual(t, first.PoolEntryID, byAccount[a.ID].PoolEntryID)
	require.NotEqual(t, first.FingerprintID, byAccount[a.ID].FingerprintID)
	require.NotEqual(t, byAccount[a.ID].PoolEntryID, byAccount[b.ID].PoolEntryID)

	// Removal drains a live source lease before making its proxy reusable.
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_sources SET lease_until=NOW()+INTERVAL '1 minute' WHERE id=$1`, first.ID)
	require.NoError(t, err)
	saved.AccountIDs = []int64{b.ID}
	require.NoError(t, restarted.SaveSelection(t.Context(), saved))
	sources, err = restarted.Sources(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.Equal(t, byAccount[b.ID].FingerprintID, sources[0].FingerprintID)
	var bindings int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_bindings`).Scan(&bindings))
	require.Equal(t, 2, bindings)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_sources SET lease_until=NULL WHERE id=$1`, first.ID)
	require.NoError(t, err)
	_, err = restarted.Sources(t.Context())
	require.NoError(t, err)
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_bindings`).Scan(&bindings))
	require.Equal(t, 1, bindings)
}
