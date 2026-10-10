package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
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

func sourceRotationEligible(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT c.use_proxy_pool AND a.status='active' AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp()) AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=a.id AND t.enabled)
 FROM accounts a JOIN ticket_borrow_sources s ON s.account_id=a.id CROSS JOIN ticket_borrow_selection c WHERE c.id=1 AND a.id=$1 AND s.deleted_at IS NULL AND `+borrowNormalScope+` FOR UPDATE OF s`, id).Scan(&eligible)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return eligible, err
}
func (r *ticketBorrowRepository) SaveRotationPolicy(ctx context.Context, id int64, p service.BorrowRotationPolicy) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockBorrowRotation(ctx, tx); err != nil {
		return err
	}
	valid, err := sourceRotationEligible(ctx, tx, id)
	if err != nil {
		return err
	}
	if !valid {
		// A removed or disabled source can always have an existing policy turned off.
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_rotation_accounts WHERE account_id=$1 AND role='source')`, id).Scan(&exists); err != nil {
			return err
		}
		if p.Automatic || !exists {
			return service.ErrBorrowRotationInvalid
		}
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_rotation_accounts(account_id,role) VALUES($1,'source') ON CONFLICT DO NOTHING`, id)
	if err != nil {
		return err
	}
	n, _ := inserted.RowsAffected()
	if p.Revision == 0 && n == 0 {
		return service.ErrBorrowConflict
	}
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_rotation_accounts SET role='source',automatic=$2,interval_seconds=$3::integer,revision=revision+1,next_at=CASE WHEN $2 THEN clock_timestamp()+$3::integer*INTERVAL '1 second' ELSE NULL END WHERE account_id=$1 AND revision=$4 AND (active_job IS NULL OR role='source')`, id, p.Automatic, p.IntervalSeconds, max(int64(1), p.Revision))
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowConflict
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) QueueRotation(ctx context.Context, in service.BorrowRotationInput, trigger string) (*service.BorrowRotationJob, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockBorrowRotation(ctx, tx); err != nil {
		return nil, err
	}
	job, err := queueBorrowRotation(ctx, tx, in, trigger)
	if err != nil {
		return nil, err
	}
	return job, tx.Commit()
}
func queueBorrowRotation(ctx context.Context, tx *sql.Tx, in service.BorrowRotationInput, trigger string) (*service.BorrowRotationJob, error) {
	if in.Role != "source" || len(in.AccountIDs) == 0 {
		return nil, service.ErrBorrowRotationInvalid
	}
	ids := append([]int64(nil), in.AccountIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	in.AccountIDs = ids
	job := &service.BorrowRotationJob{ID: in.ID, Role: "source", Trigger: trigger, AccountIDs: ids, Phase: "draining", Reason: "rotation_draining"}
	var raw []byte
	var role string
	err := tx.QueryRowContext(ctx, `SELECT account_ids,role,trigger_kind,phase,reason FROM ticket_borrow_rotation_jobs WHERE id=$1`, in.ID).Scan(&raw, &role, &job.Trigger, &job.Phase, &job.Reason)
	if err == nil {
		var old []int64
		if json.Unmarshal(raw, &old) != nil || role != "source" || fmt.Sprint(old) != fmt.Sprint(ids) {
			return nil, service.ErrBorrowConflict
		}
		return job, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	if trigger == "automatic" {
		var running bool
		if err = tx.QueryRowContext(ctx, `SELECT running FROM ticket_borrow_settings WHERE id=1`).Scan(&running); err != nil {
			return nil, err
		}
		if !running {
			return nil, service.ErrBorrowPaused
		}
	}
	current, versions := map[int64]int64{}, map[int64]int64{}
	for _, id := range ids {
		valid, e := sourceRotationEligible(ctx, tx, id)
		if e != nil {
			return nil, e
		}
		if !valid {
			return nil, service.ErrBorrowRotationInvalid
		}
		var active sql.NullString
		e = tx.QueryRowContext(ctx, `SELECT active_job FROM ticket_borrow_rotation_accounts WHERE account_id=$1`, id).Scan(&active)
		if e != nil && e != sql.ErrNoRows {
			return nil, e
		}
		if active.Valid {
			return nil, service.ErrBorrowRotationPending
		}
		var entry, version int64
		e = tx.QueryRowContext(ctx, `SELECT entry_id,environment_version FROM ticket_borrow_proxy_bindings WHERE account_id=$1 AND role='source'`, id).Scan(&entry, &version)
		if e == sql.ErrNoRows {
			return nil, service.ErrBorrowRotationNoProxy
		}
		if e != nil {
			return nil, e
		}
		current[id], versions[id] = entry, version
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id FROM proxy_pool_entries p WHERE p.enabled AND p.retired_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_proxy_bindings b WHERE b.entry_id=p.id AND NOT(b.account_id=ANY($1)))
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_rotation_assignments x WHERE x.entry_id=p.id) ORDER BY p.id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	entries := []int64{}
	for rows.Next() {
		var entry int64
		if err = rows.Scan(&entry); err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, entry)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	assignments, err := rotationMatching(ctx, ids, entries, current)
	if err != nil {
		return nil, err
	}
	if err = seedBorrowFingerprintHistory(ctx, tx); err != nil {
		return nil, err
	}
	raw, _ = json.Marshal(ids)
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_rotation_jobs(id,role,trigger_kind,account_ids) VALUES($1,'source',$2,$3)`, in.ID, trigger, string(raw)); err != nil {
		return nil, err
	}
	for _, id := range ids {
		_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_rotation_accounts(account_id,role,active_job,phase,reason) VALUES($1,'source',$2,'draining','rotation_draining') ON CONFLICT(account_id) DO UPDATE SET role='source',active_job=$2,phase='draining',reason='rotation_draining'`, id, in.ID)
		if err != nil {
			return nil, err
		}
		device, e := newBorrowFingerprint(ctx, tx, id, assignments[id])
		if e != nil {
			return nil, e
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_rotation_assignments(job_id,account_id,entry_id,previous_entry_id,device_id,environment_version,selection_kind) VALUES($1,$2,$3,$4,$5,$6,$7)`, in.ID, id, assignments[id], current[id], device, versions[id]+1, "random")
		if err != nil {
			return nil, err
		}
	}
	return job, nil
}
func finishBorrowRotation(ctx context.Context, tx *sql.Tx, id, phase, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,source_id,stage,reason,details)
 SELECT CASE WHEN x.role='target' THEN x.account_id ELSE NULL END,CASE WHEN x.role='source' THEN s.id ELSE NULL END,'rotation',$2,
 jsonb_build_object('rotation',jsonb_build_object('job_id',x.active_job,'role',x.role,'trigger',j.trigger_kind,'account_id',x.account_id,'entry_id',COALESCE(b.entry_id,0),'environment_version',COALESCE(b.environment_version,0),'previous_entry_id',COALESCE(b.entry_id,0)))
 FROM ticket_borrow_rotation_accounts x JOIN ticket_borrow_rotation_jobs j ON j.id=x.active_job LEFT JOIN ticket_borrow_sources s ON s.account_id=x.account_id LEFT JOIN ticket_borrow_proxy_bindings b ON b.account_id=x.account_id WHERE x.active_job=$1`, id, reason)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_rotation_jobs SET phase=$2,reason=$3,finished_at=clock_timestamp() WHERE id=$1`, id, phase, reason)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_rotation_accounts SET active_job=NULL,phase=$2,reason=$3,next_at=CASE WHEN automatic THEN clock_timestamp()+interval_seconds*INTERVAL '1 second' ELSE NULL END WHERE active_job=$1`, id, phase, reason)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM ticket_borrow_rotation_assignments WHERE job_id=$1`, id)
	return err
}

func (r *ticketBorrowRepository) ProcessRotations(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM ticket_borrow_rotation_jobs WHERE finished_at IS NULL ORDER BY created_at LIMIT 100`)
	if err != nil {
		return err
	}
	jobs := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range jobs {
		if err = r.processRotation(ctx, id); err != nil {
			return err
		}
	}
	// A due source policy triggers a reshuffle of all eligible bound sources.
	// Including sources without a timer lets a fully occupied pool rotate.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockBorrowRotation(ctx, tx); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT x.account_id FROM ticket_borrow_rotation_accounts x JOIN accounts a ON a.id=x.account_id JOIN ticket_borrow_proxy_bindings b ON b.account_id=x.account_id AND b.role='source' CROSS JOIN ticket_borrow_selection c CROSS JOIN ticket_borrow_settings cfg WHERE cfg.id=1 AND cfg.running AND c.id=1 AND c.use_proxy_pool AND x.role='source' AND x.automatic AND x.active_job IS NULL AND x.next_at<=clock_timestamp() AND a.status='active' AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp()) AND `+borrowNormalScope+` AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=a.id AND t.enabled) ORDER BY x.next_at,x.account_id LIMIT 1000`)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	dueIDs := ids
	rows, err = tx.QueryContext(ctx, `SELECT a.id,COALESCE(x.active_job IS NOT NULL,false)
 FROM accounts a JOIN ticket_borrow_sources s ON s.account_id=a.id AND s.deleted_at IS NULL
 JOIN ticket_borrow_proxy_bindings b ON b.account_id=a.id AND b.role='source'
 LEFT JOIN ticket_borrow_rotation_accounts x ON x.account_id=a.id
 CROSS JOIN ticket_borrow_selection c WHERE c.id=1 AND c.use_proxy_pool
 AND a.status='active' AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND `+borrowNormalScope+` AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets t WHERE t.account_id=a.id AND t.enabled) ORDER BY a.id`)
	if err != nil {
		return err
	}
	ids = nil
	pending := false
	for rows.Next() {
		var id int64
		var active bool
		if err = rows.Scan(&id, &active); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		pending = pending || active
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Wait for an existing manual/automatic batch instead of silently excluding
	// its sources from the all-source reshuffle.
	if pending || len(ids) == 0 {
		return nil
	}
	// A savepoint leaves the pool lock held on a failed plan, so failure status
	// cannot race a successful manual queue or a newly saved policy.
	if _, err = tx.ExecContext(ctx, `SAVEPOINT automatic_rotation`); err != nil {
		return err
	}
	_, err = queueBorrowRotation(ctx, tx, service.BorrowRotationInput{ID: uuid.NewString(), Role: "source", AccountIDs: ids}, "automatic")
	if err != nil {
		reason := err.Error()
		if !errors.Is(err, service.ErrBorrowRotationNoProxy) && !errors.Is(err, service.ErrBorrowRotationInvalid) && !errors.Is(err, service.ErrBorrowRotationNoFingerprint) {
			if errors.Is(err, service.ErrBorrowPaused) {
				return nil
			}
			return err
		}
		if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT automatic_rotation`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_rotation_accounts SET phase='failed',reason=$2,next_at=clock_timestamp()+interval_seconds*INTERVAL '1 second' WHERE account_id=ANY($1) AND active_job IS NULL`, pq.Array(dueIDs), reason)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) processRotation(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockBorrowRotation(ctx, tx); err != nil {
		return err
	}
	var role, trigger string
	var raw []byte
	var expired bool
	err = tx.QueryRowContext(ctx, `SELECT role,trigger_kind,account_ids,deadline<=clock_timestamp() FROM ticket_borrow_rotation_jobs WHERE id=$1 AND finished_at IS NULL FOR UPDATE`, id).Scan(&role, &trigger, &raw, &expired)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var ids []int64
	if err = json.Unmarshal(raw, &ids); err != nil {
		return err
	}
	finish := func(phase, reason string) error {
		if e := finishBorrowRotation(ctx, tx, id, phase, reason); e != nil {
			return e
		}
		return tx.Commit()
	}
	if role != "source" {
		return finish("cancelled", "rotation_source_only")
	}
	if trigger == "automatic" {
		var running bool
		if err = tx.QueryRowContext(ctx, `SELECT running FROM ticket_borrow_settings WHERE id=1`).Scan(&running); err != nil {
			return err
		}
		if !running {
			return finish("cancelled", "borrow_acquisition_paused")
		}
	}
	for _, account := range ids {
		valid, e := sourceRotationEligible(ctx, tx, account)
		if e != nil {
			return e
		}
		if !valid {
			return finish("cancelled", "rotation_configuration_changed")
		}
	}
	if expired {
		return finish("failed", "rotation_drain_timeout")
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_sources WHERE account_id=ANY($1) AND lease_until>clock_timestamp())`, pq.Array(ids)).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return nil
	}
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT count(*)=$2 AND COALESCE(bool_and(p.enabled AND p.retired_at IS NULL AND COALESCE(b.entry_id,0)=x.previous_entry_id AND b.role='source' AND b.environment_version+1=x.environment_version),false) FROM ticket_borrow_rotation_assignments x JOIN proxy_pool_entries p ON p.id=x.entry_id LEFT JOIN ticket_borrow_proxy_bindings b ON b.account_id=x.account_id WHERE x.job_id=$1`, id, len(ids)).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return finish("failed", "rotation_configuration_changed")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM ticket_borrow_proxy_bindings WHERE account_id=ANY($1)`, pq.Array(ids)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_proxy_bindings(account_id,entry_id,device_id,role,environment_version) SELECT account_id,entry_id,device_id,'source',environment_version FROM ticket_borrow_rotation_assignments WHERE job_id=$1`, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_proxy_usage(account_id,entry_id) SELECT account_id,entry_id FROM ticket_borrow_rotation_assignments WHERE job_id=$1 ON CONFLICT(account_id,entry_id) DO UPDATE SET last_used_at=clock_timestamp(),use_count=ticket_borrow_proxy_usage.use_count+1`, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_rotation_accounts a SET last_at=clock_timestamp(),selection_kind=x.selection_kind FROM ticket_borrow_rotation_assignments x WHERE a.account_id=x.account_id AND x.job_id=$1`, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_events(source_id,stage,reason,details) SELECT s.id,'rotation','rotation_environment_changed',jsonb_build_object('rotation',jsonb_build_object('job_id',x.job_id,'role','source','trigger',$2::text,'account_id',x.account_id,'previous_entry_id',x.previous_entry_id,'entry_id',x.entry_id,'environment_version',x.environment_version,'selection_kind',x.selection_kind)) FROM ticket_borrow_rotation_assignments x JOIN ticket_borrow_sources s ON s.account_id=x.account_id WHERE x.job_id=$1`, id, trigger)
	if err != nil {
		return err
	}
	return finish("completed", "rotation_completed")
}
func rotationFingerprint(device string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(device)))[:12]
}
