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
	if p, pooled := service.BorrowPoolAcquisitionFromContext(ctx); pooled {
		if source <= 0 || source != p.SourceID {
			return service.ErrBorrowUnavailable
		}
		if err := r.CheckCandidatePoolAcquisition(ctx, p); err != nil {
			return err
		}
		_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_source_intents(id,target_id,batch_id,source_id,category,expires_at,pool_candidate_id)
 SELECT $1,NULL,owner_token,source_id,'search',lease_until,id FROM ticket_borrow_candidate_pool
 WHERE id=$2 AND owner_token=$3 AND state='acquiring' AND lease_until>clock_timestamp() ON CONFLICT(id) DO NOTHING`, id, p.ID, p.Lease)
		return err
	}
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	if !ok {
		return service.ErrBorrowUnavailable
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_source_intents(id,target_id,batch_id,source_id,category,expires_at)
 SELECT $1,account_id,lease_token,NULLIF($2,0),CASE WHEN last_inventory_state='search' THEN 'search' WHEN last_inventory_state='healthy' THEN 'healthy' ELSE 'refill' END,
 lease_until FROM ticket_borrow_targets
 WHERE account_id=$3 AND lease_token=$4 AND lease_until>clock_timestamp()
 ON CONFLICT(id) DO NOTHING`, id, source, a.TargetID, a.Lease)
	return err
}

// Target -> settings -> arbiter -> source. Never acquire the pool/rotation lock
// here. Other waiting targets are read, never locked, so competing callers do
// not create the reverse target/settings lock edge.
func (r *ticketBorrowRepository) GrantSourceAttempt(ctx context.Context, source int64, id string) (bool, error) {
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	p, pooled := service.BorrowPoolAcquisitionFromContext(ctx)
	if (!ok && !pooled) || (pooled && (source <= 0 || source != p.SourceID)) {
		return false, service.ErrBorrowUnavailable
	}
	if pooled {
		a = service.BorrowAcquisition{Lease: p.Lease}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if !pooled {
		if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, a.TargetID); err != nil {
			return false, err
		}
		err = checkBorrowAcquisition(ctx, tx, a, true)
	} else {
		err = checkBorrowPoolAcquisition(ctx, tx, p)
	}
	if err != nil {
		return false, err
	}
	if source == 0 {
		var active, capacity int
		if err = tx.QueryRowContext(ctx, `SELECT (`+borrowBackgroundInflight+`),CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE cfg.probe_concurrency END
 FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings cfg WHERE t.account_id=$1 AND cfg.id=1`, a.TargetID).Scan(&active, &capacity); err != nil {
			return false, err
		}
		if active >= capacity {
			return false, nil
		}
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
	if pooled {
		var inflight int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM ticket_borrow_source_attempts WHERE pool_candidate_id IS NOT NULL AND completed_at IS NULL AND lease_until>clock_timestamp()`).Scan(&inflight); err != nil {
			return false, err
		}
		if inflight >= service.BorrowCandidatePoolSourceConcurrency {
			return false, nil
		}
	}
	var cursor int
	if err = tx.QueryRowContext(ctx, `SELECT cursor FROM ticket_borrow_source_arbiter WHERE id=1 FOR UPDATE`).Scan(&cursor); err != nil {
		return false, err
	}
	// Remove stale intents independently of who reaches the arbiter first.
	if _, err = tx.ExecContext(ctx, `DELETE FROM ticket_borrow_source_intents i WHERE expires_at<=clock_timestamp() OR (pool_candidate_id IS NULL AND NOT EXISTS(
 SELECT 1 FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings cfg WHERE cfg.id=1 AND t.account_id=i.target_id AND t.enabled AND cfg.running
 AND t.lease_token=i.batch_id AND t.lease_until>clock_timestamp() AND t.lease_settings_revision=cfg.revision AND t.lease_policy_revision=t.probe_policy_revision AND t.lease_acquisition_revision=t.acquisition_revision))`); err != nil {
		return false, err
	}
	// Qualification mirrors ordinary source selection, including exclusions.
	eligible := ` FROM ticket_borrow_source_intents i
 JOIN ticket_borrow_targets t ON t.account_id=i.target_id
 LEFT JOIN ticket_borrow_sources s ON s.id=i.source_id
 JOIN accounts a ON a.id=COALESCE(s.account_id,t.account_id)
 JOIN accounts ta ON ta.id=t.account_id
 CROSS JOIN ticket_borrow_selection c
 CROSS JOIN ticket_borrow_settings cfg
 LEFT JOIN ticket_borrow_source_grants g ON g.target_id=i.target_id
 WHERE c.id=1 AND cfg.id=1 AND (
 (i.source_id IS NULL AND t.acquisition_identity='self' AND (` + strings.ReplaceAll(borrowBackgroundInflight, "$1", "t.account_id") + `)<CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE cfg.probe_concurrency END) OR
 (i.source_id IS NOT NULL AND t.acquisition_identity='source' AND s.enabled AND s.deleted_at IS NULL
 AND s.next_attempt_at<=clock_timestamp() AND (s.lease_until IS NULL OR s.lease_until<=clock_timestamp())
 AND ` + borrowNormalScope + ` AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets st WHERE st.account_id=a.id AND st.enabled)))
 AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND t.lease_acquisition_revision=t.acquisition_revision
 AND t.lease_account_signature=` + strings.ReplaceAll(borrowAccountSignature, "a.", "ta.") + `
 AND ta.deleted_at IS NULL AND ta.status='active' AND ta.schedulable
 AND (NOT ta.auto_pause_on_expired OR ta.expires_at IS NULL OR ta.expires_at>clock_timestamp())
 AND COALESCE(t.upstream_wait_until<=clock_timestamp(),true) AND COALESCE(t.budget_wait_until<=clock_timestamp(),true)
 AND EXISTS(SELECT 1 FROM proxy_pool_entries WHERE enabled AND retired_at IS NULL)`
	poolEligible := ` FROM ticket_borrow_source_intents i
 JOIN ticket_borrow_candidate_pool cp ON cp.id=i.pool_candidate_id
 JOIN ticket_borrow_sources s ON s.id=i.source_id JOIN accounts a ON a.id=s.account_id
 CROSS JOIN ticket_borrow_selection c CROSS JOIN ticket_borrow_settings cfg
 WHERE c.id=1 AND cfg.id=1 AND cfg.running AND cp.settings_revision=cfg.revision
 AND cp.owner_token=i.batch_id AND cp.state='acquiring' AND cp.lease_until>clock_timestamp()
 AND cp.source_id=i.source_id AND i.expires_at>clock_timestamp()
 AND s.enabled AND s.deleted_at IS NULL AND s.next_attempt_at<=clock_timestamp() AND (s.lease_until IS NULL OR s.lease_until<=clock_timestamp())
 AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true)
 AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true) AND ` + borrowNormalScope + `
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets st WHERE st.account_id=a.id AND st.enabled)
 AND EXISTS(SELECT 1 FROM proxy_pool_entries WHERE enabled AND retired_at IS NULL)
 AND ` + borrowPoolEnabledConsumers + `
 AND (SELECT count(*) FROM ticket_borrow_source_attempts q WHERE q.pool_candidate_id IS NOT NULL AND q.completed_at IS NULL AND q.lease_until>clock_timestamp())<4`
	eligible = ` FROM (SELECT i.id,i.target_id,i.batch_id,COALESCE(i.source_id,0) source_id,i.category,i.queued_at,g.granted_at` + eligible + `
 UNION ALL SELECT i.id,0::bigint,i.batch_id,i.source_id,i.category,i.queued_at,
 (SELECT max(q.reserved_at) FROM ticket_borrow_source_attempts q WHERE q.pool_candidate_id IS NOT NULL)` + poolEligible + `) i`

	var own bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+eligible+` WHERE i.id=$1 AND i.target_id=$2 AND i.batch_id=$3 AND COALESCE(i.source_id,0)=$4)`, id, a.TargetID, a.Lease, source).Scan(&own); err != nil {
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
		err = tx.QueryRowContext(ctx, `SELECT i.id`+eligible+` WHERE i.category=$1 ORDER BY i.granted_at NULLS FIRST,i.queued_at,i.target_id,i.id LIMIT 1`, weights[idx]).Scan(&winner)
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
	if source > 0 {
		res, e := tx.ExecContext(ctx, `UPDATE ticket_borrow_sources SET lease_token=$2,lease_until=clock_timestamp()+INTERVAL '180 seconds',cursor=cursor+1
 WHERE id=$1 AND (lease_until IS NULL OR lease_until<=clock_timestamp())`, source, id)
		if e != nil {
			return false, e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return false, e
		}
		if n != 1 {
			return false, nil
		}
	}
	var poolID any
	if pooled {
		poolID = p.ID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_source_attempts(source_id,intent_id,target_id,acquisition_account_id,lease_until,pool_candidate_id)
 VALUES(NULLIF($1,0),$2,NULLIF($3,0),CASE WHEN $1=0 THEN $3 ELSE (SELECT account_id FROM ticket_borrow_sources WHERE id=$1) END,clock_timestamp()+INTERVAL '180 seconds',$4)`, source, id, a.TargetID, poolID); err != nil {
		return false, err
	}

	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_source_arbiter SET cursor=$1 WHERE id=1`, next); err != nil {
		return false, err
	}
	if !pooled {
		if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_source_grants(target_id) VALUES($1) ON CONFLICT(target_id) DO UPDATE SET granted_at=clock_timestamp()`, a.TargetID); err != nil {
			return false, err
		}
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
	_, err := r.db.ExecContext(ctx, `WITH cancelled AS (
 DELETE FROM ticket_borrow_source_attempts WHERE intent_id=$1 AND sent_at IS NULL)
 UPDATE ticket_borrow_source_attempts SET completed_at=COALESCE(completed_at,clock_timestamp()) WHERE intent_id=$1 AND sent_at IS NOT NULL`, id)
	return err
}

func (r *ticketBorrowRepository) SourceHoldUntil(context.Context, int64) (*time.Time, error) {
	return nil, nil
}
