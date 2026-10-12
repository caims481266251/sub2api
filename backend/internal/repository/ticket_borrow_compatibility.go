package repository

import (
	"context"
	"database/sql"
	"time"
)

// CheckBorrowCompatibility reports whether a narrowly scoped source/target
// environment circuit is cooling down. Missing rows are healthy and remain
// cheap to check.
func (r *ticketBorrowRepository) CheckBorrowCompatibility(ctx context.Context, targetID int64, identity, key string) (time.Time, bool, error) {
	var next time.Time
	var failures int
	err := r.db.QueryRowContext(ctx, `SELECT next_attempt_at,consecutive_failures
 FROM ticket_borrow_compatibility WHERE target_id=$1 AND identity_hash=$2 AND compatibility_hash=$3`, targetID, identity, key).Scan(&next, &failures)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	now := time.Now()
	return next, failures >= 3 && next.After(now), nil
}

// RecordBorrowCompatibility updates a short-lived circuit breaker. Success
// resets the combination immediately. Failures only begin delaying after three
// consecutive results and then use a bounded exponential backoff.
func (r *ticketBorrowRepository) RecordBorrowCompatibility(ctx context.Context, targetID int64, identity, key, reason string, success bool) (time.Time, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, err
	}
	var failures int
	err = tx.QueryRowContext(ctx, `SELECT consecutive_failures FROM ticket_borrow_compatibility
 WHERE target_id=$1 AND identity_hash=$2 AND compatibility_hash=$3 FOR UPDATE`, targetID, identity, key).Scan(&failures)
	if err == sql.ErrNoRows {
		failures = 0
		err = nil
	}
	if err != nil {
		return time.Time{}, err
	}
	next := now
	if success {
		failures = 0
	} else {
		failures++
		if failures >= 3 {
			step := failures - 3
			if step > 3 {
				step = 3
			}
			delay := 30 * (1 << step)
			if delay > 300 {
				delay = 300
			}
			next = now.Add(time.Duration(delay) * time.Second)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_compatibility
 (target_id,identity_hash,compatibility_hash,consecutive_failures,last_reason,next_attempt_at,last_success_at,last_failure_at,updated_at)
 VALUES($1,$2,$3,$4,$5,$6,CASE WHEN $7 THEN $8 ELSE NULL END,CASE WHEN $7 THEN NULL ELSE $8 END,$8)
 ON CONFLICT(target_id,identity_hash,compatibility_hash) DO UPDATE SET
 consecutive_failures=EXCLUDED.consecutive_failures,last_reason=EXCLUDED.last_reason,
 next_attempt_at=EXCLUDED.next_attempt_at,last_success_at=EXCLUDED.last_success_at,
 last_failure_at=EXCLUDED.last_failure_at,updated_at=EXCLUDED.updated_at`,
		targetID, identity, key, failures, reason, next, success, now)
	if err != nil {
		return time.Time{}, err
	}
	if err = tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return next, nil
}

var _ interface {
	CheckBorrowCompatibility(context.Context, int64, string, string) (time.Time, bool, error)
	RecordBorrowCompatibility(context.Context, int64, string, string, string, bool) (time.Time, error)
} = (*ticketBorrowRepository)(nil)
