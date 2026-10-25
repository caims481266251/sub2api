package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// All paths acquire target -> health/settings -> route locks. Business leases
// stay independent of admission state until their response bodies finish.
func (r *ticketBorrowRepository) ObserveRouteOutcome(ctx context.Context, o service.BorrowRouteOutcome) error {
	if o.Route == nil || o.Lease == "" {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, o.Route.TargetID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_route_leases SET outcome_recorded=true WHERE id=$1 AND route_id=$2 AND generation=$3 AND NOT outcome_recorded`, o.Lease, o.Route.ID, o.Route.Generation)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil
	}
	if err = updateBorrowRouteOutcome(ctx, tx, o, false, ""); err != nil {
		return err
	}
	return tx.Commit()
}

func updateBorrowRouteOutcome(ctx context.Context, tx *sql.Tx, o service.BorrowRouteOutcome, health bool, signature string) error {
	route := o.Route
	if o.Success {
		// Only a verified health result can restore pending routes; business can
		// only record success for the current normal generation.
		res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_routes r SET state='normal',cooldown_until=NULL,
 generation=r.generation,
 last_success_at=clock_timestamp(),last_business_success_at=CASE WHEN $4 THEN r.last_business_success_at ELSE clock_timestamp() END
 FROM ticket_borrow_targets t,accounts a WHERE r.id=$1 AND r.generation=$2 AND r.target_id=$3 AND t.account_id=r.target_id AND a.id=t.account_id
 AND t.enabled AND t.revision=r.target_revision AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND (NOT $4 OR `+borrowAccountSignature+`=$5)
 AND r.revoked_at IS NULL AND r.inventory_eligible AND `+borrowQualityEligible+` AND ((NOT $4 AND r.state='normal') OR ($4 AND r.state IN ('normal','pending_probe','pending_quality')))
 AND LEAST(r.expires_at,r.admission_until,COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=r.cookie_hash),r.expires_at))>clock_timestamp()`, route.ID, route.Generation, route.TargetID, health, signature)
		if err == nil && health {
			n, _ := res.RowsAffected()
			if n == 0 {
				return service.ErrBorrowUnavailable
			}
		}
		return err
	}
	if o.Reason == "" {
		return nil
	}
	// One state change per generation; late concurrent failures may only extend
	// an explicit longer Retry-After within that same cooling generation.
	_, err := tx.ExecContext(ctx, `WITH changed AS (
 UPDATE ticket_borrow_routes r SET state='cooling',generation=r.generation+1,quality_valid_until=NULL,
 cooldown_until=GREATEST(clock_timestamp()+(CASE WHEN $8 THEN (s.route_quality->>'interval_seconds')::int ELSE s.route_cooldown_seconds END)*INTERVAL '1 second',$4::timestamptz,t.upstream_wait_until),
 last_error=$5,last_error_at=clock_timestamp()
 FROM ticket_borrow_settings s,ticket_borrow_targets t WHERE s.id=1 AND t.account_id=r.target_id
 AND r.id=$1 AND r.target_id=$3 AND r.generation=$2 AND r.revoked_at IS NULL
 AND (r.state='normal' OR ($6 AND r.state IN ('pending_probe','pending_quality')))
 AND (NOT $6 OR EXISTS(SELECT 1 FROM accounts a WHERE a.id=r.target_id AND `+borrowAccountSignature+`=$7))
 AND LEAST(r.expires_at,r.admission_until)>clock_timestamp() RETURNING r.*
 ), wake AS (
 UPDATE ticket_borrow_targets t SET refresh_generation=refresh_generation+1,next_probe_at=clock_timestamp(),next_attempt_at=clock_timestamp()
 WHERE t.account_id=$3 AND EXISTS(SELECT 1 FROM changed)
 ) INSERT INTO ticket_borrow_events(target_id,source_id,route_id,stage,reason,details)
 SELECT target_id,source_id,id,'route','route_cooling',jsonb_build_object('generation',generation,'cooldown_until',cooldown_until,'cause',last_error) FROM changed`, route.ID, route.Generation, route.TargetID, o.RetryAt, o.Reason, health, signature, o.QualityRetry)
	if err != nil {
		return err
	}
	if o.RetryAt != nil {
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_routes SET cooldown_until=GREATEST(cooldown_until,$3) WHERE id=$1 AND generation=$2+1 AND state='cooling'`, route.ID, route.Generation, *o.RetryAt)
	}
	return err
}

func (r *ticketBorrowRepository) CheckRouteReservation(ctx context.Context, route *service.BorrowRoute, lease string) error {
	var eligible bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_routes r JOIN ticket_borrow_targets t ON t.account_id=r.target_id JOIN accounts a ON a.id=t.account_id
 LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 JOIN ticket_borrow_route_leases l ON l.route_id=r.id
 WHERE r.id=$1 AND r.generation=$2 AND l.id=$3 AND l.generation=r.generation AND l.lease_until>clock_timestamp()
 AND (`+borrowRouteEligibilityReason+`)='')`, route.ID, route.Generation, lease).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return service.ErrBorrowNoEligibleRoute
	}
	return nil
}

func (r *ticketBorrowRepository) SweepRoutePool(ctx context.Context) error {
	// Row state is descriptive; every admission also checks time and authority.
	// Acquire target locks first to avoid racing outcome/publication lock order.
	_, err := r.db.ExecContext(ctx, `WITH locked AS MATERIALIZED (
 SELECT t.account_id FROM ticket_borrow_targets t WHERE EXISTS(SELECT 1 FROM ticket_borrow_routes r
 WHERE r.target_id=t.account_id AND r.state IN ('normal','pending_probe','pending_quality','cooling') AND
 (LEAST(r.expires_at,r.admission_until,COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=r.cookie_hash),r.expires_at))<=clock_timestamp() OR r.revoked_at IS NOT NULL OR r.target_revision<>t.revision OR NOT t.enabled))
 ORDER BY t.account_id FOR UPDATE OF t SKIP LOCKED)
 UPDATE ticket_borrow_routes r SET state=CASE WHEN r.revoked_at IS NOT NULL OR r.target_revision<>t.revision OR NOT t.enabled THEN 'invalid' ELSE 'expired' END,generation=generation+1
 FROM locked,ticket_borrow_targets t WHERE t.account_id=locked.account_id AND r.target_id=t.account_id AND r.state IN ('normal','pending_probe','pending_quality','cooling') AND
 (LEAST(r.expires_at,r.admission_until,COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=r.cookie_hash),r.expires_at))<=clock_timestamp() OR r.revoked_at IS NOT NULL OR r.target_revision<>t.revision OR NOT t.enabled)`)
	return err
}

// Both ordinary business and background requests count against account capacity.
// Probe concurrency remains an additional background-only limit.
const borrowBusinessInflight = `(SELECT count(*) FROM ticket_borrow_route_leases WHERE target_id=$1 AND lease_until>clock_timestamp())`

// Start with the previous check's duration plus two seconds for scheduling.
// Leave at least one second after a completed pass, even for checks longer than
// the interval. Capacity/budget still apply; a delayed check cannot extend the
// old pass, so slow checks can still leave an admission gap.
const borrowQualityRefreshLead = `LEAST(
 GREATEST(1,(SELECT (route_quality->>'interval_seconds')::int FROM ticket_borrow_settings WHERE id=1)-1),
 GREATEST(2,COALESCE((r.quality_result->>'duration_ms')::double precision,0)/1000+2))*INTERVAL '1 second'`

const borrowHealthDue = `r.inventory_eligible AND NOT r.validation_only AND r.revoked_at IS NULL AND t.revision=r.target_revision AND t.enabled
 AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(t.upstream_wait_until<=clock_timestamp(),true) AND COALESCE(t.budget_wait_until<=clock_timestamp(),true)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_rotation_accounts ra WHERE ra.account_id=r.target_id AND ra.phase='draining' AND ra.active_job IS NOT NULL)
 AND COALESCE(r.health_until<=clock_timestamp(),true)
 AND LEAST(r.expires_at,r.admission_until,COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=r.cookie_hash),r.expires_at))>clock_timestamp()
 AND ((r.state IN ('pending_probe','pending_quality') AND COALESCE(r.health_checked_at<=clock_timestamp()-(SELECT (route_quality->>'interval_seconds')::int FROM ticket_borrow_settings WHERE id=1)*INTERVAL '1 second',true)) OR (r.state='cooling' AND r.cooldown_until<=clock_timestamp() AND COALESCE(r.health_checked_at<=clock_timestamp()-(SELECT (route_quality->>'interval_seconds')::int FROM ticket_borrow_settings WHERE id=1)*INTERVAL '1 second',true)) OR
 (r.state='normal' AND COALESCE(r.quality_valid_until<=clock_timestamp()+` + borrowQualityRefreshLead + `,true)))`

func (r *ticketBorrowRepository) AcquireRouteHealth(ctx context.Context, token string) (*service.BorrowRouteHealthJob, error) {
	if token == "" {
		return nil, service.ErrBorrowUnavailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Global recovery priority, round robin by last target, then oldest due time.
	var target int64
	err = tx.QueryRowContext(ctx, `SELECT t.account_id FROM ticket_borrow_targets t JOIN accounts a ON a.id=t.account_id
	CROSS JOIN ticket_borrow_route_health h CROSS JOIN ticket_borrow_settings cfg WHERE h.id=1 AND cfg.id=1
 AND COALESCE(h.last_started_at<=clock_timestamp()-INTERVAL '1 second',true)
 AND EXISTS(SELECT 1 FROM ticket_borrow_routes r WHERE r.target_id=t.account_id AND `+borrowHealthDue+` AND `+replaceBorrowTarget(borrowHealthBudgetAvailable, "t.account_id")+`)
 AND (`+replaceBorrowTarget(borrowBusinessInflight+" + "+borrowBackgroundInflight, "t.account_id")+`)<GREATEST(1,a.concurrency)
 AND (`+replaceBorrowTarget(borrowBackgroundInflight, "t.account_id")+`)<(SELECT CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE s.probe_concurrency END FROM ticket_borrow_settings s WHERE s.id=1)
 AND `+borrowHealthHasTurn+`
 ORDER BY EXISTS(SELECT 1 FROM ticket_borrow_routes r WHERE r.target_id=t.account_id AND r.state='normal' AND `+borrowHealthDue+`) DESC,
 (t.account_id>h.last_target_id) DESC,t.account_id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Recheck after the target lock: a concurrent candidate may have reserved
	// capacity while the initial SELECT was taking its snapshot.
	var capacityOK bool
	err = tx.QueryRowContext(ctx, `SELECT `+borrowHealthHasTurn+` AND (`+borrowBackgroundInflight+`)<CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE cfg.probe_concurrency END
 AND (`+borrowBackgroundInflight+` + `+borrowBusinessInflight+`)<GREATEST(1,a.concurrency)
 FROM ticket_borrow_targets t JOIN accounts a ON a.id=t.account_id CROSS JOIN ticket_borrow_settings cfg WHERE t.account_id=$1 AND cfg.id=1`, target).Scan(&capacityOK)
	if err != nil {
		return nil, err
	}
	if !capacityOK {
		return nil, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_route_health SET last_started_at=clock_timestamp(),last_target_id=$1 WHERE id=1`, target); err != nil {
		return nil, err
	}
	var route service.BorrowRoute
	var secret, signature string
	var qualityJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT r.id,r.target_id,r.target_revision,COALESCE(r.source_id,0),r.identity_hash,r.cookie_hash,r.gateway,r.expires_at,r.admission_until,r.generation,r.secret,r.verification_model,`+borrowAccountSignature+`,(SELECT route_quality FROM ticket_borrow_settings WHERE id=1)
 FROM ticket_borrow_routes r JOIN ticket_borrow_targets t ON t.account_id=r.target_id JOIN accounts a ON a.id=t.account_id
 WHERE r.target_id=$1 AND `+borrowHealthDue+`
 AND `+borrowHealthBudgetAvailable+` ORDER BY CASE r.state WHEN 'normal' THEN 0 WHEN 'pending_quality' THEN 1 ELSE 2 END,COALESCE(r.quality_valid_until,r.cooldown_until,r.health_checked_at,r.checked_at),r.id LIMIT 1 FOR UPDATE OF r`, target).Scan(&route.ID, &route.TargetID, &route.TargetRevision, &route.SourceID, &route.IdentityHash, &route.CookieHash, &route.Gateway, &route.ExpiresAt, &route.UsableUntil, &route.Generation, &secret, &route.VerificationModel, &signature, &qualityJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = r.openRouteSecret(ctx, tx, route.ID, secret, &route.Secret); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `UPDATE ticket_borrow_routes SET
 state=CASE WHEN state='normal' THEN state ELSE 'pending_quality' END,
 generation=generation+CASE WHEN state IN ('normal','pending_probe','pending_quality') THEN 0 ELSE 1 END,
 quality_valid_until=CASE WHEN state='normal' THEN quality_valid_until ELSE NULL END,
 health_token=$2,health_until=clock_timestamp()+INTERVAL '60 seconds',health_checked_at=clock_timestamp() WHERE id=$1 RETURNING generation`, route.ID, token).Scan(&route.Generation); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_requests(id,target_id,kind,batch_id,source_id,phase,lease_until,details) VALUES($1,$2,'probe',$1,0,'route_health',clock_timestamp()+INTERVAL '60 seconds',jsonb_build_object('route_id',$3::text))`, token, target, route.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	var quality service.BorrowRouteQualityConfig
	if err = json.Unmarshal(qualityJSON, &quality); err != nil {
		return nil, err
	}
	return &service.BorrowRouteHealthJob{Route: &route, Token: token, AccountSignature: signature, QualityConfig: &quality}, nil
}

func (r *ticketBorrowRepository) CheckRouteHealth(ctx context.Context, j *service.BorrowRouteHealthJob, identity string) error {
	return checkBorrowRouteHealth(ctx, r.db, j, identity)
}

// A pooled route check owns one capacity/budget reservation and sends only
// quality. Candidate mint/continue requests use their separate acquisition.
func (r *ticketBorrowRepository) beginRouteHealthRequest(ctx context.Context, q service.BorrowRequest) error {
	if q.Kind != "probe" || q.TargetID != q.RouteHealth.Route.TargetID || q.BatchID != q.RouteHealth.Token || (q.Phase != "quality" || q.ID != q.RouteHealth.Token) {
		return service.ErrBorrowUnavailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, q.TargetID); err != nil {
		return err
	}
	if err = checkBorrowRouteHealth(ctx, tx, q.RouteHealth, q.Identity); err != nil {
		return err
	}
	if err = reapBorrowRequests(ctx, tx, q.TargetID); err != nil {
		return err
	}
	var inflight, business, capacity, concurrency int
	err = tx.QueryRowContext(ctx, `SELECT (`+borrowBackgroundInflight+`),`+borrowBusinessInflight+`,GREATEST(1,a.concurrency),
 CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE s.probe_concurrency END
 FROM ticket_borrow_targets t JOIN accounts a ON a.id=t.account_id CROSS JOIN ticket_borrow_settings s WHERE t.account_id=$1 AND s.id=1`, q.TargetID).Scan(&inflight, &business, &capacity, &concurrency)
	if err != nil {
		return err
	}
	if inflight > concurrency || inflight+business > capacity {
		return service.ErrBorrowProbeBusy
	}
	details, err := borrowRequestDetails(q)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_requests SET phase='quality',details=$2
 WHERE id=$1 AND batch_id=$1 AND phase='route_health' AND finished_at IS NULL AND started_at IS NULL AND lease_until>clock_timestamp()`, q.ID, details)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowUnavailable
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) checkRouteHealthRequest(ctx context.Context, q service.BorrowRequest) error {
	if err := r.CheckRouteHealth(ctx, q.RouteHealth, q.Identity); err != nil {
		return err
	}
	var valid bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_requests WHERE id=$1 AND batch_id=$2 AND target_id=$3 AND kind='probe' AND phase=$4 AND finished_at IS NULL AND lease_until>clock_timestamp())`, q.ID, q.RouteHealth.Token, q.TargetID, q.Phase).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return service.ErrBorrowUnavailable
	}
	return nil
}

func checkBorrowRouteHealth(ctx context.Context, db borrowQueryer, j *service.BorrowRouteHealthJob, identity string) error {
	var valid bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_routes r JOIN ticket_borrow_targets t ON t.account_id=r.target_id JOIN accounts a ON a.id=t.account_id
 WHERE r.id=$1 AND r.generation=$2 AND r.identity_hash=$3 AND r.health_token=$4 AND r.health_until>clock_timestamp()
 AND r.inventory_eligible AND NOT r.validation_only AND r.state IN ('normal','pending_probe','pending_quality') AND r.revoked_at IS NULL AND t.enabled AND t.revision=r.target_revision
 AND COALESCE(t.upstream_wait_until<=clock_timestamp(),true) AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_rotation_accounts ra WHERE ra.account_id=r.target_id AND ra.phase='draining' AND ra.active_job IS NOT NULL)
 AND `+borrowAccountSignature+`=$5
 AND LEAST(r.expires_at,r.admission_until,COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=r.cookie_hash),r.expires_at))>clock_timestamp()
 AND (SELECT route_quality FROM ticket_borrow_settings WHERE id=1)=$6::jsonb)`, j.Route.ID, j.Route.Generation, identity, j.Token, j.AccountSignature, borrowQualityConfigJSON(j)).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return service.ErrBorrowUnavailable
	}
	return nil
}

func (r *ticketBorrowRepository) RenewRouteHealth(ctx context.Context, j *service.BorrowRouteHealthJob) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, j.Route.TargetID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_routes SET health_until=clock_timestamp()+INTERVAL '60 seconds' WHERE id=$1 AND health_token=$2 AND health_until>clock_timestamp()`, j.Route.ID, j.Token)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowUnavailable
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_routes SET health_until=clock_timestamp()+INTERVAL '60 seconds' WHERE id=$1 AND health_token=$2`, j.Route.ID, j.Token); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_requests SET lease_until=clock_timestamp()+INTERVAL '60 seconds' WHERE id=$1 AND finished_at IS NULL`, j.Token); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) FinishRouteHealth(ctx context.Context, j *service.BorrowRouteHealthJob, o service.BorrowRouteOutcome) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, j.Route.TargetID); err != nil {
		return err
	}
	owned := true
	if err = checkBorrowRouteHealth(ctx, tx, j, j.Route.IdentityHash); err != nil {
		if !errors.Is(err, service.ErrBorrowUnavailable) {
			return err
		}
		owned = false
	}
	if owned && o.Success {
		passed := o.Quality.Passed(*j.QualityConfig)
		var completed bool
		if passed {
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_requests WHERE id=$1 AND batch_id=$1 AND target_id=$2 AND kind='probe' AND phase='quality' AND finished_at IS NOT NULL AND started_at IS NOT NULL AND status=200)`, j.Token, j.Route.TargetID).Scan(&completed)
			if err != nil {
				return err
			}
		}
		if !passed || !completed {
			o.Success = false
			o.Reason = "route_quality_incomplete"
		}
	}
	if !owned {
		o.Success = false
		o.Reason = "route_health_stale"
	}
	if owned && !o.Success && o.Reason != "" && o.Quality != nil {
		o.QualityRetry = true
		if o.Quality != nil {
			o.Quality.Status = "failed"
			o.Quality.Reason = o.Reason
		}
	}

	if owned && o.Quality != nil {
		raw, e := json.Marshal(o.Quality)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_routes SET quality_result=$2,quality_valid_until=CASE WHEN $3 THEN clock_timestamp()+$5*INTERVAL '1 second' ELSE NULL END WHERE id=$1 AND health_token=$4`, j.Route.ID, raw, o.Success, j.Token, j.QualityConfig.IntervalSeconds); err != nil {
			return err
		}
	}
	if owned {
		o.Route = j.Route
		if err = updateBorrowRouteOutcome(ctx, tx, o, true, j.AccountSignature); err != nil {
			if !errors.Is(err, service.ErrBorrowUnavailable) {
				return err
			}
			o.Success = false
			o.Reason = "route_health_stale"
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL WHERE id=1 AND token=$1`, j.Token); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_routes SET health_token='',health_until=NULL WHERE id=$1 AND health_token=$2`, j.Route.ID, j.Token); err != nil {
		return err
	}
	reason := o.Reason
	if o.Success {
		reason = "route_quality_passed"
	}
	if reason == "" {
		reason = "route_health_cancelled"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_requests SET finished_at=clock_timestamp(),outcome=$2 WHERE id=$1 AND finished_at IS NULL`, j.Token, reason); err != nil {
		return err
	}
	details := &service.BorrowCandidateDetails{Quality: o.Quality}

	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,route_id,stage,reason,details) VALUES($1,$2,'route',$3,$4)`, j.Route.TargetID, j.Route.ID, reason, raw); err != nil {
		return err
	}
	return tx.Commit()
}

var _ service.BorrowRoutePoolRepository = (*ticketBorrowRepository)(nil)

func replaceBorrowTarget(s, target string) string { return strings.ReplaceAll(s, "$1", target) }

// Shared by every business admission path; an overdue check is fail-closed even
// before the background worker has claimed it. Time never interrupts sent leases.
const borrowQualityEligible = `r.quality_result->>'status'='passed' AND r.quality_valid_until>clock_timestamp()`

func borrowQualityConfigJSON(j *service.BorrowRouteHealthJob) []byte {
	raw, _ := json.Marshal(j.QualityConfig)
	return raw
}
