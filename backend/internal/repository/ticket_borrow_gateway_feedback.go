package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Random environment and source IDs deliberately do not fragment the scope.
const borrowGatewayScope = `md5(jsonb_build_array(t.revision,t.acquisition_identity,t.acquisition_revision,
 t.probe_policy_revision,t.lease_settings_revision,t.lease_account_signature,t.lease_identity)::text)`

func borrowGatewayScopeForLease(ctx context.Context, db borrowQueryer, a service.BorrowAcquisition) (string, error) {
	var scope string
	err := db.QueryRowContext(ctx, `SELECT `+borrowGatewayScope+` FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings cfg
 WHERE cfg.id=1 AND t.account_id=$1 AND t.revision=$2 AND t.lease_token=$3 AND t.lease_identity=$4
 AND t.lease_acquisition_revision=t.acquisition_revision AND t.lease_policy_revision=t.probe_policy_revision
 AND t.lease_settings_revision=cfg.revision`, a.TargetID, a.Revision, a.Lease, a.Identity).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrBorrowConflict
	}
	return scope, err
}

func (r *ticketBorrowRepository) BorrowGatewayPriorities(ctx context.Context, a service.BorrowAcquisition) (map[string]time.Time, error) {
	scope, err := borrowGatewayScopeForLease(ctx, r.db, a)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT gateway,deprioritized_until FROM ticket_borrow_gateway_feedback
 WHERE target_id=$1 AND scope=$2 AND deprioritized_until>clock_timestamp()`, a.TargetID, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var gateway string
		var until time.Time
		if err = rows.Scan(&gateway, &until); err != nil {
			return nil, err
		}
		out[gateway] = until
	}
	return out, rows.Err()
}

func (r *ticketBorrowRepository) RecordBorrowGatewayOutcome(ctx context.Context, a service.BorrowAcquisition, o service.BorrowGatewayOutcome) error {
	if !service.BorrowGatewayFeedbackReason(o.Reason) {
		return nil
	}
	if o.CandidateID == "" || o.CookieHash == "" || o.Gateway == "" || o.StartedAt.IsZero() {
		return errors.New("borrow_gateway_outcome_invalid")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, a.TargetID); err != nil {
		return err
	}
	scope, err := borrowGatewayScopeForLease(ctx, tx, a)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_gateway_outcomes(target_id,scope,gateway,candidate_id,cookie_hash,started_at,reason)
 VALUES($1,$2,$3,$4,$5,$6::timestamptz,$7) ON CONFLICT DO NOTHING`, a.TargetID, scope, o.Gateway, o.CandidateID, o.CookieHash, o.StartedAt, o.Reason)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_gateway_feedback(target_id,scope,gateway) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, a.TargetID, scope, o.Gateway); err != nil {
		return err
	}
	if o.Reason == "target_probe_passed" {
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_gateway_feedback SET reset_after=clock_timestamp(),
 last_success_started_at=GREATEST(last_success_started_at,$4::timestamptz),deprioritized_until='-infinity',updated_at=clock_timestamp()
 WHERE target_id=$1 AND scope=$2 AND gateway=$3`, a.TargetID, scope, o.Gateway, o.StartedAt)
	} else {
		// Do not extend a live demotion, reuse its triggering results after expiry,
		// or let an older in-flight candidate undo a more recent successful seed.
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_gateway_feedback f SET deprioritized_until=clock_timestamp()+INTERVAL '30 seconds',
 reset_after=clock_timestamp()+INTERVAL '30 seconds',updated_at=clock_timestamp()
 WHERE f.target_id=$1 AND f.scope=$2 AND f.gateway=$3 AND f.deprioritized_until<=clock_timestamp()
 AND $4::timestamptz>=f.last_success_started_at
 AND (SELECT count(*) FROM ticket_borrow_gateway_outcomes o WHERE o.target_id=f.target_id AND o.scope=f.scope AND o.gateway=f.gateway
 AND o.reason IN ('target_ticket_changed','target_route_changed') AND o.started_at>=f.last_success_started_at
 AND o.created_at>GREATEST(f.reset_after,clock_timestamp()-INTERVAL '120 seconds'))>=3`, a.TargetID, scope, o.Gateway, o.StartedAt)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

var _ service.BorrowGatewayFeedbackRepository = (*ticketBorrowRepository)(nil)
