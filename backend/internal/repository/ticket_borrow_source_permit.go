package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *ticketBorrowRepository) QueueSourceAttempt(ctx context.Context, source int64, id string) error {
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	if !ok {
		return service.ErrBorrowUnavailable
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_source_intents(id,target_id,batch_id,source_id,category,expires_at)
 SELECT $1,account_id,lease_token,$2,CASE WHEN last_inventory_state='search' THEN 'search' WHEN last_inventory_state='healthy' THEN 'healthy' ELSE 'refill' END,
 LEAST(lease_until,last_batch_started_at+INTERVAL '150 seconds') FROM ticket_borrow_targets
 WHERE account_id=$3 AND lease_token=$4 AND lease_until>clock_timestamp()
 ON CONFLICT(id) DO NOTHING`, id, source, a.TargetID, a.Lease)
	return err
}

// Target -> settings -> arbiter -> source. Never acquire the pool/rotation lock
// here. Other waiting targets are read, never locked, so competing callers do
// not create the reverse target/settings lock edge.
func (r *ticketBorrowRepository) GrantSourceAttempt(ctx context.Context, source int64, id string) (bool, error) {
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	if !ok {
		return false, service.ErrBorrowUnavailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, a.TargetID); err != nil {
		return false, err
	}
	if err = checkBorrowAcquisition(ctx, tx, a, true); err != nil {
		return false, err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT recovery FROM ticket_borrow_settings WHERE id=1 FOR UPDATE`).Scan(&raw); err != nil {
		return false, err
	}
	var cfg service.BorrowRecoverySettings
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return false, err
	}
	limit := max(1, cfg.SourceRequestsPerMinute)
	if cfg.SourceRequestsPerMinute <= 0 {
		limit = 12
	}
	var count int
	var available sql.NullTime
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT count(*),(array_agg(reserved_at ORDER BY reserved_at DESC))[$1]+INTERVAL '60 seconds',clock_timestamp()
 FROM ticket_borrow_source_attempts WHERE reserved_at>clock_timestamp()-INTERVAL '60 seconds'`, limit).Scan(&count, &available, &now); err != nil {
		return false, err
	}
	if count >= limit {
		return false, &service.BorrowBudgetWaitError{Scope: "source", Until: available.Time, RetryAfter: available.Time.Sub(now)}
	}
	var cursor int
	if err = tx.QueryRowContext(ctx, `SELECT cursor FROM ticket_borrow_source_arbiter WHERE id=1 FOR UPDATE`).Scan(&cursor); err != nil {
		return false, err
	}
	// Remove stale intents independently of who reaches the arbiter first.
	if _, err = tx.ExecContext(ctx, `DELETE FROM ticket_borrow_source_intents i WHERE expires_at<=clock_timestamp() OR NOT EXISTS(
 SELECT 1 FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings cfg WHERE cfg.id=1 AND t.account_id=i.target_id AND t.enabled AND cfg.running
 AND t.lease_token=i.batch_id AND t.lease_until>clock_timestamp() AND t.lease_settings_revision=cfg.revision AND t.lease_policy_revision=t.probe_policy_revision)`); err != nil {
		return false, err
	}
	// Qualification mirrors ordinary source selection, including exclusions.
	eligible := ` FROM ticket_borrow_source_intents i JOIN ticket_borrow_sources s ON s.id=i.source_id
 JOIN accounts a ON a.id=s.account_id CROSS JOIN ticket_borrow_selection c
 JOIN ticket_borrow_targets t ON t.account_id=i.target_id
 JOIN accounts ta ON ta.id=t.account_id
 LEFT JOIN ticket_borrow_source_grants g ON g.target_id=i.target_id
 WHERE c.id=1 AND c.use_proxy_pool AND s.enabled AND s.deleted_at IS NULL AND s.next_attempt_at<=clock_timestamp()
 AND (s.lease_until IS NULL OR s.lease_until<=clock_timestamp()) AND ` + borrowNormalScope + `
 AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND t.lease_account_signature=` + strings.ReplaceAll(borrowAccountSignature, "a.", "ta.") + `
 AND ta.deleted_at IS NULL AND ta.status='active' AND ta.schedulable
 AND (NOT ta.auto_pause_on_expired OR ta.expires_at IS NULL OR ta.expires_at>clock_timestamp())
 AND COALESCE(t.upstream_wait_until<=clock_timestamp(),true) AND COALESCE(t.budget_wait_until<=clock_timestamp(),true)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets st WHERE st.account_id=a.id AND st.enabled)
 AND EXISTS(SELECT 1 FROM ticket_borrow_proxy_bindings b JOIN proxy_pool_entries p ON p.id=b.entry_id WHERE b.account_id=a.id AND b.role='source' AND p.enabled AND p.retired_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_rotation_accounts rot WHERE rot.account_id=a.id AND
 ((rot.phase='draining' AND rot.active_job IS NOT NULL) OR (rot.automatic AND rot.next_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM ticket_borrow_active_replenishment w WHERE w.source_account_id=a.id))))`
	var own bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+eligible+` AND i.id=$1)`, id).Scan(&own); err != nil {
		return false, err
	}
	if !own {
		return false, service.ErrBorrowUnavailable
	}
	weights := []string{"search", "refill", "search", "healthy", "search", "refill", "search"}
	winner := ""
	next := cursor
	for step := 0; step < len(weights); step++ {
		idx := (cursor + step) % len(weights)
		err = tx.QueryRowContext(ctx, `SELECT i.id`+eligible+` AND i.category=$1 ORDER BY g.granted_at NULLS FIRST,i.queued_at,i.target_id,i.id LIMIT 1`, weights[idx]).Scan(&winner)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return false, err
		}
		next = (idx + 1) % len(weights)
		break
	}
	if winner != id {
		return false, tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_sources SET lease_token=$2,lease_until=clock_timestamp()+INTERVAL '180 seconds',cursor=cursor+1
 WHERE id=$1 AND (lease_until IS NULL OR lease_until<=clock_timestamp())`, source, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_source_attempts(source_id,intent_id,target_id) VALUES($1,$2,$3)`, source, id, a.TargetID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_source_arbiter SET cursor=$1 WHERE id=1`, next); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_source_grants(target_id) VALUES($1) ON CONFLICT(target_id) DO UPDATE SET granted_at=clock_timestamp()`, a.TargetID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ticket_borrow_source_intents WHERE id=$1`, id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
func (r *ticketBorrowRepository) CancelSourceAttempt(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM ticket_borrow_source_intents WHERE id=$1`, id)
	return err
}
func (r *ticketBorrowRepository) SourceAttemptSent(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_source_attempts SET sent_at=COALESCE(sent_at,clock_timestamp()) WHERE intent_id=$1`, id)
	return err
}
func (r *ticketBorrowRepository) SourceAttemptCompleted(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_source_attempts SET completed_at=COALESCE(completed_at,clock_timestamp()) WHERE intent_id=$1`, id)
	return err
}

func (r *ticketBorrowRepository) SourceHoldUntil(ctx context.Context, source int64) (*time.Time, error) {
	var until *time.Time
	err := r.db.QueryRowContext(ctx, `SELECT min(until_at) FROM ticket_borrow_active_replenishment WHERE source_id=$1`, source).Scan(&until)
	return until, err
}
