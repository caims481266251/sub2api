package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Pool control only serializes pool accounting. Never acquire a target,
// settings, source or allocator lock after holding this lock. Claim takes the
// existing target lock first; allocator/source code only reads pool fences.
func lockBorrowCandidatePool(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT id FROM ticket_borrow_candidate_pool_control WHERE id=1 FOR UPDATE`)
	return err
}

const borrowPoolDemandFrom = ` FROM ticket_borrow_candidate_pool_demand d
 JOIN ticket_borrow_targets t ON t.account_id=d.target_id
 JOIN accounts a ON a.id=t.account_id CROSS JOIN ticket_borrow_settings cfg
 WHERE cfg.id=1 AND cfg.running AND d.until_at>clock_timestamp()
 AND t.enabled AND t.acquisition_identity='source' AND t.lease_token=d.batch_id AND t.lease_until>clock_timestamp()
 AND t.lease_settings_revision=cfg.revision AND t.lease_policy_revision=t.probe_policy_revision
 AND t.lease_acquisition_revision=t.acquisition_revision AND t.lease_account_signature=` + borrowAccountSignature + `
 AND t.lease_identity=d.identity_hash
 AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true)
 AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND COALESCE(t.upstream_wait_until<=clock_timestamp(),true) AND COALESCE(t.budget_wait_until<=clock_timestamp(),true)
 AND (SELECT count(*) FROM ticket_borrow_requests q WHERE q.target_id=t.account_id AND q.kind='probe'
 AND q.reserved_at>clock_timestamp()-INTERVAL '60 seconds')+2<=GREATEST(1,COALESCE((cfg.recovery->>'probe_requests_per_minute')::int,12))`

// Shared by producer admission and the management snapshot. A claimed candidate
// and its HTTP attempt consume the same slot, not two different slots.
const borrowPoolOpenDemandSlots = `GREATEST(0,
 LEAST(d.slots,(GREATEST(1,COALESCE((cfg.recovery->>'probe_requests_per_minute')::int,12))-
 (SELECT count(*) FROM ticket_borrow_requests q WHERE q.target_id=t.account_id AND q.kind='probe' AND q.reserved_at>clock_timestamp()-INTERVAL '60 seconds'))/2)
 -GREATEST((SELECT count(*) FROM ticket_borrow_candidate_pool p WHERE p.target_id=t.account_id AND p.state='claimed'),
 (SELECT count(*) FROM ticket_borrow_requests q WHERE q.target_id=t.account_id AND q.kind='probe' AND q.finished_at IS NULL AND q.lease_until>clock_timestamp())))`

// A small standby wave can be fetched while verification slots are occupied.
// Reserve HTTP headroom for claimed work before forecasting future consumption.
const borrowPoolBufferedDemandSlots = `GREATEST(0,LEAST(d.slots,
 (GREATEST(1,COALESCE((cfg.recovery->>'probe_requests_per_minute')::int,12))-
 (SELECT count(*) FROM ticket_borrow_requests q WHERE q.target_id=t.account_id AND q.kind='probe' AND q.reserved_at>clock_timestamp()-INTERVAL '60 seconds'))/2
 -(SELECT count(*) FROM ticket_borrow_candidate_pool p WHERE p.target_id=t.account_id AND p.state='claimed')))`

func (r *ticketBorrowRepository) RegisterCandidatePoolDemand(ctx context.Context, a service.BorrowAcquisition, slots int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, a.TargetID); err != nil {
		return err
	}
	if err = checkBorrowAcquisition(ctx, tx, a, true); err != nil {
		return err
	}
	var window, cont int
	if err = tx.QueryRowContext(ctx, `SELECT admission_window_seconds,continue_timeout_seconds FROM ticket_borrow_settings WHERE id=1`).Scan(&window, &cont); err != nil {
		return err
	}
	if time.Duration(window)*time.Second <= service.BorrowPoolVerificationBudget(cont, "mint") {
		return service.ErrBorrowCandidatePoolWindow
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_candidate_pool_demand(target_id,batch_id,identity_hash,slots,until_at)
	SELECT t.account_id,$2,$3,LEAST($4,CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE cfg.probe_concurrency END),clock_timestamp()+INTERVAL '3 seconds'
	FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings cfg WHERE cfg.id=1 AND t.account_id=$1 AND t.acquisition_identity='source'
 ON CONFLICT(target_id) DO UPDATE SET batch_id=EXCLUDED.batch_id,identity_hash=EXCLUDED.identity_hash,slots=EXCLUDED.slots,until_at=EXCLUDED.until_at,
	requested_at=CASE WHEN ticket_borrow_candidate_pool_demand.batch_id=EXCLUDED.batch_id THEN ticket_borrow_candidate_pool_demand.requested_at ELSE clock_timestamp() END`, a.TargetID, a.Lease, a.Identity, max(1, slots))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrBorrowUnavailable
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) RemoveCandidatePoolDemand(ctx context.Context, a service.BorrowAcquisition) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_candidate_pool_demand SET until_at=clock_timestamp() WHERE target_id=$1 AND batch_id=$2`, a.TargetID, a.Lease)
	return err
}

func (r *ticketBorrowRepository) ReserveCandidatePoolFetch(ctx context.Context, id, owner string) (*service.BorrowPoolAcquisition, error) {
	if id == "" || owner == "" {
		return nil, service.ErrBorrowUnavailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockBorrowCandidatePool(ctx, tx); err != nil {
		return nil, err
	}
	var slots, buffered, acquiring, queued, total, capacity int
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT capacity FROM ticket_borrow_candidate_pool_control WHERE id=1`).Scan(&capacity); err != nil {
		return nil, err
	}
	// Open slots remain the consumer concurrency limit; standby only overlaps source work.
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(`+borrowPoolOpenDemandSlots+`),0),COALESCE(sum(`+borrowPoolBufferedDemandSlots+`),0)`+borrowPoolDemandFrom).Scan(&slots, &buffered); err != nil {
		return nil, err
	}
	slots = max(slots, min(service.BorrowCandidatePoolStandby, buffered))
	if slots == 0 {
		return nil, nil
	}
	var sourceCapacity bool
	if err = tx.QueryRowContext(ctx, `SELECT running AND
 (SELECT count(*) FROM ticket_borrow_source_attempts WHERE reserved_at>clock_timestamp()-INTERVAL '60 seconds')
 <GREATEST(1,COALESCE((recovery->>'source_requests_per_minute')::int,12))
 AND (SELECT count(*) FROM ticket_borrow_source_attempts WHERE pool_candidate_id IS NOT NULL AND completed_at IS NULL AND lease_until>clock_timestamp())<$1
 AND EXISTS(SELECT 1 FROM proxy_pool_entries WHERE enabled AND retired_at IS NULL)
 FROM ticket_borrow_settings WHERE id=1`, service.BorrowCandidatePoolSourceConcurrency).Scan(&sourceCapacity); err != nil {
		return nil, err
	}
	if !sourceCapacity {
		return nil, nil
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE state='acquiring'),count(*) FILTER(WHERE state='queued'),count(*)
 FROM ticket_borrow_candidate_pool WHERE state<>'finished'`).Scan(&acquiring, &queued, &total); err != nil {
		return nil, err
	}
	// Standby is included in the hard cap, with the existing short queue lifetime.
	if acquiring >= service.BorrowCandidatePoolSourceConcurrency || acquiring+queued >= min(slots, capacity) || total >= capacity {
		return nil, nil
	}
	var source int64
	err = tx.QueryRowContext(ctx, `SELECT src.id FROM ticket_borrow_sources src JOIN accounts a ON a.id=src.account_id
 CROSS JOIN ticket_borrow_selection c CROSS JOIN ticket_borrow_candidate_pool_control ctl
 WHERE c.id=1 AND ctl.id=1 AND src.enabled AND src.deleted_at IS NULL AND src.next_attempt_at<=clock_timestamp()
 AND (src.lease_until IS NULL OR src.lease_until<=clock_timestamp()) AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true)
 AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true) AND `+borrowNormalScope+`
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets st WHERE st.account_id=a.id AND st.enabled)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool p WHERE p.source_id=src.id AND p.state='acquiring')
 ORDER BY (src.id<=ctl.source_cursor),src.id LIMIT 1`).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM ticket_borrow_settings WHERE id=1 AND running`).Scan(&revision); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_candidate_pool(id,owner_token,settings_revision,source_id,lease_until)
 VALUES($1,$2,$3,$4,clock_timestamp()+INTERVAL '60 seconds')`, id, owner, revision, source); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_pool_control SET source_cursor=$1 WHERE id=1`, source); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &service.BorrowPoolAcquisition{ID: id, Lease: owner, SettingsRevision: revision, SourceID: source}, nil
}

func checkBorrowPoolAcquisition(ctx context.Context, db borrowQueryer, a service.BorrowPoolAcquisition) error {
	var eligible bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool p CROSS JOIN ticket_borrow_settings cfg
 WHERE cfg.id=1 AND cfg.running AND cfg.revision=p.settings_revision AND p.settings_revision=$3
	AND p.id=$1 AND p.owner_token=$2 AND p.source_id=$4 AND p.state='acquiring' AND p.lease_until>clock_timestamp()
 AND EXISTS(SELECT 1`+borrowPoolDemandFrom+`))`, a.ID, a.Lease, a.SettingsRevision, a.SourceID).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return service.ErrBorrowUnavailable
	}
	return nil
}
func (r *ticketBorrowRepository) CheckCandidatePoolAcquisition(ctx context.Context, a service.BorrowPoolAcquisition) error {
	return checkBorrowPoolAcquisition(ctx, r.db, a)
}

func (r *ticketBorrowRepository) EnqueueCandidatePool(ctx context.Context, a service.BorrowPoolAcquisition, c *service.BorrowPoolCandidate) error {
	if c == nil || c.ID != a.ID || c.SourceID != a.SourceID || c.Details == nil || c.Environment == nil || c.Environment.ID != c.ID || c.Environment.Fingerprint == nil || c.Cookie.Value == "" || c.Details.CookieHash == "" || c.Details.AcquisitionStartedAt == nil {
		return service.ErrBorrowUnavailable
	}
	secret, err := r.seal(c)
	if err != nil {
		return err
	}
	details, err := json.Marshal(c.Details)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockBorrowCandidatePool(ctx, tx); err != nil {
		return err
	}
	if err = checkBorrowPoolAcquisition(ctx, tx, a); err != nil {
		return err
	}
	var consumers bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+borrowPoolDemandFrom+`)`).Scan(&consumers); err != nil {
		return err
	}
	if !consumers {
		return service.ErrBorrowUnavailable
	}
	var cont int
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT continue_timeout_seconds,clock_timestamp() FROM ticket_borrow_settings WHERE id=1`).Scan(&cont, &now); err != nil {
		return err
	}
	if c.Expires.Sub(now) < service.BorrowPoolVerificationBudget(cont, "mint") {
		return service.ErrBorrowCandidateTimeBudget
	}
	var duplicate bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool WHERE cookie_hash=$1 AND id<>$2)`, c.Details.CookieHash, c.ID).Scan(&duplicate); err != nil {
		return err
	}
	if duplicate {
		return service.ErrBorrowCandidateDuplicate
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_pool SET state='queued',enqueued_at=clock_timestamp(),admission_until=$2,cookie_hash=$3,secret=$4,details=$5,
 lease_until=LEAST($2,clock_timestamp()+$6*INTERVAL '1 millisecond') WHERE id=$1`, c.ID, c.Expires, c.Details.CookieHash, secret, details, service.BorrowCandidatePoolQueueLifetime.Milliseconds()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_events(source_id,stage,reason,details)
 SELECT source_id,'candidate','candidate_pool_enqueued',details FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) ClaimCandidatePool(ctx context.Context, a service.BorrowAcquisition) (*service.BorrowPoolCandidate, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, a.TargetID); err != nil {
		return nil, err
	}
	if err = checkBorrowAcquisition(ctx, tx, a, true); err != nil {
		return nil, err
	}
	if err = lockBorrowCandidatePool(ctx, tx); err != nil {
		return nil, err
	}
	var cursor int64
	if err = tx.QueryRowContext(ctx, `SELECT consumer_cursor FROM ticket_borrow_candidate_pool_control WHERE id=1`).Scan(&cursor); err != nil {
		return nil, err
	}
	available := borrowPoolDemandFrom + ` AND (SELECT count(*) FROM ticket_borrow_candidate_pool p WHERE p.target_id=t.account_id AND p.state='claimed')<d.slots
 AND (SELECT count(*) FROM ticket_borrow_requests q WHERE q.target_id=t.account_id AND q.kind='probe' AND q.finished_at IS NULL AND q.lease_until>clock_timestamp())<d.slots
 AND (SELECT count(*) FROM ticket_borrow_requests q WHERE q.target_id=t.account_id AND q.kind='probe' AND q.reserved_at>clock_timestamp()-INTERVAL '60 seconds')
 +2*(1+(SELECT count(*) FROM ticket_borrow_candidate_pool p WHERE p.target_id=t.account_id AND p.state='claimed'))
 <=GREATEST(1,COALESCE((cfg.recovery->>'probe_requests_per_minute')::int,12))`
	var winner int64
	// Two search turns followed by one oldest waiter of any category. A target
	// needing renewal cannot starve behind another target's endless failures.
	order := ` ORDER BY d.granted_at NULLS FIRST,d.requested_at,d.target_id LIMIT 1`
	if cursor%3 != 2 {
		err = tx.QueryRowContext(ctx, `SELECT d.target_id`+available+` AND t.last_inventory_state='search'`+order).Scan(&winner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if winner == 0 {
		err = tx.QueryRowContext(ctx, `SELECT d.target_id`+available+order).Scan(&winner)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if winner != a.TargetID {
		return nil, nil
	}
	var cont int
	if err = tx.QueryRowContext(ctx, `SELECT continue_timeout_seconds FROM ticket_borrow_settings WHERE id=1`).Scan(&cont); err != nil {
		return nil, err
	}
	scope, err := borrowGatewayScopeForLease(ctx, tx, a)
	if err != nil {
		return nil, err
	}
	var demoted bool
	var freshnessRank int
	var id, secret string
	var queued, now, deadline time.Time
	// The pool control row already serializes all claims/updates. The freshness
	// rank records whether feedback actually displaced the default choice.
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.secret,p.enqueued_at,clock_timestamp(),LEAST(p.admission_until,e.admission_until,
 e.started_at+cfg.admission_window_seconds*INTERVAL '1 second',COALESCE(cd.expires_at,p.admission_until)),COALESCE(f.deprioritized_until>clock_timestamp(),false),
 row_number() OVER (ORDER BY LEAST(p.admission_until,e.admission_until,e.started_at+cfg.admission_window_seconds*INTERVAL '1 second',COALESCE(cd.expires_at,p.admission_until)) DESC,p.enqueued_at DESC,p.id)
 FROM ticket_borrow_candidate_pool p JOIN ticket_borrow_candidate_environments e ON e.id=p.id
 CROSS JOIN ticket_borrow_settings cfg LEFT JOIN ticket_borrow_cookie_deadlines cd ON cd.cookie_hash=p.cookie_hash
 LEFT JOIN ticket_borrow_gateway_feedback f ON f.target_id=$2 AND f.scope=$3 AND f.gateway=p.details->>'gateway' AND $4
 WHERE cfg.id=1 AND p.state='queued' AND p.settings_revision=cfg.revision AND p.lease_until>clock_timestamp()
 AND e.released_at IS NULL AND e.lease_until>clock_timestamp()
 AND LEAST(p.admission_until,e.admission_until,e.started_at+cfg.admission_window_seconds*INTERVAL '1 second',COALESCE(cd.expires_at,p.admission_until))>=clock_timestamp()+$1*INTERVAL '1 millisecond'
 ORDER BY COALESCE(f.deprioritized_until>clock_timestamp(),false),LEAST(p.admission_until,e.admission_until,e.started_at+cfg.admission_window_seconds*INTERVAL '1 second',COALESCE(cd.expires_at,p.admission_until)) DESC,p.enqueued_at DESC,p.id LIMIT 1`, service.BorrowPoolVerificationBudget(cont, "mint").Milliseconds(), a.TargetID, scope, service.BorrowGatewayFeedbackEnabled(ctx)).Scan(&id, &secret, &queued, &now, &deadline, &demoted, &freshnessRank)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c service.BorrowPoolCandidate
	if err = r.open(secret, &c); err != nil {
		return nil, err
	}
	if c.ID != id || c.Details == nil || c.Environment == nil || c.Environment.Fingerprint == nil {
		return nil, service.ErrBorrowUnavailable
	}
	c.Details.GatewayPriority = "normal"
	if freshnessRank > 1 {
		c.Details.GatewayPriority = "prioritized"
	}
	if demoted {
		c.Details.GatewayPriority = "recheck"
	}
	c.Expires = deadline
	c.Details.BatchID = a.Lease
	c.Details.TargetRevision, c.Details.TargetIdentityHash = a.Revision, a.Identity
	c.Details.PoolEnqueuedAt = &queued
	c.Details.PoolClaimedAt = &now
	wait := now.Sub(queued).Milliseconds()
	c.Details.ProbeQueueMS = &wait
	c.Details.PoolMinimumRemainingMS = service.BorrowPoolVerificationBudget(cont, "mint").Milliseconds()
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_pool SET state='claimed',target_id=$2,target_revision=$3,identity_hash=$4,claim_token=$5,claimed_at=clock_timestamp(),
 admission_until=LEAST(admission_until,$6),lease_until=clock_timestamp()+$7*INTERVAL '1 millisecond' WHERE id=$1`, id, a.TargetID, a.Revision, a.Identity, a.Lease, deadline, service.BorrowCandidatePoolClaimLease.Milliseconds()); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_environments SET target_id=$2,batch_id=$3,admission_until=LEAST(admission_until,$4),lease_until=GREATEST(lease_until,clock_timestamp()+$5*INTERVAL '1 millisecond') WHERE id=$1`, id, a.TargetID, a.Lease, deadline, service.BorrowCandidatePoolClaimLease.Milliseconds()); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_pool_demand SET granted_at=clock_timestamp() WHERE target_id=$1`, a.TargetID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_pool_control SET consumer_cursor=consumer_cursor+1 WHERE id=1`); err != nil {
		return nil, err
	}
	details, err := json.Marshal(c.Details)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_pool SET details=$2 WHERE id=$1`, id, details); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,source_id,stage,reason,details) SELECT target_id,source_id,'candidate','candidate_pool_claimed',details FROM ticket_borrow_candidate_pool WHERE id=$1`, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &c, nil
}

func finishBorrowPoolRows(ctx context.Context, tx *sql.Tx, condition string, args ...any) error {
	_, err := tx.ExecContext(ctx, `WITH finished AS (
 UPDATE ticket_borrow_candidate_pool p SET state='finished',finished_at=clock_timestamp(),secret='',reason=$1 WHERE p.state<>'finished' AND (`+condition+`) RETURNING p.*),
 released AS (UPDATE ticket_borrow_candidate_environments e SET released_at=COALESCE(released_at,clock_timestamp()) FROM finished f WHERE e.id=f.id)
 INSERT INTO ticket_borrow_events(target_id,source_id,stage,reason,details) SELECT COALESCE(target_id,0),source_id,'candidate',reason,details||jsonb_build_object('candidate_id',id) FROM finished`, args...)
	return err
}
func (r *ticketBorrowRepository) FinishCandidatePool(ctx context.Context, id, owner, reason string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockBorrowCandidatePool(ctx, tx); err != nil {
		return err
	}
	err = finishBorrowPoolRows(ctx, tx, `p.id=$2 AND ((p.state IN ('acquiring','queued') AND p.owner_token=$3) OR (p.state='claimed' AND p.claim_token=$3))`, reason, id, owner)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) SweepCandidatePool(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockBorrowCandidatePool(ctx, tx); err != nil {
		return err
	}
	// Never release physical source/target work merely because configuration
	// changed. Owners drain it; expired tasks are reaped only after HTTP leases.
	if err = finishBorrowPoolRows(ctx, tx, `p.state='acquiring' AND p.lease_until<=clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_source_attempts q WHERE q.pool_candidate_id=p.id AND q.completed_at IS NULL AND q.lease_until>clock_timestamp())`, "candidate_pool_source_abandoned"); err != nil {
		return err
	}
	if err = finishBorrowPoolRows(ctx, tx, `p.state='queued' AND (p.lease_until<=clock_timestamp() OR p.admission_until<=clock_timestamp()
 OR NOT EXISTS(SELECT 1 FROM ticket_borrow_settings cfg WHERE cfg.id=1 AND cfg.running AND cfg.revision=p.settings_revision))`, "candidate_pool_expired_or_stopped"); err != nil {
		return err
	}
	if err = finishBorrowPoolRows(ctx, tx, `p.state='queued' AND EXISTS(
 SELECT 1 FROM ticket_borrow_candidate_environments e CROSS JOIN ticket_borrow_settings cfg
 LEFT JOIN ticket_borrow_cookie_deadlines cd ON cd.cookie_hash=p.cookie_hash
 WHERE e.id=p.id AND cfg.id=1 AND LEAST(p.admission_until,e.admission_until,cd.expires_at)
 <clock_timestamp()+(45+cfg.continue_timeout_seconds+$2)*INTERVAL '1 second')`, "candidate_time_budget_insufficient", int(service.BorrowCandidatePoolPublishReserve/time.Second)); err != nil {
		return err
	}
	if err = finishBorrowPoolRows(ctx, tx, `p.state='claimed' AND (p.lease_until<=clock_timestamp() OR NOT EXISTS(
 SELECT 1 FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings cfg WHERE cfg.id=1 AND cfg.running AND cfg.revision=p.settings_revision
 AND t.account_id=p.target_id AND t.revision=p.target_revision AND t.enabled AND t.lease_token=p.claim_token AND t.lease_until>clock_timestamp()
 AND t.lease_policy_revision=t.probe_policy_revision AND t.lease_acquisition_revision=t.acquisition_revision))
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_requests q WHERE q.details->>'candidate_id'=p.id AND q.finished_at IS NULL AND q.lease_until>clock_timestamp())`, "candidate_pool_verification_abandoned"); err != nil {
		return err
	}
	var consumers bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1`+borrowPoolDemandFrom+`)`).Scan(&consumers); err != nil {
		return err
	}
	if !consumers {
		if err = finishBorrowPoolRows(ctx, tx, `p.state='queued'`, "candidate_pool_no_consumers"); err != nil {
			return err
		}
	}
	// Unsent expired attempts are already cancelled by their owners. Abandoned
	// intents cannot prevent a source account or future producer from recovering.
	if _, err = tx.ExecContext(ctx, `DELETE FROM ticket_borrow_source_intents i WHERE pool_candidate_id IS NOT NULL AND
 (expires_at<=clock_timestamp() OR NOT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool p WHERE p.id=i.pool_candidate_id AND p.state='acquiring' AND p.lease_until>clock_timestamp()))`); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) PublishedPoolCandidate(ctx context.Context, a service.BorrowAcquisition, candidate string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `SELECT id FROM ticket_borrow_routes WHERE target_id=$1 AND target_revision=$2 AND identity_hash=$3 AND candidate_id=$4 AND NOT validation_only`, a.TargetID, a.Revision, a.Identity, candidate).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

var _ service.BorrowCandidatePoolRepository = (*ticketBorrowRepository)(nil)
