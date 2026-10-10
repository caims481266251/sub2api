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

func TestTicketBorrowRenewDefaultMigrationPreservesSavedSettings(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA renew_default_migration_fixture; SET LOCAL search_path TO renew_default_migration_fixture;
 CREATE TABLE ticket_borrow_settings(id INTEGER PRIMARY KEY,revision BIGINT DEFAULT 1,renew_before_seconds INTEGER DEFAULT 15);
 INSERT INTO ticket_borrow_settings VALUES(1,1,15),(2,2,15),(3,5,0),(4,3,210);
 CREATE TABLE ticket_borrow_targets(account_id BIGINT PRIMARY KEY,renew_before_seconds INTEGER DEFAULT 15);
 INSERT INTO ticket_borrow_targets VALUES(1,60),(2,0),(3,15);`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("283_ticket_borrow_renew_default.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.Exec(string(raw))
		require.NoError(t, err)
	}
	_, err = tx.Exec(`INSERT INTO ticket_borrow_settings(id) VALUES(5); INSERT INTO ticket_borrow_targets(account_id) VALUES(4)`)
	require.NoError(t, err)
	for id, want := range map[int][2]int{1: {2, 180}, 2: {2, 15}, 3: {5, 0}, 4: {3, 210}, 5: {1, 180}} {
		var revision, renew int
		require.NoError(t, tx.QueryRow(`SELECT revision,renew_before_seconds FROM ticket_borrow_settings WHERE id=$1`, id).Scan(&revision, &renew))
		require.Equal(t, want, [2]int{revision, renew})
	}
	for id, want := range map[int]int{1: 60, 2: 0, 3: 15, 4: 180} {
		var renew int
		require.NoError(t, tx.QueryRow(`SELECT renew_before_seconds FROM ticket_borrow_targets WHERE account_id=$1`, id).Scan(&renew))
		require.Equal(t, want, renew)
	}
}
