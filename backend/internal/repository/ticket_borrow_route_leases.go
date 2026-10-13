package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const borrowRouteSelect = `SELECT r.id,r.target_id,r.target_revision,COALESCE(r.source_id,0),r.identity_hash,r.cookie_hash,r.gateway,LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at)),r.secret,
 LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)
 FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash JOIN ticket_borrow_targets t ON t.account_id=r.target_id AND t.revision=r.target_revision
 JOIN accounts a ON a.id=t.account_id JOIN ticket_borrow_settings s ON s.id=1
 WHERE t.account_id=$1 AND t.enabled AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp()) AND r.revoked_at IS NULL AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp() AND r.identity_hash=$2 AND NOT r.validation_only AND (($3='' AND r.inventory_eligible) OR r.id=$3)`

// Publication inserts checked_at after verification. Expiry and ID only break ties.
const borrowRouteLatestOrder = `r.checked_at DESC,LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until) DESC,r.id`

func (r *ticketBorrowRepository) readRoute(ctx context.Context, db borrowQueryer, id int64, identity, preferred string) (*service.BorrowRoute, error) {
	order := borrowRouteLatestOrder
	var route service.BorrowRoute
	var secret string
	err := db.QueryRowContext(ctx, borrowRouteSelect+" ORDER BY "+order+" LIMIT 1", id, identity, preferred).Scan(&route.ID, &route.TargetID, &route.TargetRevision, &route.SourceID, &route.IdentityHash, &route.CookieHash, &route.Gateway, &route.ExpiresAt, &secret, &route.UsableUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrBorrowNoEligibleRoute
	}
	if err != nil {
		return nil, err
	}
	if err = r.open(secret, &route.Secret); err != nil {
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_route_leases(id,target_id,route_id) VALUES($1,$2,$3)`, lease, id, route.ID); err != nil {
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
 t.enabled AND t.revision=r.target_revision AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())

 AND r.revoked_at IS NULL AND r.identity_hash=$2 AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp(),
 load.inflight,r.checked_at,r.checked_at
 FROM ticket_borrow_routes r JOIN ticket_borrow_targets t ON t.account_id=r.target_id JOIN accounts a ON a.id=t.account_id
 CROSS JOIN ticket_borrow_settings s LEFT JOIN ticket_borrow_sources src ON src.id=r.source_id
 LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 CROSS JOIN LATERAL (SELECT count(*) AS inflight FROM ticket_borrow_route_leases l WHERE l.route_id=r.id AND l.lease_until>clock_timestamp()) load
 WHERE s.id=1 AND r.target_id=$1 AND NOT r.validation_only
 AND ((r.revoked_at IS NULL AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp()) OR load.inflight>0)
 ORDER BY `+borrowRouteLatestOrder, id, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []service.BorrowRouteStatus{}
	for rows.Next() {
		var status service.BorrowRouteStatus
		if err = rows.Scan(&status.ID, &status.Gateway, &status.SourceAccountID, &status.ExpiresAt, &status.UsableUntil, &status.InventoryEligible, &status.Usable, &status.Inflight, &status.PublishedAt, &status.CheckedAt); err != nil {
			return nil, err
		}
		result = append(result, status)
	}
	return result, rows.Err()
}
