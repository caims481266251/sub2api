package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *ticketBorrowRepository) Replenishment(ctx context.Context, id int64, identity string) (*service.BorrowReplenishmentState, error) {
	var s service.BorrowReplenishmentState
	err := r.db.QueryRowContext(ctx, `SELECT source_id,source_account_id,environment_version,until_at,next_at,(replenishment->>'interval_seconds')::int,attempts FROM ticket_borrow_active_replenishment WHERE target_id=$1 AND identity_hash=$2`, id, identity).Scan(&s.SourceID, &s.SourceAccountID, &s.EnvironmentVersion, &s.Until, &s.NextAt, &s.IntervalSeconds, &s.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
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
	if !p.Allows(id) {
		return nil, nil
	}
	return &p, nil
}

// Called under pool -> target locks, in the publication transaction. Hold time
// is fixed for an environment, shared across targets, and never extended by a
// replenishment success. A restart cannot leave an unbounded rotation freeze.
func armBorrowReplenishment(ctx context.Context, tx *sql.Tx, route *service.BorrowRoute, p *service.BorrowReplenishmentSettings) error {
	if p == nil || route.SourceEnvironmentVersion == 0 || route.SourceSubjectHash == "" || route.TargetSubjectHash == "" {
		return nil
	}
	var sourceAccount int64
	var matching bool
	err := tx.QueryRowContext(ctx, `SELECT s.account_id,
 s.revision=$2 AND b.environment_version=$3 AND COALESCE(x.revision,0)=$4 AND x.active_job IS NULL
 AND encode(sha256(convert_to(COALESCE(a.credentials->>'chatgpt_account_id','')||':'||COALESCE(a.credentials->>'chatgpt_user_id',''),'UTF8')),'hex')=$5
 AND EXISTS(SELECT 1 FROM accounts ta WHERE ta.id=$6 AND encode(sha256(convert_to(COALESCE(ta.credentials->>'chatgpt_account_id','')||':'||COALESCE(ta.credentials->>'chatgpt_user_id',''),'UTF8')),'hex')=$7)
 FROM ticket_borrow_sources s JOIN accounts a ON a.id=s.account_id JOIN ticket_borrow_proxy_bindings b ON b.account_id=s.account_id AND b.role='source'
 LEFT JOIN ticket_borrow_rotation_accounts x ON x.account_id=s.account_id CROSS JOIN ticket_borrow_selection c WHERE c.id=1 AND s.id=$1 AND s.enabled AND s.deleted_at IS NULL AND `+borrowNormalScope, route.SourceID, route.SourceRevision, route.SourceEnvironmentVersion, route.SourceRotationRevision, route.SourceSubjectHash, route.TargetID, route.TargetSubjectHash).Scan(&sourceAccount, &matching)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if !matching {
		return nil
	}
	var until time.Time
	err = tx.QueryRowContext(ctx, `INSERT INTO ticket_borrow_replenishment_holds(account_id,environment_version,until_at)
 VALUES($1,$2,clock_timestamp()+$3*INTERVAL '1 second')
 ON CONFLICT(account_id) DO UPDATE SET environment_version=EXCLUDED.environment_version,
 until_at=CASE WHEN ticket_borrow_replenishment_holds.environment_version=EXCLUDED.environment_version THEN ticket_borrow_replenishment_holds.until_at ELSE EXCLUDED.until_at END RETURNING until_at`, sourceAccount, route.SourceEnvironmentVersion, p.HoldSeconds).Scan(&until)
	if err != nil {
		return err
	}
	// An ended window for this target/environment cannot be rearmed by a late
	// concurrent success. A new environment or source can establish a new window.
	_, err = tx.ExecContext(ctx, `WITH started AS (INSERT INTO ticket_borrow_replenishment(target_id,target_revision,identity_hash,source_id,source_revision,environment_version,rotation_revision,settings_revision,selection_revision,policy_revision,source_identity,target_identity,target_config,until_at)
 SELECT $1,$2,$3,$4,$5,$6,$7,cfg.revision,sel.revision,t.probe_policy_revision,
 $11,$12,(SELECT md5(jsonb_build_array(proxy_id,extra->'enable_tls_fingerprint',extra->'tls_fingerprint_profile_id')::text) FROM accounts WHERE id=$1),$8::timestamptz
 FROM ticket_borrow_settings cfg CROSS JOIN ticket_borrow_selection sel JOIN accounts a ON a.id=$9 JOIN ticket_borrow_targets t ON t.account_id=$1 WHERE cfg.id=1 AND sel.id=1 AND $8>clock_timestamp()
 ON CONFLICT(target_id) DO UPDATE SET target_revision=EXCLUDED.target_revision,identity_hash=EXCLUDED.identity_hash,
 source_id=EXCLUDED.source_id,source_revision=EXCLUDED.source_revision,environment_version=EXCLUDED.environment_version,rotation_revision=EXCLUDED.rotation_revision,
 settings_revision=EXCLUDED.settings_revision,selection_revision=EXCLUDED.selection_revision,policy_revision=EXCLUDED.policy_revision,source_identity=EXCLUDED.source_identity,target_identity=EXCLUDED.target_identity,target_config=EXCLUDED.target_config,
 until_at=EXCLUDED.until_at,started_at=clock_timestamp(),next_at=clock_timestamp(),reason='success',attempts=0
 WHERE ticket_borrow_replenishment.source_id<>EXCLUDED.source_id OR ticket_borrow_replenishment.environment_version<>EXCLUDED.environment_version RETURNING *)
 INSERT INTO ticket_borrow_events(target_id,source_id,stage,reason,details)
 SELECT target_id,source_id,'replenishment','replenishment_started',jsonb_build_object('replenishment',jsonb_build_object('environment_version',environment_version,'until',until_at,'inventory',$10::int)) FROM started`, route.TargetID, route.TargetRevision, route.IdentityHash, route.SourceID, route.SourceRevision, route.SourceEnvironmentVersion, route.SourceRotationRevision, until, sourceAccount, p.Inventory, route.SourceSubjectHash, route.TargetSubjectHash)
	return err
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
 ORDER BY LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at)) DESC,r.checked_at ASC,r.id ASC OFFSET $4) RETURNING *)
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
