//go:build integration

package repository

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowHistorical401MarkerDoesNotOverrideNormalRecovery(t *testing.T) {
	_, pool, enc := poolFixture(t)
	r, _ := borrowIntegrationFixture(t)
	selection := borrowSelectionFixture(t, r)
	a := borrowNormalAccountFixture(t)
	selection.AccountIDs = []int64{a.ID}
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	_, err := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "status.example:80"})
	require.NoError(t, err)
	sources, err := r.Sources(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	source := sources[0]
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("normal-token")))
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_sources SET auth_rejected_token_hash=$2,auth_rejected_at=NOW(),last_reason='source_auth_excluded' WHERE id=$1`, source.ID, hash)
	require.NoError(t, err)
	restarted := NewTicketBorrowRepository(integrationDB, enc)
	current, err := restarted.Source(t.Context(), source.ID)
	require.NoError(t, err)
	require.True(t, current.Enabled)
	require.Equal(t, source.PoolEntryID, current.PoolEntryID)
	options, err := restarted.SourceOptions(t.Context())
	require.NoError(t, err)
	for _, o := range options.Accounts {
		if o.ID == a.ID {
			require.False(t, o.AuthExcluded)
		}
	}
	_, err = integrationDB.Exec(`UPDATE accounts SET temp_unschedulable_until=NOW()+INTERVAL '10 minutes' WHERE id=$1`, a.ID)
	require.NoError(t, err)
	_, err = restarted.Source(t.Context(), source.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	_, err = integrationDB.Exec(`UPDATE accounts SET temp_unschedulable_until=NULL WHERE id=$1`, a.ID)
	require.NoError(t, err)
	current, err = restarted.Source(t.Context(), source.ID)
	require.NoError(t, err)
	require.True(t, current.Enabled)
	var retained string
	require.NoError(t, integrationDB.QueryRow(`SELECT auth_rejected_token_hash FROM ticket_borrow_sources WHERE id=$1`, source.ID).Scan(&retained))
	require.Equal(t, hash, retained)
}

func TestTicketBorrowSourceSharesNormalQuotaCooldownAndRecovery(t *testing.T) {
	_, pool, _ := poolFixture(t)
	_, err := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "status.example:80"})
	require.NoError(t, err)
	r, _ := borrowIntegrationFixture(t)
	selection := borrowSelectionFixture(t, r)
	a := borrowNormalAccountFixture(t)
	selection.AccountIDs, selection.UseProxyPool = []int64{a.ID}, false
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	reset := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
	_, err = integrationDB.Exec(`UPDATE accounts SET rate_limit_reset_at=$2 WHERE id=$1`, a.ID, reset)
	require.NoError(t, err)
	sources, err := r.Sources(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.False(t, sources[0].Enabled)
	require.Equal(t, "source_rate_limited", sources[0].UnavailableReason)
	require.NotNil(t, sources[0].RateLimitResetAt)
	require.WithinDuration(t, reset, *sources[0].RateLimitResetAt, time.Microsecond)
	_, err = r.Source(t.Context(), sources[0].ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	_, err = integrationDB.Exec(`UPDATE accounts SET rate_limit_reset_at=NOW()-INTERVAL '1 second' WHERE id=$1`, a.ID)
	require.NoError(t, err)
	source, err := r.Source(t.Context(), sources[0].ID)
	require.NoError(t, err)
	require.True(t, source.Enabled)
	require.Empty(t, source.UnavailableReason)
}
