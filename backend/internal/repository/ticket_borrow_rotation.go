package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func lockBorrowRotation(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT id FROM proxy_pool_state WHERE id=1 FOR UPDATE`)
	return err
}

// Read-only compatibility for environments created before source-only rotation.
// No current API or worker can allocate or rotate a target environment.
func (r *ticketBorrowRepository) TargetEnvironment(ctx context.Context, id int64) (service.BorrowTargetEnvironment, error) {
	var env service.BorrowTargetEnvironment
	var ciphertext string
	var pending, valid bool
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(b.entry_id,0),COALESCE(b.environment_version,0),COALESCE(b.device_id,''),COALESCE(p.credential_ciphertext,''),COALESCE(x.phase='draining' AND x.active_job IS NOT NULL,false),COALESCE(p.enabled AND p.retired_at IS NULL,true)
 FROM ticket_borrow_targets t LEFT JOIN ticket_borrow_proxy_bindings b ON b.account_id=t.account_id AND b.role='target' LEFT JOIN proxy_pool_entries p ON p.id=b.entry_id LEFT JOIN ticket_borrow_rotation_accounts x ON x.account_id=t.account_id WHERE t.account_id=$1`, id).Scan(&env.EntryID, &env.Version, &env.DeviceID, &ciphertext, &pending, &valid)
	if err == sql.ErrNoRows {
		return env, nil
	}
	if err != nil {
		return env, err
	}
	if pending {
		return env, service.ErrBorrowRotationPending
	}
	if !valid {
		return env, service.ErrBorrowUnavailable
	}
	if env.EntryID > 0 {
		env.Proxy, err = r.encryptor.Decrypt(ciphertext)
	}
	return env, err
}
func (r *ticketBorrowRepository) RotationStates(ctx context.Context) ([]service.BorrowRotationState, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT x.account_id,x.role,x.revision,x.automatic,x.interval_seconds,x.next_at,x.last_at,x.phase,x.reason,COALESCE(x.active_job,''),COALESCE(b.entry_id,0),COALESCE(b.environment_version,0),COALESCE(b.device_id,''),COALESCE(p.protocol||'://'||CASE WHEN p.host LIKE '%:%' THEN '['||p.host||']' ELSE p.host END||':'||p.port,''),clock_timestamp(),x.selection_kind
 FROM ticket_borrow_rotation_accounts x JOIN accounts a ON a.id=x.account_id AND a.deleted_at IS NULL LEFT JOIN ticket_borrow_proxy_bindings b ON b.account_id=x.account_id LEFT JOIN proxy_pool_entries p ON p.id=b.entry_id WHERE x.role='source' ORDER BY x.account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.BorrowRotationState{}
	for rows.Next() {
		var v service.BorrowRotationState
		var device string
		if err = rows.Scan(&v.AccountID, &v.Role, &v.Revision, &v.Automatic, &v.IntervalSeconds, &v.NextAt, &v.LastAt, &v.Phase, &v.Reason, &v.ActiveJob, &v.PoolEntryID, &v.EnvironmentVersion, &device, &v.PoolEndpoint, &v.ServerNow, &v.SelectionKind); err != nil {
			return nil, err
		}
		if device != "" {
			v.FingerprintID = rotationFingerprint(device)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Account-bound rotation was replaced by per-candidate allocation. Keep these
// repository entry points inert as well as the retired service/API endpoints.
func (r *ticketBorrowRepository) SaveRotationPolicy(context.Context, int64, service.BorrowRotationPolicy) error {
	return service.ErrBorrowRotationRetired
}
func (r *ticketBorrowRepository) QueueRotation(context.Context, service.BorrowRotationInput, string) (*service.BorrowRotationJob, error) {
	return nil, service.ErrBorrowRotationRetired
}
func (r *ticketBorrowRepository) ProcessRotations(context.Context) error { return nil }

func rotationFingerprint(device string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(device)))[:12]
}
