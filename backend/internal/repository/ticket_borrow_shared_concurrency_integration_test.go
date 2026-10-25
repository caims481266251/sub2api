//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestBorrowSharedConcurrencyMigrationPreservesRoutesAndLeases(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA shared_capacity_fixture; SET LOCAL search_path TO shared_capacity_fixture;
 CREATE TABLE accounts(id BIGINT PRIMARY KEY, concurrency INT);
 INSERT INTO accounts VALUES(1,10);
 CREATE TABLE ticket_borrow_routes(id TEXT PRIMARY KEY,state TEXT,generation BIGINT,
 dispatch_limit INT NOT NULL DEFAULT 3 CHECK(dispatch_limit>0),
 quality_result JSONB,quality_valid_until TIMESTAMPTZ,cooldown_until TIMESTAMPTZ);
 INSERT INTO ticket_borrow_routes VALUES
 ('new','pending_quality',1,3,'{}',NULL,NULL),
 ('healthy','normal',2,7,'{"status":"passed","answer":"21"}',NOW()+INTERVAL '30 seconds',NULL),
 ('cooling','cooling',3,3,'{"status":"failed"}',NULL,NOW()+INTERVAL '2 minutes'),
 ('expired','expired',4,1,'{}',NULL,NULL);
 CREATE TABLE ticket_borrow_route_leases(id TEXT,route_id TEXT,generation BIGINT,lease_until TIMESTAMPTZ);
 INSERT INTO ticket_borrow_route_leases VALUES('sent','healthy',2,NOW()+INTERVAL '60 seconds');`)
	require.NoError(t, err)
	snapshot := func() string {
		t.Helper()
		var value string
		require.NoError(t, tx.QueryRow(`SELECT jsonb_build_object(
 'routes',(SELECT jsonb_agg(to_jsonb(r)-'dispatch_limit' ORDER BY id) FROM ticket_borrow_routes r),
 'leases',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM ticket_borrow_route_leases l),
 'accounts',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM accounts a))::text`).Scan(&value))
		return value
	}
	before := snapshot()
	raw, err := migrations.FS.ReadFile("309_ticket_borrow_shared_account_concurrency.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.Exec(string(raw))
		require.NoError(t, err)
		require.JSONEq(t, before, snapshot(), "migration must preserve eligibility, quality, cooldown, active leases and account capacity")
	}
	var columns int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema='shared_capacity_fixture' AND table_name='ticket_borrow_routes' AND column_name='dispatch_limit'`).Scan(&columns))
	require.Zero(t, columns)
}
