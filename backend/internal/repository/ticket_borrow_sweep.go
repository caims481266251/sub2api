package repository

import (
	"context"
	"database/sql"
)

// Reap before taking the pool lock: request completion locks targets first.
// This runs even when new acquisition is paused, so recovery does not depend
// on another BeginTargetRequest. Live HTTP leases are never reclaimed.
func (r *ticketBorrowRepository) sweepBorrowRequests(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT t.account_id FROM ticket_borrow_targets t
 WHERE EXISTS(SELECT 1 FROM ticket_borrow_requests q WHERE q.target_id=t.account_id AND q.kind='probe'
 AND q.finished_at IS NULL AND q.lease_until<=clock_timestamp())
 ORDER BY t.account_id LIMIT 100 FOR UPDATE OF t SKIP LOCKED`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = reapBorrowRequests(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Frozen material is retained while a candidate, route, or sent request owns
// it. Expired execution records cannot hold it forever after a crashed worker.
func releaseAbandonedBorrowEnvironments(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_environments e SET released_at=clock_timestamp()
 WHERE e.released_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool p WHERE p.id=e.pool_candidate_id AND p.state<>'finished')
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=e.target_id AND t.lease_token=e.batch_id AND t.lease_until>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_source_attempts q WHERE q.pool_candidate_id=e.id AND q.completed_at IS NULL AND q.lease_until>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_requests q WHERE q.details->>'candidate_id'=e.id AND q.finished_at IS NULL AND q.lease_until>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_routes r WHERE r.candidate_id=e.id AND
 (LEAST(r.admission_until,r.expires_at)>clock_timestamp() OR EXISTS(SELECT 1 FROM ticket_borrow_route_leases l WHERE l.route_id=r.id AND l.lease_until>clock_timestamp())))`)
	return err
}
