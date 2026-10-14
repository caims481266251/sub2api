package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Both reservation paths hold the target row before counting or inserting.
const borrowBackgroundInflight = `(SELECT count(*) FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe' AND finished_at IS NULL AND lease_until>clock_timestamp())
 + (SELECT count(*) FROM ticket_borrow_routes hr WHERE hr.target_id=$1 AND hr.health_token<>'' AND hr.health_until>clock_timestamp() AND NOT EXISTS(SELECT 1 FROM ticket_borrow_requests hq WHERE hq.batch_id=hr.health_token AND hq.finished_at IS NULL AND hq.lease_until>clock_timestamp()))
 + (SELECT count(*) FROM ticket_borrow_candidate_pool cp WHERE cp.target_id=$1 AND cp.state='claimed' AND cp.lease_until>clock_timestamp() AND NOT EXISTS(SELECT 1 FROM ticket_borrow_requests cq WHERE cq.target_id=cp.target_id AND cq.details->>'candidate_id'=cp.id AND cq.finished_at IS NULL AND cq.lease_until>clock_timestamp()))
 + (SELECT count(*) FROM ticket_borrow_source_attempts WHERE target_id=$1 AND source_id IS NULL AND completed_at IS NULL AND lease_until>clock_timestamp())`

// Only work owned by the candidate coordinator prevents a replacement task.
// Pooled-route health has independent ownership and continues between tasks.
const borrowAcquisitionInflight = `(SELECT count(*) FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe' AND finished_at IS NULL AND lease_until>clock_timestamp() AND COALESCE(details->>'route_id','')='')
 + (SELECT count(*) FROM ticket_borrow_candidate_pool WHERE target_id=$1 AND state='claimed' AND lease_until>clock_timestamp())
 + (SELECT count(*) FROM ticket_borrow_source_attempts WHERE target_id=$1 AND source_id IS NULL AND completed_at IS NULL AND lease_until>clock_timestamp())`

// All request reservations and recovery transitions serialize on the target row.
// A crashed process leaves a bounded lease, never an indefinitely positive counter.
func reapBorrowRequests(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `WITH expired AS (
 UPDATE ticket_borrow_requests SET finished_at=lease_until,outcome='lease_expired'
 WHERE target_id=$1 AND kind='probe' AND finished_at IS NULL AND lease_until<=clock_timestamp() RETURNING finished_at
 ) UPDATE ticket_borrow_targets SET last_request_finished_at=GREATEST(last_request_finished_at,(SELECT max(finished_at) FROM expired))
 WHERE account_id=$1 AND EXISTS(SELECT 1 FROM expired)`, id)
	return err
}
func (r *ticketBorrowRepository) BeginTargetRequest(ctx context.Context, q service.BorrowRequest) error {
	if q.RouteHealth != nil {
		return r.beginRouteHealthRequest(ctx, q)
	}
	if q.Kind != "probe" {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled bool
	var revision int64
	var lease string
	if err = tx.QueryRowContext(ctx, `SELECT enabled,revision,lease_token FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, q.TargetID).Scan(&enabled, &revision, &lease); err != nil {
		return err
	}
	var rotating bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_rotation_accounts WHERE account_id=$1 AND phase='draining' AND active_job IS NOT NULL)`, q.TargetID).Scan(&rotating); err != nil {
		return err
	}
	if rotating {
		return service.ErrBorrowRotationPending
	}
	if !enabled || (q.Kind == "probe" && (lease != q.BatchID || revision != q.Revision)) {
		return service.ErrBorrowUnavailable
	}
	if err = reapBorrowRequests(ctx, tx, q.TargetID); err != nil {
		return err
	}
	if q.Kind == "probe" {
		if err = checkBorrowAcquisition(ctx, tx, service.BorrowAcquisition{TargetID: q.TargetID, Revision: q.Revision, Lease: q.BatchID, Identity: q.Identity}, true); err != nil {
			return err
		}
		var running, cooled bool
		var inflight, concurrency int
		var raw []byte
		if err = tx.QueryRowContext(ctx, `SELECT s.running,s.recovery,COALESCE(t.upstream_wait_until<=clock_timestamp(),true),(`+borrowBackgroundInflight+`),CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE s.probe_concurrency END FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings s WHERE t.account_id=$1 AND s.id=1`, q.TargetID).Scan(&running, &raw, &cooled, &inflight, &concurrency); err != nil {
			return err
		}
		if !running {
			return service.ErrBorrowPaused
		}
		var claimed bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool WHERE id=$2 AND target_id=$1 AND claim_token=$3 AND state='claimed' AND lease_until>clock_timestamp())`, q.TargetID, q.CandidateID, q.BatchID).Scan(&claimed); err != nil {
			return err
		}
		if claimed {
			var issued bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_requests WHERE target_id=$1 AND details->>'candidate_id'=$2 AND finished_at IS NULL AND lease_until>clock_timestamp())`, q.TargetID, q.CandidateID).Scan(&issued); err != nil {
				return err
			}
			if issued {
				return service.ErrBorrowProbeBusy
			}
			// The claim already owns this position, including the gap between
			// mint and continuation. This request must not count itself twice.
			inflight--
		} else if q.Phase == "mint" && q.SourceID != 0 {
			var turn bool
			if err = tx.QueryRowContext(ctx, `SELECT `+borrowCandidateHasTurn+` FROM ticket_borrow_targets t JOIN accounts a ON a.id=t.account_id CROSS JOIN ticket_borrow_settings cfg WHERE t.account_id=$1 AND cfg.id=1`, q.TargetID).Scan(&turn); err != nil {
				return err
			}
			if !turn {
				return service.ErrBorrowProbeBusy
			}
		}
		var capacity, business int
		if err = tx.QueryRowContext(ctx, `SELECT GREATEST(1,a.concurrency),`+borrowBusinessInflight+` FROM accounts a WHERE a.id=$1`, q.TargetID).Scan(&capacity, &business); err != nil {
			return err
		}
		if inflight >= concurrency || inflight+business >= capacity {
			return service.ErrBorrowProbeBusy
		}
		if !cooled {
			return service.ErrBorrowRecoveryWaiting
		}
		var cfg service.BorrowRecoverySettings
		if err = json.Unmarshal(raw, &cfg); err != nil {
			return err
		}
		if cfg.ProbeRequestsPerMinute <= 0 {
			cfg.ProbeRequestsPerMinute = 12
		}
		{
			var count int
			var available sql.NullTime
			var now time.Time
			if err = tx.QueryRowContext(ctx, `SELECT count(*),(array_agg(reserved_at ORDER BY reserved_at DESC))[$2]+INTERVAL '60 seconds',statement_timestamp() FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe' AND reserved_at>statement_timestamp()-INTERVAL '60 seconds'`, q.TargetID, cfg.ProbeRequestsPerMinute).Scan(&count, &available, &now); err != nil {
				return err
			}
			if count >= cfg.ProbeRequestsPerMinute {
				if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET budget_wait_until=$2,last_reason='target_request_budget' WHERE account_id=$1`, q.TargetID, available.Time); err != nil {
					return err
				}
				if err = tx.Commit(); err != nil {
					return err
				}
				return &service.BorrowBudgetWaitError{Scope: "target", Until: available.Time, RetryAfter: available.Time.Sub(now)}
			}
		}
	}
	details, err := borrowRequestDetails(q)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_requests(id,target_id,kind,batch_id,source_id,phase,lease_until,details) VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()+INTERVAL '180 seconds',$7)`, q.ID, q.TargetID, q.Kind, q.BatchID, q.SourceID, q.Phase, details)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) CheckTargetRequest(ctx context.Context, q service.BorrowRequest) error {
	if q.RouteHealth != nil {
		return r.checkRouteHealthRequest(ctx, q)
	}
	if q.Kind != "probe" {
		return nil
	}
	var running, active, cooled bool
	err := r.db.QueryRowContext(ctx, `SELECT s.running,
 x.finished_at IS NULL AND x.lease_until>clock_timestamp() AND t.enabled AND (x.kind='business' OR (t.lease_token=$2 AND t.revision=$3 AND t.lease_until>clock_timestamp())),
 COALESCE(t.upstream_wait_until<=clock_timestamp(),true)
 FROM ticket_borrow_requests x JOIN ticket_borrow_targets t ON t.account_id=x.target_id CROSS JOIN ticket_borrow_settings s WHERE x.id=$1 AND s.id=1`, q.ID, q.BatchID, q.Revision).Scan(&running, &active, &cooled)
	if err == sql.ErrNoRows {
		return service.ErrBorrowUnavailable
	}
	if err != nil {
		return err
	}
	if !active {
		return service.ErrBorrowUnavailable
	}
	if q.Kind == "probe" && !running {
		return service.ErrBorrowPaused
	}
	if q.Kind == "probe" && !cooled {
		return service.ErrBorrowRecoveryWaiting
	}
	if q.Kind == "probe" {
		return checkBorrowAcquisition(ctx, r.db, service.BorrowAcquisition{TargetID: q.TargetID, Revision: q.Revision, Lease: q.BatchID, Identity: q.Identity}, false)
	}
	return nil
}
func (r *ticketBorrowRepository) MarkTargetRequestSent(ctx context.Context, q service.BorrowRequest) error {
	if q.Kind != "probe" {
		return nil
	}
	// Check immediately before the transport can write. The reservation includes
	// dial time; started_at is the first connected dispatch, not a packet capture.
	if err := r.CheckTargetRequest(ctx, q); err != nil {
		return err
	}
	details, err := borrowRequestDetails(q)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `WITH sent AS (UPDATE ticket_borrow_requests SET started_at=clock_timestamp(),details=$2 WHERE id=$1 AND started_at IS NULL AND finished_at IS NULL RETURNING target_id,started_at)
	 UPDATE ticket_borrow_targets t SET last_request_started_at=GREATEST(t.last_request_started_at,s.started_at) FROM sent s WHERE t.account_id=s.target_id`, q.ID, details)
	return err
}

func borrowRequestDetails(q service.BorrowRequest) ([]byte, error) {
	return json.Marshal(struct {
		Diagnostic             *service.BorrowHTTPAttempt `json:"diagnostic,omitempty"`
		ContinueTimeoutSeconds int                        `json:"continue_timeout_seconds,omitempty"`
		CandidateID            string                     `json:"candidate_id,omitempty"`
		RouteID                string                     `json:"route_id,omitempty"`
		Environment            *service.BorrowEnvironment `json:"environment,omitempty"`
	}{q.Diagnostic, q.ContinueTimeoutSeconds, q.CandidateID, q.RouteID, q.Environment})
}
func (r *ticketBorrowRepository) RenewTargetRequest(ctx context.Context, q service.BorrowRequest) error {
	if q.Kind != "probe" {
		return nil
	}
	res, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_requests SET lease_until=clock_timestamp()+INTERVAL '180 seconds' WHERE id=$1 AND finished_at IS NULL AND lease_until>clock_timestamp()`, q.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowUnavailable
	}
	return nil
}
func (r *ticketBorrowRepository) EndTargetRequest(ctx context.Context, q service.BorrowRequest, status int, outcome string) error {
	if q.Kind != "probe" {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, q.TargetID); err != nil {
		return err
	}
	diagnostic, err := json.Marshal(struct {
		Diagnostic *service.BorrowHTTPAttempt `json:"diagnostic,omitempty"`
	}{q.Diagnostic})
	if err != nil {
		return err
	}
	// Finishing business records activity but never extends an upstream deadline.
	_, err = tx.ExecContext(ctx, `WITH ended AS (UPDATE ticket_borrow_requests SET finished_at=clock_timestamp(),status=$2,outcome=$3,details=COALESCE(details,'{}'::jsonb)||$4::jsonb WHERE id=$1 AND finished_at IS NULL RETURNING target_id,finished_at)
 UPDATE ticket_borrow_targets t SET last_request_finished_at=GREATEST(t.last_request_finished_at,e.finished_at)
 FROM ended e WHERE t.account_id=e.target_id`, q.ID, status, outcome, diagnostic)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) RecordProbeOutcome(ctx context.Context, id int64, lease, reason string, retryAfter time.Duration) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentLease string
	if err = tx.QueryRowContext(ctx, `SELECT lease_token FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, id).Scan(&currentLease); err != nil {
		return err
	}
	if currentLease != lease {
		return service.ErrBorrowConflict
	}
	if reason == "target_probe_passed" {
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET consecutive_failures=0 WHERE account_id=$1`, id)
	} else if reason == "target_ticket_changed" || reason == "target_route_changed" || reason == "target_continue_timeout" {
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets t SET consecutive_failures=consecutive_failures+1,acquisition_duration_ms=GREATEST(acquisition_duration_ms,`+borrowSearchElapsedMS+`) WHERE account_id=$1`, id)
	}
	if err != nil {
		return err
	}
	if retryAfter > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET upstream_wait_until=GREATEST(upstream_wait_until,clock_timestamp()+$2*INTERVAL '1 millisecond'),upstream_wait_reason=$3 WHERE account_id=$1`, id, retryAfter.Milliseconds(), reason)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) AdvanceScan(ctx context.Context, id int64, lease string, count int) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_targets SET scan_offset=LEAST(scan_offset+$3,jsonb_array_length(scan_sources)) WHERE account_id=$1 AND lease_token=$2`, id, lease, count)
	return err
}
