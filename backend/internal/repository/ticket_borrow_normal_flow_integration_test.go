//go:build integration

package repository

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTicketBorrowNormalFlowMigrationPreservesHealthAndRetiresOnlyOldDelays(t *testing.T) {
	r, _ := borrowIntegrationFixture(t)
	selection := borrowSelectionFixture(t, r)
	a, b := borrowNormalAccountFixture(t), borrowNormalAccountFixture(t)
	selection.AccountIDs = []int64{a.ID, b.ID}
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	_, err := r.Sources(t.Context())
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_selection SET use_proxy_pool=false,rotate_identity=true WHERE id=1`)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_sources SET last_reason='source_http_429',next_attempt_at=NOW()+INTERVAL '10 minutes' WHERE account_id=$1`, a.ID)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_sources SET last_reason='source_request_budget',next_attempt_at=NOW()+INTERVAL '10 minutes' WHERE account_id=$1`, b.ID)
	require.NoError(t, err)
	reset := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)
	_, err = integrationDB.Exec(`UPDATE accounts SET rate_limit_reset_at=$2 WHERE id=$1`, a.ID, reset)
	require.NoError(t, err)
	migration, err := os.ReadFile("../../migrations/287_ticket_borrow_normal_flow.sql")
	require.NoError(t, err)
	_, err = integrationDB.Exec(string(migration))
	require.NoError(t, err)
	saved, err := r.Selection(t.Context())
	require.NoError(t, err)
	require.True(t, saved.UseProxyPool)
	require.False(t, saved.RotateIdentity)
	var rateReset time.Time
	var due bool
	var reason string
	require.NoError(t, integrationDB.QueryRow(`SELECT a.rate_limit_reset_at,s.next_attempt_at<=clock_timestamp(),s.last_reason FROM accounts a JOIN ticket_borrow_sources s ON s.account_id=a.id WHERE a.id=$1`, a.ID).Scan(&rateReset, &due, &reason))
	require.WithinDuration(t, reset, rateReset, time.Microsecond)
	require.True(t, due)
	require.Equal(t, "source_http_429", reason)
	require.NoError(t, integrationDB.QueryRow(`SELECT next_attempt_at>clock_timestamp() FROM ticket_borrow_sources WHERE account_id=$1`, b.ID).Scan(&due))
	require.True(t, due)
}
