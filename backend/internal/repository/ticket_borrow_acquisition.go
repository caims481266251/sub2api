package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type borrowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// The target row serializes acquisition reservations and publication. Identity
// is the same snapshot used by normal route admission, not a proxy-pool version.
// Automatic renewal compares against the same persisted cutoff as business admission.
func checkBorrowAcquisition(ctx context.Context, db borrowQueryer, a service.BorrowAcquisition, waits bool) error {
	var running, active, ready, upstream bool
	var budgetUntil sql.NullTime
	var now time.Time
	err := db.QueryRowContext(ctx, `SELECT s.running,t.enabled AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())

 AND ($2=0 OR t.revision=$2) AND ($3='' OR (t.lease_token=$3 AND t.lease_until>clock_timestamp()
 AND t.lease_settings_revision=s.revision AND t.lease_policy_revision=t.probe_policy_revision AND t.lease_acquisition_revision=t.acquisition_revision
 AND ($4='' OR t.lease_identity='' OR t.lease_identity=$4) AND t.lease_account_signature=`+borrowAccountSignature+`)),
 NOT COALESCE((s.replenishment->>'enabled')::boolean,false) AND EXISTS(SELECT 1 FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.target_id=t.account_id AND r.target_revision=t.revision AND r.revoked_at IS NULL AND r.inventory_eligible AND NOT r.validation_only
 AND ($4='' OR r.identity_hash=$4) AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp()+`+borrowRenewalLead+`*INTERVAL '1 second'),
 COALESCE(t.upstream_wait_until>clock_timestamp(),false),t.budget_wait_until,clock_timestamp()
 FROM ticket_borrow_targets t JOIN accounts a ON a.id=t.account_id CROSS JOIN ticket_borrow_settings s
 WHERE t.account_id=$1 AND s.id=1`, a.TargetID, a.Revision, a.Lease, a.Identity).Scan(&running, &active, &ready, &upstream, &budgetUntil, &now)
	if err == sql.ErrNoRows {
		return service.ErrBorrowUnavailable
	}
	if err != nil {
		return err
	}
	if !running {
		return service.ErrBorrowPaused
	}
	if !active {
		return service.ErrBorrowUnavailable
	}
	if ready && a.Lease == "" {
		return service.ErrBorrowRouteReady
	}
	if !running {
		return service.ErrBorrowPaused
	}
	if upstream {
		return service.ErrBorrowRecoveryWaiting
	}
	if waits && budgetUntil.Valid && budgetUntil.Time.After(now) {
		return &service.BorrowBudgetWaitError{Scope: "target", Until: budgetUntil.Time, RetryAfter: budgetUntil.Time.Sub(now)}
	}
	return nil
}

func (r *ticketBorrowRepository) CheckAcquisition(ctx context.Context, a service.BorrowAcquisition) error {
	return checkBorrowAcquisition(ctx, r.db, a, true)
}

// Every actual source attempt, including retries, consumes the global budget.
// A failed reservation does not count as an HTTP request and does not spin.
func (r *ticketBorrowRepository) ReserveSourceAttempt(ctx context.Context, sourceID int64, lease string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a, ok := service.BorrowAcquisitionFromContext(ctx); ok {
		if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, a.TargetID); err != nil {
			return err
		}
		if err = checkBorrowAcquisition(ctx, tx, a, true); err != nil {
			return err
		}
	}
	var running, active bool
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT running,recovery FROM ticket_borrow_settings WHERE id=1 FOR UPDATE`).Scan(&running, &raw); err != nil {
		return err
	}
	if !running {
		return service.ErrBorrowPaused
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_sources WHERE id=$1 AND enabled AND deleted_at IS NULL AND lease_token=$2 AND lease_until>clock_timestamp())`, sourceID, lease).Scan(&active); err != nil {
		return err
	}
	if !active {
		return service.ErrBorrowUnavailable
	}
	var cfg service.BorrowRecoverySettings
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	limit := cfg.SourceRequestsPerMinute
	if limit <= 0 {
		limit = 12
	}
	var count int
	var available sql.NullTime
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT count(*),(array_agg(reserved_at ORDER BY reserved_at DESC))[$1]+INTERVAL '60 seconds',statement_timestamp() FROM ticket_borrow_source_attempts WHERE reserved_at>statement_timestamp()-INTERVAL '60 seconds'`, limit).Scan(&count, &available, &now); err != nil {
		return err
	}
	if count >= limit {
		return &service.BorrowBudgetWaitError{Scope: "source", Until: available.Time, RetryAfter: available.Time.Sub(now)}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_source_attempts(source_id) VALUES($1)`, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) sourceBudgetStatus(ctx context.Context, limit int) (*service.BorrowBudgetStatus, time.Time, error) {
	status := &service.BorrowBudgetStatus{Limit: limit}
	var available sql.NullTime
	var now time.Time
	err := r.db.QueryRowContext(ctx, `SELECT count(*),(array_agg(reserved_at ORDER BY reserved_at DESC))[$1]+INTERVAL '60 seconds',statement_timestamp() FROM ticket_borrow_source_attempts WHERE reserved_at>statement_timestamp()-INTERVAL '60 seconds'`, limit).Scan(&status.Used, &available, &now)
	if available.Valid {
		status.AvailableAt = &available.Time
	}
	return status, now, err
}
