//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowEventRetentionMigrationKeeps24HoursWithoutCountCap(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(t.Context(), `
CREATE TEMP TABLE ticket_borrow_events(id bigserial,created_at timestamptz,reason text);
CREATE TEMP TABLE usage_logs(id integer);
CREATE TEMP TABLE ticket_borrow_requests(id integer);
INSERT INTO usage_logs VALUES(1); INSERT INTO ticket_borrow_requests VALUES(1);
INSERT INTO ticket_borrow_events(created_at,reason)
 SELECT now()-interval '1 hour','recent' FROM generate_series(1,600);
INSERT INTO ticket_borrow_events(created_at,reason) VALUES
 (now()-interval '24 hours','boundary'),
 (now()-interval '24 hours'-interval '1 microsecond','expired'),
 (now()-interval '7 days','expired');`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("279_ticket_borrow_event_retention.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), string(raw))
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM ticket_borrow_events`).Scan(&count))
	require.Equal(t, 601, count, "all recent records must survive, even above 500")
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM ticket_borrow_events WHERE reason='expired'`).Scan(&count))
	require.Zero(t, count)
	for _, table := range []string{"usage_logs", "ticket_borrow_requests"} {
		require.NoError(t, tx.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Equal(t, 1, count)
	}
}

func TestTicketBorrowEventRetentionFiltersBeforeCleanupAndPreservesRecentHistory(t *testing.T) {
	const targetID = -279001
	ctx := t.Context()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM ticket_borrow_events WHERE target_id=$1`, targetID)
	})
	_, err := integrationDB.ExecContext(ctx, `
INSERT INTO ticket_borrow_events(target_id,stage,reason,created_at)
 SELECT $1,'rotation','recent',now()-interval '1 hour' FROM generate_series(1,600);
`, targetID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,stage,reason,created_at) VALUES
 ($1,'source','expired',now()-interval '25 hours'),
 ($1,'business','historical_business',now()-interval '1 hour')`, targetID)
	require.NoError(t, err)
	r := &ticketBorrowRepository{db: integrationDB}
	var before int64
	var total int
	for {
		rows, err := r.Events(ctx, before, targetID)
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			require.Equal(t, "recent", row.Reason)
		}
		total += len(rows)
		before = rows[len(rows)-1].ID
	}
	require.Equal(t, 600, total)
	require.NoError(t, r.CleanupEvents(ctx))
	var kept, expired int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE reason='recent'),count(*) FILTER(WHERE reason='expired') FROM ticket_borrow_events WHERE target_id=$1`, targetID).Scan(&kept, &expired))
	require.Equal(t, 600, kept)
	require.Zero(t, expired)
	require.NoError(t, r.CleanupEvents(ctx), "cleanup is idempotent")
}
