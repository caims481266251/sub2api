package repository

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *ticketBorrowRepository) Replenishment(context.Context, int64, string) (*service.BorrowReplenishmentState, error) {
	return nil, nil
}
func (r *ticketBorrowRepository) EndReplenishment(ctx context.Context, a service.BorrowAcquisition, source int64, reason string) error {
	// The same target lock as publication prevents a losing concurrent result
	// from cancelling a newer success window or a different target's priority.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var valid bool
	if err = tx.QueryRowContext(ctx, `SELECT lease_token=$2 AND revision=$3 AND lease_until>clock_timestamp() FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, a.TargetID, a.Lease, a.Revision).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return service.ErrBorrowConflict
	}
	_, err = tx.ExecContext(ctx, `WITH ended AS (UPDATE ticket_borrow_replenishment SET until_at=clock_timestamp(),reason=$3
 WHERE target_id=$1 AND source_id=$2 AND identity_hash=$4 AND reason='success' RETURNING *)
 INSERT INTO ticket_borrow_events(target_id,source_id,stage,reason,details)
 SELECT target_id,source_id,'replenishment','replenishment_ended',jsonb_build_object('replenishment',jsonb_build_object('environment_version',environment_version,'until',until_at,'exit_reason',reason)) FROM ended`, a.TargetID, source, reason, a.Identity)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func borrowReplenishmentPolicy(ctx context.Context, db borrowQueryer, id int64) (*service.BorrowReplenishmentSettings, error) {
	var raw []byte
	if err := db.QueryRowContext(ctx, `SELECT replenishment FROM ticket_borrow_settings WHERE id=1`).Scan(&raw); err != nil {
		return nil, err
	}
	var p service.BorrowReplenishmentSettings
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p.Inventory = service.BorrowRollingInventory
	if !p.Allows(id) {
		p.Inventory = 1
	}
	return &p, nil
}

// Successful environments are never retained by the candidate scheduler.
func armBorrowReplenishment(context.Context, *sql.Tx, *service.BorrowRoute, *service.BorrowReplenishmentSettings) error {
	return nil
}

func trimBorrowInventory(ctx context.Context, tx *sql.Tx, route *service.BorrowRoute, p *service.BorrowReplenishmentSettings) error {
	if p == nil {
		return nil
	}
	// Eligibility is separate from revocation: bound sessions still resolve an
	// older route by ID and existing streams remain untouched.
	_, err := tx.ExecContext(ctx, `WITH retired AS (UPDATE ticket_borrow_routes SET inventory_eligible=false WHERE id IN (
 SELECT r.id FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.target_id=$1 AND r.target_revision=$2 AND r.identity_hash=$3 AND r.inventory_eligible AND r.revoked_at IS NULL
 ORDER BY LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until) DESC,r.checked_at ASC,r.id ASC OFFSET $4) RETURNING *)
 INSERT INTO ticket_borrow_events(target_id,source_id,route_id,stage,reason)
 SELECT target_id,source_id,id,'replenishment','replenishment_inventory_retired' FROM retired`, route.TargetID, route.TargetRevision, route.IdentityHash, p.Inventory)
	return err
}

// Expire invalid runtime state once. Lock targets first, as outcome recording
// does; skip an active publication and reconsider it on the next scheduler tick.
// No rotation/configuration locks are acquired in this transaction.
func (r *ticketBorrowRepository) expireBorrowReplenishment(ctx context.Context) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `WITH locked AS MATERIALIZED (
 SELECT t.account_id FROM ticket_borrow_targets t JOIN ticket_borrow_replenishment w ON w.target_id=t.account_id
 WHERE w.reason='success' AND NOT EXISTS(SELECT 1 FROM ticket_borrow_active_replenishment active WHERE active.target_id=w.target_id)
 ORDER BY t.account_id FOR UPDATE OF t SKIP LOCKED), ended AS (
 UPDATE ticket_borrow_replenishment w SET reason=CASE WHEN until_at<=clock_timestamp() THEN 'hold_expired' ELSE 'configuration_or_health_changed' END,until_at=LEAST(until_at,clock_timestamp())
 FROM locked WHERE w.target_id=locked.account_id AND w.reason='success' RETURNING w.*)
 INSERT INTO ticket_borrow_events(target_id,source_id,stage,reason,details)
 SELECT target_id,source_id,'replenishment','replenishment_ended',jsonb_build_object('replenishment',jsonb_build_object('environment_version',environment_version,'until',until_at,'exit_reason',reason)) FROM ended`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
