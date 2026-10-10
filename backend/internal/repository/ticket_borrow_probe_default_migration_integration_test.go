//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowProbeDefaultMigrationPreservesExistingPolicies(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA probe_default_migration_fixture; SET LOCAL search_path TO probe_default_migration_fixture;
 CREATE TABLE ticket_borrow_targets(account_id BIGINT PRIMARY KEY, probe_concurrency INTEGER NOT NULL DEFAULT 3);
 INSERT INTO ticket_borrow_targets(account_id) VALUES(1);
 INSERT INTO ticket_borrow_targets VALUES(2,5);`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("280_ticket_borrow_probe_concurrency_default.sql")
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO ticket_borrow_targets(account_id) VALUES(3)`)
	require.NoError(t, err)
	for id, expected := range map[int]int{1: 3, 2: 5, 3: 2} {
		var concurrency int
		require.NoError(t, tx.QueryRow(`SELECT probe_concurrency FROM ticket_borrow_targets WHERE account_id=$1`, id).Scan(&concurrency))
		require.Equal(t, expected, concurrency)
	}
}
