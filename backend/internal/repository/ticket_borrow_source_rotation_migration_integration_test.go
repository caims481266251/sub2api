//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowSourceRotationMigrationSeedsHistoryAndRetiresTargetJobs(t *testing.T) {
	tx, e := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, e)
	defer tx.Rollback()
	_, e = tx.Exec(`CREATE SCHEMA source_rotation_migration_fixture; SET LOCAL search_path TO source_rotation_migration_fixture;
 CREATE TABLE accounts(id BIGINT PRIMARY KEY); CREATE TABLE proxy_pool_entries(id BIGINT PRIMARY KEY);
 CREATE TABLE ticket_borrow_selection(id BIGINT PRIMARY KEY);INSERT INTO ticket_borrow_selection VALUES(1);
 INSERT INTO accounts VALUES(1),(2); INSERT INTO proxy_pool_entries VALUES(11),(12);`)
	require.NoError(t, e)
	for _, name := range []string{"275_ticket_borrow_proxy_bindings.sql", "276_ticket_borrow_rotation.sql"} {
		raw, e := migrations.FS.ReadFile(name)
		require.NoError(t, e)
		_, e = tx.Exec(string(raw))
		require.NoError(t, e)
	}
	_, e = tx.Exec(`INSERT INTO ticket_borrow_proxy_bindings(account_id,entry_id,device_id,role) VALUES(1,11,'existing-source-device','source'),(2,12,'existing-target-device','target');
 INSERT INTO ticket_borrow_rotation_jobs(id,role,trigger_kind,account_ids) VALUES('target-job','target','automatic','[2]'),('source-job','source','manual','[1]');
 INSERT INTO ticket_borrow_rotation_accounts(account_id,role,automatic,active_job) VALUES(2,'target',true,'target-job'),(1,'source',false,'source-job');
 INSERT INTO ticket_borrow_rotation_assignments(job_id,account_id,entry_id,previous_entry_id,device_id,environment_version) VALUES('target-job',2,11,12,'pending-target-device',2),('source-job',1,12,11,'pending-source-device',2);`)
	require.NoError(t, e)
	raw, e := migrations.FS.ReadFile("277_ticket_borrow_source_rotation_history.sql")
	require.NoError(t, e)
	_, e = tx.Exec(string(raw))
	require.NoError(t, e)
	var count int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_usage`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_accounts WHERE role='target'`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_assignments`).Scan(&count))
	require.Zero(t, count)
	var phase, device string
	require.NoError(t, tx.QueryRow(`SELECT phase FROM ticket_borrow_rotation_jobs WHERE id='target-job'`).Scan(&phase))
	require.Equal(t, "cancelled", phase)
	require.NoError(t, tx.QueryRow(`SELECT phase FROM ticket_borrow_rotation_jobs WHERE id='source-job'`).Scan(&phase))
	require.Equal(t, "cancelled", phase)
	require.NoError(t, tx.QueryRow(`SELECT device_id FROM ticket_borrow_proxy_bindings WHERE account_id=1`).Scan(&device))
	require.Equal(t, "existing-source-device", device)
	require.NoError(t, tx.QueryRow(`SELECT device_id FROM ticket_borrow_proxy_bindings WHERE account_id=2`).Scan(&device))
	require.Equal(t, "existing-target-device", device)
	_, e = tx.Exec(`UPDATE ticket_borrow_rotation_accounts SET automatic=true WHERE account_id=1`)
	require.NoError(t, e)
	require.NoError(t, seedBorrowFingerprintHistory(t.Context(), tx))
	hash, tls := service.BorrowFingerprintHashes("existing-source-device")
	var saved string
	require.NoError(t, tx.QueryRow(`SELECT tls_profile_hash FROM ticket_borrow_fingerprint_history WHERE fingerprint_hash=$1`, hash).Scan(&saved))
	require.Equal(t, tls, saved)
}
