package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type ticketBorrowRepository struct {
	db        *sql.DB
	encryptor service.OpenAICredentialEncryptor
}

func NewTicketBorrowRepository(db *sql.DB, encryptor service.OpenAICredentialEncryptor) service.TicketBorrowRepository {
	return &ticketBorrowRepository{db, encryptor}
}
func (r *ticketBorrowRepository) seal(value any) (string, error) {
	if _, err := r.encryptor.InitializeEncryption(); err != nil {
		return "", errors.New("borrow_encryption_unavailable")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return r.encryptor.Encrypt(string(b))
}
func (r *ticketBorrowRepository) open(value string, out any) error {
	b, err := r.encryptor.Decrypt(value)
	if err != nil {
		return errors.New("borrow_encryption_unavailable")
	}
	return json.Unmarshal([]byte(b), out)
}

const borrowSourceColumns = `id,name,model,enabled,revision,rotate_identity,cursor,last_reason,next_attempt_at,secret,subject_hash`

func (r *ticketBorrowRepository) scanSource(row interface{ Scan(...any) error }) (*service.BorrowSource, error) {
	var s service.BorrowSource
	var secret string
	err := row.Scan(&s.ID, &s.Name, &s.Model, &s.Enabled, &s.Revision, &s.RotateIdentity, &s.Cursor, &s.LastReason, &s.NextAttemptAt, &secret, &s.SubjectHash)
	if err != nil {
		return nil, err
	}
	if err = r.open(secret, &s.Secret); err != nil {
		return nil, err
	}
	s.ProxyCount = len(s.Secret.Proxies)
	return &s, nil
}
func (r *ticketBorrowRepository) Sources(ctx context.Context) ([]service.BorrowSource, error) {
	return r.normalSources(ctx, 0)
}
func (r *ticketBorrowRepository) Source(ctx context.Context, id int64) (*service.BorrowSource, error) {
	var ref bool
	if err := r.db.QueryRowContext(ctx, `SELECT account_id IS NOT NULL FROM ticket_borrow_sources WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&ref); err != nil {
		return nil, err
	}
	if ref {
		rows, err := r.normalSources(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(rows) != 1 || !rows[0].Enabled {
			return nil, service.ErrBorrowUnavailable
		}
		return &rows[0], nil
	}
	return r.scanSource(r.db.QueryRowContext(ctx, `SELECT `+borrowSourceColumns+` FROM ticket_borrow_sources WHERE id=$1 AND deleted_at IS NULL`, id))
}
func (r *ticketBorrowRepository) SaveSource(ctx context.Context, s *service.BorrowSource) error {
	secret, err := r.seal(s.Secret)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize imports so duplicate identities cannot create independent refresh owners.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(269001)`); err != nil {
		return err
	}
	subject, _ := s.Secret.Credentials["chatgpt_account_id"].(string)
	user, _ := s.Secret.Credentials["chatgpt_user_id"].(string)
	var duplicate bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE deleted_at IS NULL AND platform='openai' AND credentials->>'chatgpt_account_id'=$1 AND ($2='' OR COALESCE(credentials->>'chatgpt_user_id','')=$2))`, subject, user).Scan(&duplicate)
	if err != nil {
		return err
	}
	if duplicate {
		return errors.New("borrow_source_is_normal_account")
	}
	if s.ID == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO ticket_borrow_sources(name,model,enabled,rotate_identity,secret,subject_hash,subject_id,user_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,revision`, s.Name, s.Model, s.Enabled, s.RotateIdentity, secret, s.SubjectHash, subject, user).Scan(&s.ID, &s.Revision)
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_sources SET name=$2,model=$3,enabled=$4,rotate_identity=$5,secret=$6,revision=revision+1,updated_at=NOW(),next_attempt_at=NOW() WHERE id=$1 AND revision=$7 AND deleted_at IS NULL AND (lease_until IS NULL OR lease_until<=NOW()) AND subject_hash=$8 AND account_id IS NULL`, s.ID, s.Name, s.Model, s.Enabled, s.RotateIdentity, secret, s.Revision, s.SubjectHash)
		if err == nil {
			n, _ := result.RowsAffected()
			if n != 1 {
				return service.ErrBorrowConflict
			}
			s.Revision++
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *ticketBorrowRepository) DeleteSource(ctx context.Context, id, revision int64) error {
	res, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_sources SET deleted_at=NOW(),enabled=false,secret='',revision=revision+1,lease_token='',lease_until=NULL WHERE id=$1 AND revision=$2 AND account_id IS NULL AND deleted_at IS NULL AND (lease_until IS NULL OR lease_until<=NOW())`, id, revision)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowConflict
	}
	return nil
}
func (r *ticketBorrowRepository) RefreshSource(ctx context.Context, id, revision int64, lease string, secret service.BorrowSourceSecret) error {
	sealed, err := r.seal(secret)
	if err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_sources SET secret=$4,revision=revision+1,updated_at=NOW() WHERE id=$1 AND revision=$2 AND lease_token=$3 AND lease_until>NOW() AND deleted_at IS NULL AND account_id IS NULL`, id, revision, lease, sealed)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowConflict
	}
	return nil
}
func (r *ticketBorrowRepository) Settings(ctx context.Context) (service.BorrowSettings, error) {
	var s service.BorrowSettings
	var recovery, replenishment []byte
	err := r.db.QueryRowContext(ctx, `SELECT revision,running,ttl_seconds,prewarm_seconds,recovery,min_route_remaining_seconds,probe_concurrency,renew_before_seconds,continue_timeout_seconds,replenishment,admission_window_seconds FROM ticket_borrow_settings WHERE id=1`).Scan(&s.Revision, &s.Running, &s.TTLSeconds, &s.PrewarmSeconds, &recovery, &s.MinRouteRemainingSeconds, &s.ProbeConcurrency, &s.RenewBeforeSeconds, &s.ContinueTimeoutSeconds, &replenishment, &s.AdmissionWindowSeconds)
	if err == nil {
		err = json.Unmarshal(recovery, &s.Recovery)
		if err == nil {
			err = json.Unmarshal(replenishment, &s.Replenishment)
		}
		service.NormalizeBorrowSettings(&s)
		if err == nil {
			s.SourceBudget, s.ServerNow, err = r.sourceBudgetStatus(ctx, s.Recovery.SourceRequestsPerMinute)
		}
	}
	return s, err
}
func (r *ticketBorrowRepository) SaveSettings(ctx context.Context, s service.BorrowSettings) error {
	previous, err := r.Settings(ctx)
	if err != nil {
		return err
	}
	if err = service.ResolveBorrowAdmissionSettings(&s, previous); err != nil {
		return err
	}
	service.NormalizeBorrowSettings(&s)
	if err := s.Replenishment.Validate(); err != nil {
		return err
	}
	var replenishment any
	if s.Replenishment != nil {
		policy := *s.Replenishment
		raw, err := json.Marshal(policy)
		if err != nil {
			return err
		}
		replenishment = string(raw)
	}
	var recovery any
	if s.Recovery != nil {
		raw, err := json.Marshal(s.Recovery)
		if err != nil {
			return err
		}
		recovery = string(raw)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Reservations lock a target before settings; use the same order when
	// changing limits. Never clear upstream restrictions with a budget edit.
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets ORDER BY account_id FOR UPDATE`); err != nil {
		return err
	}
	if s.Replenishment != nil {
		var saved []byte
		if err = tx.QueryRowContext(ctx, `SELECT replenishment FROM ticket_borrow_settings WHERE id=1 FOR UPDATE`).Scan(&saved); err != nil {
			return err
		}
		var previous service.BorrowReplenishmentSettings
		if err = json.Unmarshal(saved, &previous); err != nil {
			return err
		}

		if err = s.Replenishment.Validate(); err != nil {
			return err
		}
		raw, e := json.Marshal(s.Replenishment)
		if e != nil {
			return e
		}
		replenishment = string(raw)
	}
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_settings SET running=$1,ttl_seconds=$2,prewarm_seconds=$3,revision=revision+1,recovery=COALESCE($5::jsonb,recovery),min_route_remaining_seconds=COALESCE($6,min_route_remaining_seconds),probe_concurrency=COALESCE($7,probe_concurrency),renew_before_seconds=COALESCE($8,renew_before_seconds),continue_timeout_seconds=COALESCE($9,continue_timeout_seconds),replenishment=COALESCE($10::jsonb,replenishment),admission_window_seconds=$11 WHERE id=1 AND revision=$4`, s.Running, s.TTLSeconds, s.PrewarmSeconds, s.Revision, recovery, s.MinRouteRemainingSeconds, s.ProbeConcurrency, s.RenewBeforeSeconds, s.ContinueTimeoutSeconds, replenishment, s.AdmissionWindowSeconds)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowConflict
	}
	// Shrinks are permanent for existing candidates and routes; later increases
	// apply only to new candidates. Unknown legacy starts retain their old cutoff.
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_environments SET admission_until=LEAST(admission_until,started_at+$1*INTERVAL '1 second') WHERE started_at IS NOT NULL`, *s.AdmissionWindowSeconds); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_routes SET admission_until=LEAST(admission_until,acquisition_started_at+$1*INTERVAL '1 second') WHERE acquisition_started_at IS NOT NULL`, *s.AdmissionWindowSeconds); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET budget_wait_until=NULL,refresh_generation=refresh_generation+1,next_probe_at=clock_timestamp(),next_attempt_at=clock_timestamp()`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_sources SET next_attempt_at=clock_timestamp() WHERE last_reason='source_request_budget'`); err != nil {
		return err
	}
	// Shrinking inventory retires excess rows immediately, without revoking streams or bindings.
	if _, err = tx.ExecContext(ctx, `WITH ranked AS (
 SELECT r.id,row_number() OVER(PARTITION BY r.target_id,r.target_revision,r.identity_hash ORDER BY LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until) DESC,r.checked_at ASC,r.id ASC) n
 FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.inventory_eligible AND NOT r.validation_only AND r.revoked_at IS NULL)
 UPDATE ticket_borrow_routes r SET inventory_eligible=false FROM ranked,ticket_borrow_settings cfg WHERE cfg.id=1 AND r.id=ranked.id AND ranked.n>CASE WHEN (cfg.replenishment->>'enabled')::boolean THEN 2 ELSE 1 END`); err != nil {
		return err
	}
	return tx.Commit()
}

var borrowTargetSelect = `SELECT a.id,a.name,COALESCE(t.enabled,false),COALESCE(t.revision,0),COALESCE(t.cursor,0),COALESCE(t.refresh_requested,false),COALESCE(t.scan_sources,'[]'::jsonb),COALESCE(t.scan_offset,0),COALESCE(t.phase,'waiting'),COALESCE(t.last_reason,''),COALESCE(t.next_attempt_at,NOW()),clock_timestamp(),t.upstream_wait_until,COALESCE(t.cooldown_seconds,0),COALESCE(t.recovery_level,0),COALESCE(t.consecutive_failures,0),COALESCE(t.recovery_epoch,0),t.last_request_started_at,t.last_request_finished_at,
 (` + strings.ReplaceAll(borrowBackgroundInflight, "$1", "a.id") + `),
 (SELECT count(*) FROM ticket_borrow_requests x WHERE x.target_id=a.id AND x.kind='probe' AND x.reserved_at>clock_timestamp()-INTERVAL '60 seconds'),t.upstream_wait_until,COALESCE(t.upstream_wait_reason,''),t.budget_wait_until,COALESCE(t.probe_policy_revision,0),CASE WHEN t.probe_policy_override THEN t.probe_concurrency ELSE s.probe_concurrency END,` + borrowRenewalLead + `,NOT COALESCE(t.probe_policy_override,false),s.probe_concurrency,` + borrowRenewalLead + `,COALESCE(t.acquisition_identity,'self'),COALESCE(t.acquisition_revision,0) FROM accounts a LEFT JOIN ticket_borrow_targets t ON t.account_id=a.id JOIN ticket_borrow_settings s ON s.id=1 `

func scanBorrowTarget(row interface{ Scan(...any) error }) (service.BorrowTarget, error) {
	var t service.BorrowTarget
	var sources []byte
	err := row.Scan(&t.AccountID, &t.Name, &t.Enabled, &t.Revision, &t.Cursor, &t.RefreshRequested, &sources, &t.ScanOffset, &t.Phase, &t.Reason, &t.NextAttemptAt, &t.ServerNow, &t.CooldownUntil, &t.CooldownSeconds, &t.RecoveryLevel, &t.ConsecutiveFailures, &t.RecoveryEpoch, &t.LastRequestStartedAt, &t.LastRequestFinishedAt, &t.Inflight, &t.ProbeRequestsLastMinute, &t.UpstreamWaitUntil, &t.UpstreamWaitReason, &t.BudgetWaitUntil, &t.ProbePolicy.Revision, &t.ProbePolicy.Concurrency, &t.ProbePolicy.RenewBeforeSeconds, &t.ProbePolicy.UseGlobal, &t.ProbePolicy.GlobalConcurrency, &t.ProbePolicy.GlobalRenewBeforeSeconds, &t.AcquisitionIdentity, &t.AcquisitionRevision)
	if err == nil {
		err = json.Unmarshal(sources, &t.ScanSources)
		t.ScanTotal = len(t.ScanSources)
	}
	return t, err
}
func (r *ticketBorrowRepository) Target(ctx context.Context, id int64) (service.BorrowTarget, error) {
	return scanBorrowTarget(r.db.QueryRowContext(ctx, borrowTargetSelect+`WHERE a.id=(SELECT COALESCE(parent_account_id,id) FROM accounts WHERE id=$1 AND deleted_at IS NULL) AND a.deleted_at IS NULL`, id))
}
func (r *ticketBorrowRepository) Targets(ctx context.Context) ([]service.BorrowTarget, error) {
	rows, err := r.db.QueryContext(ctx, borrowTargetSelect+`WHERE t.enabled AND a.deleted_at IS NULL ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.BorrowTarget{}
	for rows.Next() {
		t, e := scanBorrowTarget(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (r *ticketBorrowRepository) SetTarget(ctx context.Context, id int64, enabled bool, revision int64) error {
	return r.setTarget(ctx, id, enabled, revision, 0, "")
}
func (r *ticketBorrowRepository) SetTargetIdentity(ctx context.Context, id int64, enabled bool, revision, acquisitionRevision int64, identity string) error {
	if identity != service.BorrowAcquisitionSelf && identity != service.BorrowAcquisitionSource {
		return service.ErrBorrowAcquisitionIdentity
	}
	return r.setTarget(ctx, id, enabled, revision, acquisitionRevision, identity)
}
func (r *ticketBorrowRepository) setTarget(ctx context.Context, id int64, enabled bool, revision, acquisitionRevision int64, identity string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(269003)`); err != nil {
		return err
	}
	if enabled {
		var raw string
		e := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='astra_routing_experiment_v1'`).Scan(&raw)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e == nil {
			var old config.AstraRoutingSettings
			if e = json.Unmarshal([]byte(raw), &old); e != nil {
				return e
			}
			if old.CookiePool.Enabled {
				for _, other := range append(old.CookiePool.SourceAccountIDs, old.CookiePool.TargetAccountIDs...) {
					if other == id {
						return errors.New("borrow_legacy_config_conflict")
					}
				}
			}
			if old.WSSession.Enabled {
				for _, other := range old.WSSession.AccountIDs {
					if other == id {
						return errors.New("borrow_legacy_config_conflict")
					}
				}
			}
		}
	}
	var allowed bool
	if err = tx.QueryRowContext(ctx, `SELECT platform='openai' AND type IN ('oauth','setup-token') AND parent_account_id IS NULL FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return errors.New("borrow_target_unsupported")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_targets(account_id) VALUES($1) ON CONFLICT DO NOTHING`, id)
	if err != nil {
		return err
	}
	var old bool
	var version, identityRevision int64
	var oldIdentity string
	if err = tx.QueryRowContext(ctx, `SELECT enabled,revision,acquisition_identity,acquisition_revision FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, id).Scan(&old, &version, &oldIdentity, &identityRevision); err != nil {
		return err
	}
	if revision != version && !(revision == 0 && version == 1) {
		return service.ErrBorrowConflict
	}
	if identity != "" && acquisitionRevision != identityRevision && !(acquisitionRevision == 0 && revision == 0 && version == 1 && identityRevision == 1) {
		return service.ErrBorrowConflict
	}
	if identity != "" && identity != oldIdentity {
		if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET acquisition_identity=$2,acquisition_revision=acquisition_revision+1,lease_token='',lease_until=NULL,refresh_generation=refresh_generation+1,next_probe_at=clock_timestamp(),next_attempt_at=clock_timestamp() WHERE account_id=$1`, id, identity); err != nil {
			return err
		}
	}
	if old != enabled {
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET enabled=$2,revision=revision+1,phase='waiting',last_reason='',next_attempt_at=NOW(),lease_token='',lease_until=NULL,updated_at=NOW() WHERE account_id=$1`, id, enabled)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
func borrowJobTable(kind string) (string, string, error) {
	switch kind {
	case "target":
		return "ticket_borrow_targets", "account_id", nil
	case "source":
		return "ticket_borrow_sources", "id", nil
	}
	return "", "", errors.New("invalid_borrow_job")
}
func (r *ticketBorrowRepository) AcquireJob(ctx context.Context, kind string, id int64, token string) (bool, error) {
	table, key, err := borrowJobTable(kind)
	if err != nil {
		return false, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	extra := ""
	leaseDuration := 180 * time.Second // Source HTTP ownership, separate from target tasks.
	var inventory *service.BorrowInventory
	var acquisition service.BorrowAcquisition
	if kind == "target" {
		leaseDuration = service.BorrowTargetLeaseTTL
		// Lock/reap before eligibility so orphaned requests conservatively extend silence.
		if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, id); err != nil {
			return false, err
		}
		if err = reapBorrowRequests(ctx, tx, id); err != nil {
			return false, err
		}
		a, _ := service.BorrowAcquisitionFromContext(ctx)
		a.TargetID, a.Lease = id, ""
		acquisition = a
		if err = checkBorrowAcquisition(ctx, tx, a, true); err != nil {
			if errors.Is(err, service.ErrBorrowRouteReady) || errors.Is(err, service.ErrBorrowRecoveryWaiting) || errors.Is(err, service.ErrBorrowRequestBudget) || errors.Is(err, service.ErrBorrowPaused) {
				return false, nil
			}
			return false, err
		}
		inventory, err = borrowInventory(ctx, tx, id, a.Identity)
		if err != nil {
			return false, err
		}
		if inventory.BlockedReason != "" || inventory.NextAt.After(inventory.ServerNow) {
			return false, nil
		}
		extra = ",phase='acquiring',refresh_requested=false,cooldown_seconds=0"
	}
	suffix := " AND (SELECT running FROM ticket_borrow_settings WHERE id=1)"
	if kind == "target" {
		suffix += " AND COALESCE(upstream_wait_until<=clock_timestamp(),true) AND COALESCE(budget_wait_until<=clock_timestamp(),true) AND (" + borrowBackgroundInflight + ")=0"
	}
	if kind == "source" {
		suffix += " AND next_attempt_at<=NOW() AND deleted_at IS NULL"
	}
	res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET lease_token=$2,lease_until=clock_timestamp()+$3*INTERVAL '1 millisecond',cursor=cursor+1`+extra+` WHERE `+key+`=$1 AND enabled AND (lease_until IS NULL OR lease_until<=clock_timestamp())`+suffix, id, token, leaseDuration.Milliseconds())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 && kind == "target" {
		_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets t SET last_batch_started_at=clock_timestamp(),last_inventory_state=$3,
 lease_generation=refresh_generation,processed_generation=refresh_generation,lease_settings_revision=cfg.revision,lease_policy_revision=t.probe_policy_revision,lease_acquisition_revision=t.acquisition_revision,
 lease_identity=$4,lease_account_signature=`+borrowAccountSignature+`,next_probe_at=clock_timestamp(),next_attempt_at=clock_timestamp()
 FROM ticket_borrow_settings cfg,accounts a WHERE cfg.id=1 AND a.id=t.account_id AND t.account_id=$1 AND t.lease_token=$2`, id, token, inventory.State, acquisition.Identity)
		if err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}
func (r *ticketBorrowRepository) FinishJob(ctx context.Context, kind string, id int64, token, reason string, delay time.Duration) error {
	table, key, err := borrowJobTable(kind)
	if err != nil {
		return err
	}
	if kind == "target" {
		tx, e := r.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		var identity string
		e = tx.QueryRowContext(ctx, `SELECT lease_identity FROM ticket_borrow_targets WHERE account_id=$1 AND lease_token=$2 FOR UPDATE`, id, token).Scan(&identity)
		if e == sql.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET lease_until=NULL,lease_token='',last_reason=$3,
 processed_generation=GREATEST(processed_generation,lease_generation),
 next_attempt_at=CASE WHEN refresh_generation>lease_generation THEN clock_timestamp() ELSE clock_timestamp()+$4*INTERVAL '1 millisecond' END,
 phase=CASE WHEN upstream_wait_until>clock_timestamp() THEN 'upstream_waiting' WHEN budget_wait_until>clock_timestamp() THEN 'budget_waiting' ELSE 'waiting' END,updated_at=clock_timestamp()
 WHERE account_id=$1 AND lease_token=$2`, id, token, reason, delay.Milliseconds())
		if e != nil {
			return e
		}
		inv, e := borrowInventory(ctx, tx, id, identity)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET next_probe_at=$2 WHERE account_id=$1`, id, inv.NextAt); e != nil {
			return e
		}
		return tx.Commit()
	}
	_, err = r.db.ExecContext(ctx, `UPDATE `+table+` SET lease_until=NULL,lease_token='',last_reason=$3,next_attempt_at=NOW()+$4*INTERVAL '1 millisecond',updated_at=NOW() WHERE `+key+`=$1 AND lease_token=$2`, id, token, reason, delay.Milliseconds())
	return err
}

func (r *ticketBorrowRepository) RefreshTarget(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_targets SET next_attempt_at=NOW(),next_probe_at=NOW(),refresh_requested=true,refresh_generation=refresh_generation+1 WHERE account_id=$1 AND enabled`, id)
	return err
}
func (r *ticketBorrowRepository) Now(ctx context.Context) (time.Time, error) {
	var now time.Time
	err := r.db.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now)
	return now, err
}
func (r *ticketBorrowRepository) ObserveCookie(ctx context.Context, hash string, ttl time.Duration) (time.Time, error) {
	var expires time.Time
	err := r.db.QueryRowContext(ctx, `INSERT INTO ticket_borrow_cookie_deadlines(cookie_hash,expires_at) VALUES($1,clock_timestamp()+$2*INTERVAL '1 millisecond')
 ON CONFLICT(cookie_hash) DO UPDATE SET expires_at=EXCLUDED.expires_at
 WHERE EXCLUDED.expires_at<ticket_borrow_cookie_deadlines.expires_at RETURNING expires_at`, hash, ttl.Milliseconds()).Scan(&expires)
	if err == sql.ErrNoRows {
		err = r.db.QueryRowContext(ctx, `SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=$1`, hash).Scan(&expires)
		return expires, err
	}
	if err == nil {
		_, err = r.db.ExecContext(ctx, `UPDATE ticket_borrow_targets SET refresh_generation=refresh_generation+1,next_probe_at=clock_timestamp(),next_attempt_at=clock_timestamp()
 WHERE account_id IN(SELECT target_id FROM ticket_borrow_routes WHERE cookie_hash=$1 AND revoked_at IS NULL AND inventory_eligible AND NOT validation_only)`, hash)
	}
	return expires, err
}

func (r *ticketBorrowRepository) Publish(ctx context.Context, route *service.BorrowRoute, lease string) error {
	if route.Shadow {
		return errors.New("borrow_shadow_unsupported")
	}
	secret, err := r.seal(route.Secret)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, route.TargetID); err != nil {
		return err
	}
	policy, err := borrowReplenishmentPolicy(ctx, tx, route.TargetID)
	if err != nil {
		return err
	}
	if err = checkBorrowAcquisition(ctx, tx, service.BorrowAcquisition{TargetID: route.TargetID, Revision: route.TargetRevision, Lease: lease, Identity: route.IdentityHash}, false); err != nil {
		return service.ErrBorrowConflict
	}
	if route.CandidateID != "" {
		var pooled bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool WHERE id=$1)`, route.CandidateID).Scan(&pooled); err != nil {
			return err
		}
		if pooled {
			// Target -> pool is also the claim lock order. Keep the ownership
			// fence through commit so a late result cannot publish after cleanup.
			if err = lockBorrowCandidatePool(ctx, tx); err != nil {
				return err
			}
			var valid bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool p
 JOIN ticket_borrow_candidate_environments e ON e.id=p.id CROSS JOIN ticket_borrow_settings cfg
 WHERE cfg.id=1 AND p.settings_revision=cfg.revision AND p.id=$1 AND p.state='claimed'
 AND p.target_id=$2 AND p.target_revision=$3 AND p.identity_hash=$4 AND p.claim_token=$5
 AND p.cookie_hash=$6 AND p.lease_until>clock_timestamp() AND e.released_at IS NULL
 AND p.admission_until>clock_timestamp() AND e.admission_until>clock_timestamp())`, route.CandidateID, route.TargetID, route.TargetRevision, route.IdentityHash, lease, route.CookieHash).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return service.ErrBorrowConflict
			}
		}
	}
	if route.AcquisitionStartedAt == nil || route.AcquisitionStartedAt.IsZero() || route.AdmissionUntil.IsZero() {
		return service.ErrBorrowCandidateExpiry
	}
	if route.SourceCredentialHash != "" {
		var eligible bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts a
 CROSS JOIN ticket_borrow_selection c JOIN ticket_borrow_targets t ON t.account_id=$1
 LEFT JOIN ticket_borrow_sources src ON src.id=NULLIF($2,0)
 WHERE c.id=1 AND a.id=$3 AND t.acquisition_identity=$4
 AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>clock_timestamp())
 AND COALESCE(a.rate_limit_reset_at<=clock_timestamp(),true) AND COALESCE(a.overload_until<=clock_timestamp(),true) AND COALESCE(a.temp_unschedulable_until<=clock_timestamp(),true)
 AND encode(sha256(convert_to(COALESCE(a.credentials->>'access_token',''),'UTF8')),'hex')=$5
 AND encode(sha256(convert_to(COALESCE(a.credentials->>'chatgpt_account_id','')||':'||COALESCE(a.credentials->>'chatgpt_user_id',''),'UTF8')),'hex')=$6
 AND (($4='self' AND $2=0 AND a.id=t.account_id) OR ($4='source' AND src.enabled AND src.deleted_at IS NULL
 AND src.account_id=a.id AND src.revision=$7 AND `+borrowNormalScope+`
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_targets st WHERE st.account_id=a.id AND st.enabled))))`, route.TargetID, route.SourceID, route.AcquisitionAccountID, route.AcquisitionIdentity, route.SourceCredentialHash, route.SourceSubjectHash, route.SourceRevision).Scan(&eligible)
		if err != nil {
			return err
		}
		if !eligible {
			return service.ErrBorrowConflict
		}
	}
	var now time.Time
	if err = tx.QueryRowContext(ctx, `SELECT clock_timestamp(),LEAST($1::timestamptz,$2::timestamptz,
 $3::timestamptz+admission_window_seconds*INTERVAL '1 second',
 COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=$4),$1::timestamptz))
 FROM ticket_borrow_settings WHERE id=1`, route.ExpiresAt, route.AdmissionUntil, route.AcquisitionStartedAt, route.CookieHash).Scan(&now, &route.AdmissionUntil); err != nil {
		return err
	}
	if !now.Before(route.AdmissionUntil) {
		return service.ErrBorrowCandidateExpiry
	}
	// Measure before inserting this publication, which becomes the next search's
	// anchor. Duplicate/failed publication rolls this update back atomically.
	if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets t SET acquisition_duration_ms=
 CASE WHEN acquisition_duration_ms=0 THEN `+borrowSearchElapsedMS+`
 ELSE GREATEST(`+borrowSearchElapsedMS+`,(acquisition_duration_ms*3+`+borrowSearchElapsedMS+`)/4) END
 WHERE account_id=$1`, route.TargetID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ticket_borrow_routes(id,target_id,target_revision,source_id,identity_hash,cookie_hash,gateway,secret,expires_at,inventory_eligible,validation_only,checked_at,
 acquisition_identity,acquisition_account_id,acquisition_started_at,admission_until,candidate_id)
 VALUES($1,$2,$3,NULLIF($4,0),$5,$6,$7,$8,$9,true,false,clock_timestamp(),$10,$11,$12,$13,$14)
 ON CONFLICT(target_id,target_revision,identity_hash,cookie_hash) DO NOTHING`, route.ID, route.TargetID, route.TargetRevision, route.SourceID, route.IdentityHash, route.CookieHash, route.Gateway, secret, route.ExpiresAt,
		route.AcquisitionIdentity, route.AcquisitionAccountID, route.AcquisitionStartedAt, route.AdmissionUntil, route.CandidateID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowCandidateDuplicate
	}
	if route.CandidateID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_candidate_results(target_id,target_revision,identity_hash,cookie_hash,candidate_id,batch_id,verdict,publication,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,'healthy','target_probe_passed',$7) ON CONFLICT(target_id,target_revision,identity_hash,cookie_hash) DO NOTHING`, route.TargetID, route.TargetRevision, route.IdentityHash, route.CookieHash, route.CandidateID, lease, route.ExpiresAt); err != nil {
			return err
		}
	}
	if err = trimBorrowInventory(ctx, tx, route, policy); err != nil {
		return err
	}
	if err = armBorrowReplenishment(ctx, tx, route, policy); err != nil {
		return err
	}
	return tx.Commit()
}

// Eligibility checks remain read-only. Business requests reserve atomically.
func (r *ticketBorrowRepository) Route(ctx context.Context, id int64, identity, preferred string) (*service.BorrowRoute, error) {
	return r.readRoute(ctx, r.db, id, identity, preferred)
}
func (r *ticketBorrowRepository) Revoke(ctx context.Context, id int64, routeID, reason string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT account_id FROM ticket_borrow_targets WHERE account_id=$1 FOR UPDATE`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE ticket_borrow_routes SET revoked_at=clock_timestamp(),reason=$3 WHERE target_id=$1 AND id=$2 AND revoked_at IS NULL`, id, routeID, reason)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_targets SET refresh_generation=refresh_generation+1,next_probe_at=clock_timestamp(),next_attempt_at=clock_timestamp() WHERE account_id=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) Event(ctx context.Context, e service.BorrowEvent) error {
	if e.Stage == "business" {
		return nil
	}
	details := []byte("{}")
	if e.Details != nil {
		var err error
		details, err = json.Marshal(e.Details)
		if err != nil {
			return err
		}
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,source_id,route_id,stage,reason,details) VALUES($1,$2,$3,$4,$5,$6)`, e.TargetID, e.SourceID, e.RouteID, e.Stage, e.Reason, details)
	return err
}
func (r *ticketBorrowRepository) Events(ctx context.Context, before, target int64) ([]service.BorrowEvent, error) {
	return r.QueryEvents(ctx, service.BorrowEventQuery{Before: before, TargetID: target})
}

var _ service.TicketBorrowRepository = (*ticketBorrowRepository)(nil)

func (r *ticketBorrowRepository) LookupSession(ctx context.Context, id int64, hash string) (string, bool, error) {
	var route string
	err := r.db.QueryRowContext(ctx, `SELECT route_id FROM ticket_borrow_sessions WHERE target_id=$1 AND session_hash=$2`, id, hash).Scan(&route)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return route, err == nil, err
}
func (r *ticketBorrowRepository) BindSession(ctx context.Context, id int64, hash, route string) (string, error) {
	var bound string
	err := r.db.QueryRowContext(ctx, `INSERT INTO ticket_borrow_sessions(target_id,session_hash,route_id) VALUES($1,$2,$3) ON CONFLICT(target_id,session_hash) DO UPDATE SET session_hash=EXCLUDED.session_hash RETURNING route_id`, id, hash, route).Scan(&bound)
	return bound, err
}
func (r *ticketBorrowRepository) CandidateKnown(ctx context.Context, id int64, identity, cookie string) (bool, error) {
	var known bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_rejections WHERE target_id=$1 AND identity_hash=$2 AND cookie_hash=$3 AND expires_at>NOW()) OR EXISTS(SELECT 1 FROM ticket_borrow_routes WHERE target_id=$1 AND identity_hash=$2 AND cookie_hash=$3 AND expires_at>NOW()) OR EXISTS(SELECT 1 FROM ticket_borrow_candidate_results WHERE target_id=$1 AND identity_hash=$2 AND cookie_hash=$3 AND expires_at>NOW())`, id, identity, cookie).Scan(&known)
	return known, err
}
func (r *ticketBorrowRepository) RejectCandidate(ctx context.Context, id int64, identity, cookie string, expires time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_rejections(target_id,identity_hash,cookie_hash,expires_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id, identity, cookie, expires)
	return err
}

// CleanupEvents only expires borrowing history, never business usage or request captures.
func (r *ticketBorrowRepository) CleanupEvents(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM ticket_borrow_events WHERE created_at<NOW()-INTERVAL '24 hours'`)
	return err
}

func (r *ticketBorrowRepository) Cleanup(ctx context.Context) error {
	if err := r.CleanupEvents(ctx); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM ticket_borrow_source_attempts WHERE reserved_at<clock_timestamp()-INTERVAL '7 days'`); err != nil {
		return err
	}
	// Keep other diagnostic history and continuation tombstones as before.
	for _, query := range []string{`DELETE FROM ticket_borrow_gateway_outcomes WHERE created_at<clock_timestamp()-INTERVAL '7 days'`, `DELETE FROM ticket_borrow_gateway_feedback WHERE updated_at<clock_timestamp()-INTERVAL '7 days'`, `DELETE FROM ticket_borrow_candidate_environments WHERE lease_until<clock_timestamp()-INTERVAL '7 days'`, `DELETE FROM ticket_borrow_source_intents WHERE expires_at<=clock_timestamp()`, `DELETE FROM ticket_borrow_candidate_results WHERE expires_at<clock_timestamp()-INTERVAL '7 days'`, `DELETE FROM ticket_borrow_route_leases WHERE lease_until<=clock_timestamp()`, `DELETE FROM ticket_borrow_rotation_jobs WHERE finished_at<clock_timestamp()-INTERVAL '7 days' AND NOT EXISTS(SELECT 1 FROM ticket_borrow_rotation_accounts x WHERE x.active_job=ticket_borrow_rotation_jobs.id)`, `DELETE FROM ticket_borrow_requests WHERE finished_at<clock_timestamp()-INTERVAL '7 days'`, `DELETE FROM ticket_borrow_routes WHERE expires_at<NOW()-INTERVAL '1 day'`, `DELETE FROM ticket_borrow_rejections WHERE expires_at<NOW()`, `DELETE FROM ticket_borrow_cookie_deadlines WHERE observed_at<NOW()-INTERVAL '30 days'`, `DELETE FROM ticket_borrow_compatibility WHERE updated_at<clock_timestamp()-INTERVAL '7 days'`} {
		if _, err := r.db.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	// Pool payloads are cleared at completion. Keep the ordinary seven-day
	// audit horizon and wait until older referenced HTTP/environment rows are gone.
	_, err := r.db.ExecContext(ctx, `DELETE FROM ticket_borrow_candidate_pool p
 WHERE p.finished_at<clock_timestamp()-INTERVAL '7 days'
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_candidate_environments e WHERE e.pool_candidate_id=p.id)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_source_attempts q WHERE q.pool_candidate_id=p.id)
 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_source_intents i WHERE i.pool_candidate_id=p.id)`)
	if err != nil {
		return err
	}
	return nil
}

func (r *ticketBorrowRepository) TransferSource(ctx context.Context, a *service.Account, s *service.BorrowSource) error {
	sealed, err := r.seal(s.Secret)
	if err != nil {
		return err
	}
	expected, err := json.Marshal(a.Credentials)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(269001)`); err != nil {
		return err
	}
	var matches bool
	err = tx.QueryRowContext(ctx, `SELECT credentials=$2::jsonb AND parent_account_id IS NULL AND platform='openai' AND type IN ('oauth','setup-token') FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, a.ID, string(expected)).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return service.ErrBorrowConflict
	}
	var busy bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM openai_oauth_reauth_tasks WHERE account_id=$1 AND status IN ('queued','running','callback_processing')) OR EXISTS(SELECT 1 FROM ticket_borrow_targets WHERE account_id=$1 AND enabled) OR EXISTS(SELECT 1 FROM accounts WHERE parent_account_id=$1 AND deleted_at IS NULL)`, a.ID).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return service.ErrBorrowConflict
	}
	// History and group references retain their original account ID.
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=NOW(),schedulable=false,credentials='{}'::jsonb,updated_at=NOW() WHERE id=$1`, a.ID); err != nil {
		return err
	}
	subject, _ := s.Secret.Credentials["chatgpt_account_id"].(string)
	user, _ := s.Secret.Credentials["chatgpt_user_id"].(string)
	err = tx.QueryRowContext(ctx, `INSERT INTO ticket_borrow_sources(name,model,secret,subject_hash,subject_id,user_id) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,revision`, s.Name, s.Model, sealed, s.SubjectHash, subject, user).Scan(&s.ID, &s.Revision)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_transfers(account_id,source_id) VALUES($1,$2)`, a.ID, s.ID); err != nil {
		return err
	}
	if err = enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &a.ID, nil, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) SetPhase(ctx context.Context, id int64, lease, phase string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_targets SET phase=$3 WHERE account_id=$1 AND lease_token=$2 AND lease_until>NOW()`, id, lease, phase)
	return err
}

func (r *ticketBorrowRepository) ScanTarget(ctx context.Context, id int64, cancel bool) error {
	var res sql.Result
	var err error
	if cancel {
		res, err = r.db.ExecContext(ctx, `UPDATE ticket_borrow_targets SET scan_sources='[]'::jsonb,scan_offset=0 WHERE account_id=$1 AND enabled`, id)
	} else {
		sources, e := r.Sources(ctx)
		if e != nil {
			return e
		}
		ids := []int64{}
		for _, s := range sources {
			if s.Enabled {
				ids = append(ids, s.ID)
			}
		}
		raw, _ := json.Marshal(ids)
		res, err = r.db.ExecContext(ctx, `UPDATE ticket_borrow_targets SET scan_sources=$2,scan_offset=0,refresh_requested=true,next_attempt_at=NOW() WHERE account_id=$1 AND enabled AND (lease_until IS NULL OR lease_until<=NOW())`, id, string(raw))
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return service.ErrBorrowConflict
	}
	return nil
}
