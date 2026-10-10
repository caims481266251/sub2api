//go:build integration

package repository

import (
	"database/sql"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowMigrationPreservesOutstandingLegacyDeadlines(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	// Temporary tables shadow live fixture tables only on this transaction's
	// connection, allowing the actual migration to run against the old schema.
	_, err = tx.ExecContext(t.Context(), `
CREATE TEMP TABLE ticket_borrow_settings(prewarm_seconds integer,recovery jsonb);
INSERT INTO ticket_borrow_settings VALUES(60,'{"probe_requests_per_minute":8}');
CREATE TEMP TABLE ticket_borrow_targets(id integer,cooldown_until timestamptz,cooldown_seconds integer,last_reason text,recovery_level integer,failures_since_cooldown integer,next_attempt_at timestamptz);
INSERT INTO ticket_borrow_targets VALUES
(1,now()+interval '60 seconds',60,'target_probe_passed',1,3,now()),
(2,now()+interval '120 seconds',120,'target_rate_limited',2,3,now()),
(3,now()-interval '1 second',60,'target_credentials_unavailable',1,3,now()),
(4,NULL,0,'target_ticket_changed',0,0,now());
CREATE TEMP TABLE ticket_borrow_sources(next_attempt_at timestamptz,last_reason text);
INSERT INTO ticket_borrow_sources VALUES(now()+interval '90 seconds','source_cookie_acquired');
CREATE TEMP TABLE legacy_source_deadline AS SELECT next_attempt_at FROM ticket_borrow_sources;
CREATE TEMP TABLE legacy_deadlines AS SELECT id,cooldown_until FROM ticket_borrow_targets;
CREATE SCHEMA migration_deadline_fixture;
SET LOCAL search_path TO pg_temp,migration_deadline_fixture;
`)
	require.NoError(t, err)
	script, err := migrations.FS.ReadFile("273_ticket_borrow_expiry_only.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), string(script))
	require.NoError(t, err)
	for _, tc := range []struct {
		id      int
		reason  string
		waiting bool
	}{
		{1, "legacy_wait_unknown", true}, {2, "target_rate_limited", true},
		{3, "", false}, {4, "", false},
	} {
		var deadline sql.NullTime
		var reason string
		var unchanged bool
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT t.upstream_wait_until,t.upstream_wait_reason,COALESCE(t.upstream_wait_until=d.cooldown_until,false) FROM ticket_borrow_targets t JOIN legacy_deadlines d USING(id) WHERE id=$1`, tc.id).Scan(&deadline, &reason, &unchanged))
		require.Equal(t, tc.waiting, deadline.Valid)
		require.Equal(t, tc.reason, reason)
		require.Equal(t, tc.waiting, unchanged)
	}
	var prewarm, budget int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT prewarm_seconds,(recovery->>'probe_requests_per_minute')::integer FROM ticket_borrow_settings`).Scan(&prewarm, &budget))
	require.Zero(t, prewarm)
	require.Equal(t, 8, budget)
	var sourceDeadlineUnchanged bool
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT s.next_attempt_at=d.next_attempt_at FROM ticket_borrow_sources s CROSS JOIN legacy_source_deadline d`).Scan(&sourceDeadlineUnchanged))
	require.True(t, sourceDeadlineUnchanged)
}
