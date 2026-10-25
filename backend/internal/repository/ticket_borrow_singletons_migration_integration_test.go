//go:build integration

package repository

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const borrowSingletonRepairMigration = "311_ticket_borrow_restore_singletons.sql"

var borrowSingletonTables = []string{
	"ticket_borrow_settings",
	"ticket_borrow_selection",
	"ticket_borrow_source_arbiter",
	"ticket_borrow_candidate_pool_control",
	"ticket_borrow_route_health",
}

func TestTicketBorrowSingletonMigrationRestoresMissingRowsWithMigrationHistory(t *testing.T) {
	db := borrowSingletonMigrationDB(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `INSERT INTO accounts (id,name,platform,type,execution_mode)
VALUES (1,'singleton-native','openai','oauth','normal'),
       (2,'singleton-docker','openai','oauth','docker_kernel')`)
	require.NoError(t, err)
	history := borrowSingletonMigrationHistory(t, db)
	var originalMigrationRecorded bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM schema_migrations WHERE filename='269_ticket_borrowing.sql')`).Scan(&originalMigrationRecorded))
	require.True(t, originalMigrationRecorded)

	repo := NewTicketBorrowRepository(db, nil)
	borrowing := service.NewTicketBorrowService(repo, nil, nil)
	for _, id := range []int64{1, 2} {
		_, err := repo.Target(ctx, id)
		require.ErrorIs(t, err, sql.ErrNoRows)
		err = borrowing.Check(ctx, id, "gpt-5.6-terra", "http")
		var admission *service.OpenAITurnAdmissionError
		require.ErrorAs(t, err, &admission)
		require.Equal(t, "latest_state_unavailable", admission.Reason)
		require.ErrorIs(t, err, sql.ErrNoRows, "missing control state must remain fail-closed before repair")
	}

	// The old migrations remain recorded and cannot reseed their tables. Only
	// the new repair migration runs, as it does during an ordinary upgrade.
	require.NoError(t, ApplyMigrations(ctx, db))
	require.Equal(t, history, borrowSingletonMigrationHistory(t, db))
	var repairRecorded bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM schema_migrations WHERE filename=$1)`, borrowSingletonRepairMigration).Scan(&repairRecorded))
	require.True(t, repairRecorded)
	for _, table := range borrowSingletonTables {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+pq.QuoteIdentifier(table)+" WHERE id=1").Scan(&count))
		require.Equal(t, 1, count, table)
	}
	var running bool
	require.NoError(t, db.QueryRowContext(ctx, "SELECT running FROM ticket_borrow_settings WHERE id=1").Scan(&running))
	require.False(t, running, "recovering control state must not start background borrowing")
	selection, err := repo.Selection(ctx)
	require.NoError(t, err)
	require.True(t, selection.UseProxyPool)
	require.False(t, selection.RotateIdentity)

	for _, id := range []int64{1, 2} {
		target, err := repo.Target(ctx, id)
		require.NoError(t, err)
		require.Equal(t, id, target.AccountID)
		require.False(t, target.Enabled, "ordinary accounts must remain outside borrowing")
		require.NoError(t, borrowing.Check(ctx, id, "gpt-5.6-terra", "http"))
	}
	var targets int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM ticket_borrow_targets").Scan(&targets))
	require.Zero(t, targets, "repair must not enable or create borrowing targets")

	before := borrowSingletonRows(t, db)
	require.NoError(t, ApplyMigrations(ctx, db))
	require.Equal(t, before, borrowSingletonRows(t, db))
}

func TestTicketBorrowSingletonMigrationPreservesExistingRowsAndLeases(t *testing.T) {
	db := borrowSingletonMigrationDB(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `
INSERT INTO ticket_borrow_settings (id,running,revision,probe_concurrency,route_pool_size)
VALUES (1,true,42,7,37);
INSERT INTO ticket_borrow_selection (id,revision,group_ids,account_ids,default_model,use_proxy_pool,rotate_identity)
VALUES (1,43,'[3]','[2]','custom-model',false,true);
INSERT INTO ticket_borrow_source_arbiter (id,cursor) VALUES (1,17);
INSERT INTO ticket_borrow_candidate_pool_control (id,source_cursor,consumer_cursor,capacity,revision)
VALUES (1,18,19,23,44);
INSERT INTO ticket_borrow_route_health (id,token,lease_until,last_started_at,last_target_id)
VALUES (1,'existing-health-owner',clock_timestamp()+interval '1 hour',clock_timestamp(),2);`)
	require.NoError(t, err)
	before := borrowSingletonRows(t, db)
	history := borrowSingletonMigrationHistory(t, db)
	require.NoError(t, ApplyMigrations(ctx, db))
	require.Equal(t, before, borrowSingletonRows(t, db))
	require.Equal(t, history, borrowSingletonMigrationHistory(t, db))

	// Replay the SQL itself too: the migration runner would skip an already
	// recorded file, which alone would not exercise ON CONFLICT preservation.
	raw, err := migrations.FS.ReadFile(borrowSingletonRepairMigration)
	require.NoError(t, err)
	for range 2 {
		_, err = db.ExecContext(ctx, string(raw))
		require.NoError(t, err)
		require.Equal(t, before, borrowSingletonRows(t, db))
	}
}

// Clone the real migrated table definitions and history into a private schema.
// No test changes shared settings or deletes another test's migration records.
func borrowSingletonMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := t.Context()
	schema := "borrow_singletons_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err := integrationDB.ExecContext(ctx, "CREATE SCHEMA "+pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
		require.NoError(t, err)
	})
	u, err := url.Parse(integrationPostgresDSN)
	require.NoError(t, err)
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	tables := append([]string{
		"schema_migrations", "atlas_schema_revisions", "accounts",
		"ticket_borrow_targets", "ticket_borrow_requests", "ticket_borrow_routes",
		"ticket_borrow_candidate_pool", "ticket_borrow_source_attempts",
	}, borrowSingletonTables...)
	for _, table := range tables {
		quoted := pq.QuoteIdentifier(table)
		_, err = db.ExecContext(ctx, "CREATE TABLE "+quoted+" (LIKE public."+quoted+" INCLUDING ALL)")
		require.NoError(t, err, table)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO schema_migrations
SELECT * FROM public.schema_migrations WHERE filename<>$1`, borrowSingletonRepairMigration)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO atlas_schema_revisions SELECT * FROM public.atlas_schema_revisions")
	require.NoError(t, err)
	return db
}

func borrowSingletonMigrationHistory(t *testing.T, db *sql.DB) string {
	t.Helper()
	var history string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT jsonb_agg(to_jsonb(m) ORDER BY filename)::text
FROM schema_migrations m WHERE filename<>$1`, borrowSingletonRepairMigration).Scan(&history))
	return history
}

func borrowSingletonRows(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows := make(map[string]string, len(borrowSingletonTables))
	for _, table := range borrowSingletonTables {
		var row string
		require.NoError(t, db.QueryRowContext(t.Context(), "SELECT to_jsonb(t)::text FROM "+pq.QuoteIdentifier(table)+" t WHERE id=1").Scan(&row))
		rows[table] = row
	}
	return rows
}
