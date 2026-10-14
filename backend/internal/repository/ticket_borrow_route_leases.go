package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Admission and its admin snapshot use the same eligibility and capacity rules.
// A quality check owns account capacity, but does not invalidate a still-current
// quality pass. Already reserved requests recheck eligibility without reserving
// a second slot.
const borrowRouteEligibilityReason = `CASE
 WHEN r.revoked_at IS NOT NULL OR NOT r.inventory_eligible OR r.validation_only OR t.revision<>r.target_revision OR NOT t.enabled OR r.state='invalid' THEN 'invalid'
 WHEN LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)<=clock_timestamp() OR r.state='expired' THEN 'expired'
 WHEN a.deleted_at IS NOT NULL OR a.status<>'active' OR NOT a.schedulable OR (a.auto_pause_on_expired AND a.expires_at<=clock_timestamp()) THEN 'account_unavailable'
 WHEN r.state='cooling' THEN 'cooling'
 WHEN t.upstream_wait_until>clock_timestamp() OR a.rate_limit_reset_at>clock_timestamp() OR a.overload_until>clock_timestamp() OR a.temp_unschedulable_until>clock_timestamp() THEN 'account_cooling'
 WHEN r.state='pending_probe' THEN 'pending_probe'
 WHEN r.state<>'normal' OR NOT COALESCE((` + borrowQualityEligible + `),false) THEN 'pending_quality'
 ELSE '' END`

const borrowRouteSlots = `LEAST(GREATEST(0,r.dispatch_limit-load.inflight),account_load.available)`
const borrowRouteAdmissionReason = `CASE WHEN r.identity_hash<>$2 THEN 'invalid'
 WHEN (` + borrowRouteEligibilityReason + `)<>'' THEN (` + borrowRouteEligibilityReason + `)
 WHEN account_load.available<=0 THEN 'account_busy'
 WHEN load.inflight>=r.dispatch_limit THEN 'route_busy' ELSE '' END`

const borrowRouteAdmissionFrom = ` FROM ticket_borrow_routes r
 LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 JOIN ticket_borrow_targets t ON t.account_id=r.target_id JOIN accounts a ON a.id=t.account_id
 CROSS JOIN LATERAL (SELECT count(*) AS inflight FROM ticket_borrow_route_leases l WHERE l.route_id=r.id AND l.lease_until>clock_timestamp()) load
 CROSS JOIN LATERAL (SELECT GREATEST(0,GREATEST(1,a.concurrency)-(` + borrowBusinessInflight + ` + ` + borrowBackgroundInflight + `)) AS available) account_load`

const borrowRouteSelect = `SELECT r.id,r.target_id,r.target_revision,COALESCE(r.source_id,0),r.identity_hash,r.cookie_hash,r.gateway,LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at)),r.secret,
 LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until),r.generation,r.verification_model,r.deadline_source
 ` + borrowRouteAdmissionFrom + ` WHERE t.account_id=$1 AND ($3='' OR r.id=$3)
 AND r.state='normal' AND r.revoked_at IS NULL AND r.inventory_eligible AND NOT r.validation_only
 AND r.identity_hash=$2 AND r.target_revision=t.revision AND (` + borrowRouteAdmissionReason + `)=''`

// Continuations retain latest-route semantics. New sessions spread current load.
const borrowRouteLatestOrder = `r.checked_at DESC,LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until) DESC,r.id`

func (r *ticketBorrowRepository) readRoute(ctx context.Context, db borrowQueryer, id int64, identity, preferred string) (*service.BorrowRoute, error) {
	return r.readRouteOrdered(ctx, db, id, identity, preferred, false)
}

func (r *ticketBorrowRepository) readRouteOrdered(ctx context.Context, db borrowQueryer, id int64, identity, preferred string, latest bool) (*service.BorrowRoute, error) {
	order := borrowRouteLatestOrder
	if !latest {
		order = `load.inflight,` + order
	}
	var route service.BorrowRoute
	var secret string
	err := db.QueryRowContext(ctx, borrowRouteSelect+" ORDER BY "+order+" LIMIT 1", id, identity, preferred).Scan(&route.ID, &route.TargetID, &route.TargetRevision, &route.SourceID, &route.IdentityHash, &route.CookieHash, &route.Gateway, &route.ExpiresAt, &secret, &route.UsableUntil, &route.Generation, &route.VerificationModel, &route.DeadlineSource)
	if errors.Is(err, sql.ErrNoRows) {
		var reason string
		err = db.QueryRowContext(ctx, `SELECT `+borrowRouteAdmissionReason+` AS reason`+borrowRouteAdmissionFrom+`
 WHERE t.account_id=$1 AND ($3='' OR r.id=$3)
 AND r.revoked_at IS NULL AND r.inventory_eligible AND NOT r.validation_only
 AND r.identity_hash=$2 AND r.target_revision=t.revision
 AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp()
 ORDER BY CASE (`+borrowRouteAdmissionReason+`) WHEN '' THEN 0 WHEN 'route_busy' THEN 1 WHEN 'account_busy' THEN 2 WHEN 'pending_quality' THEN 3 WHEN 'pending_probe' THEN 4 WHEN 'cooling' THEN 5 ELSE 6 END LIMIT 1`, id, identity, preferred).Scan(&reason)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && reason == "") {
			return nil, service.ErrBorrowNoEligibleRoute
		}
		if err != nil {
			return nil, err
		}
		return nil, &service.BorrowRouteAdmissionError{Reason: reason}
	}
	if err != nil {
		return nil, err
	}
	if err = r.openRouteSecret(ctx, db, route.ID, secret, &route.Secret); err != nil {
		return nil, err
	}
	return &route, nil
}

// The target lock serializes selection, conversation binding and occupancy.
// Read-only admission checks must never reserve a business slot.
func (r *ticketBorrowRepository) ReserveRoute(ctx context.Context, id int64, identity, preferred, session, lease string) (*service.BorrowRoute, error) {
	if lease == "" {
		return nil, service.ErrBorrowUnavailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var locked int64
	if err = tx.QueryRowContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, id).Scan(&locked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = service.ErrBorrowUnavailable
		}
		return nil, err
	}
	// Another request may have bound this conversation since the caller's read.
	if session != "" {
		var bound string
		err = tx.QueryRowContext(ctx, `SELECT route_id FROM ticket_borrow_sessions WHERE target_id=$1 AND session_hash=$2`, id, session).Scan(&bound)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			if preferred != "" && preferred != bound {
				return nil, service.ErrBorrowUnavailable
			}
			preferred = bound
		}
	}
	route, err := r.readRoute(ctx, tx, id, identity, preferred)
	if err != nil {
		return nil, err
	}
	if session != "" {
		var bound string
		err = tx.QueryRowContext(ctx, `INSERT INTO ticket_borrow_sessions(target_id,session_hash,route_id) VALUES($1,$2,$3) ON CONFLICT(target_id,session_hash) DO UPDATE SET session_hash=EXCLUDED.session_hash RETURNING route_id`, id, session, route.ID).Scan(&bound)
		if err != nil {
			return nil, err
		}
		if bound != route.ID {
			// Covers a concurrent response-ID binding outside this transaction.
			if preferred != "" && preferred != bound {
				return nil, service.ErrBorrowUnavailable
			}
			route, err = r.readRoute(ctx, tx, id, identity, bound)
			if err != nil {
				return nil, err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_route_leases(id,target_id,route_id,generation) VALUES($1,$2,$3,$4)`, lease, id, route.ID, route.Generation); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return route, nil
}

func (r *ticketBorrowRepository) RenewRoute(ctx context.Context, lease string) error {
	// Route expiry/retirement must not interrupt a stream already admitted.
	res, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_route_leases SET lease_until=clock_timestamp()+INTERVAL '60 seconds' WHERE id=$1 AND lease_until>clock_timestamp()`, lease)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return service.ErrBorrowUnavailable
	}
	return err
}

func (r *ticketBorrowRepository) ReleaseRoute(ctx context.Context, lease string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM ticket_borrow_route_leases WHERE id=$1`, lease)
	return err
}

func (r *ticketBorrowRepository) RouteStatuses(ctx context.Context, id int64, identity string) ([]service.BorrowRouteStatus, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT r.id,r.gateway,COALESCE(r.acquisition_account_id,src.account_id,0),
 LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at)),
 LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until),
 r.inventory_eligible,
 (`+borrowRouteAdmissionReason+`)='',
 CASE WHEN (`+borrowRouteAdmissionReason+`)='' THEN `+borrowRouteSlots+` ELSE 0 END,
 `+borrowRouteAdmissionReason+`,
 load.inflight,r.checked_at,r.checked_at,
 CASE WHEN r.revoked_at IS NOT NULL OR NOT r.inventory_eligible OR r.identity_hash<>$2 OR t.revision<>r.target_revision OR NOT t.enabled THEN 'invalid'
 WHEN LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)<=clock_timestamp() THEN 'expired' WHEN r.state='normal' AND NOT COALESCE((`+borrowQualityEligible+`),false) THEN 'pending_quality' ELSE r.state END,
 r.generation,r.dispatch_limit,r.cooldown_until,r.last_success_at,r.last_business_success_at,r.last_error_at,r.last_error,
 COALESCE(r.health_until>clock_timestamp(),false),r.verification_model,r.deadline_source,r.quality_result,r.quality_valid_until
 `+borrowRouteAdmissionFrom+` LEFT JOIN ticket_borrow_sources src ON src.id=r.source_id
 WHERE r.target_id=$1 AND NOT r.validation_only
 AND (r.checked_at>clock_timestamp()-INTERVAL '1 day' OR load.inflight>0)
 ORDER BY `+borrowRouteLatestOrder, id, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []service.BorrowRouteStatus{}
	for rows.Next() {
		var status service.BorrowRouteStatus
		var quality []byte
		if err = rows.Scan(&status.ID, &status.Gateway, &status.SourceAccountID, &status.ExpiresAt, &status.UsableUntil, &status.InventoryEligible, &status.Usable, &status.AvailableSlots, &status.UnavailableReason, &status.Inflight, &status.PublishedAt, &status.CheckedAt, &status.State, &status.Generation, &status.DispatchLimit, &status.CooldownUntil, &status.LastSuccessAt, &status.LastBusinessSuccessAt, &status.LastErrorAt, &status.LastError, &status.Checking, &status.VerificationModel, &status.DeadlineSource, &quality, &status.QualityValidUntil); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(quality, &status.Quality); err != nil {
			return nil, err
		}
		if status.Quality != nil && status.Quality.Status == "" {
			status.Quality = nil
		}
		result = append(result, status)
	}
	return result, rows.Err()
}
