package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// This lock covers allocation, sent history, and cancellation across instances.
// It never locks accounts, targets, settings or the pool mutation lock.
func lockBorrowCandidateAllocator(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(295001)`)
	return err
}

func (r *ticketBorrowRepository) AllocateCandidateEnvironment(ctx context.Context, id, identity string) (*service.BorrowCandidateEnvironment, error) {
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	p, pooled := service.BorrowPoolAcquisitionFromContext(ctx)
	if (!ok && !pooled) || id == "" || identity == "" || (pooled && p.ID != id) {
		return nil, service.ErrBorrowUnavailable
	}
	var poolID any
	if pooled {
		a = service.BorrowAcquisition{Lease: p.Lease}
		poolID = p.ID
	}
	f, err := service.NewBorrowFingerprint()
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockBorrowCandidateAllocator(ctx, tx); err != nil {
		return nil, err
	}
	if pooled {
		err = checkBorrowPoolAcquisition(ctx, tx, p)
	} else {
		err = checkBorrowAcquisition(ctx, tx, a, true)
	}
	if err != nil {
		return nil, err
	}
	// Join new/recovered entries at the current round instead of repeatedly using
	// them to catch up with months of history. Offset is scheduling state only;
	// uses and last_used_at continue to count actual sends, including failures.
	_, err = tx.ExecContext(ctx, `WITH baseline AS (
 SELECT COALESCE(min(u.uses+u.round_offset) FILTER(WHERE u.available AND p.enabled AND p.retired_at IS NULL),
 min(u.uses+u.round_offset),0) AS round FROM ticket_borrow_candidate_proxy_usage u
 JOIN proxy_pool_entries p ON p.id=u.entry_id WHERE u.identity_key=$1)
 INSERT INTO ticket_borrow_candidate_proxy_usage(identity_key,entry_id,round_offset)
 SELECT $1,p.id,baseline.round FROM proxy_pool_entries p CROSS JOIN baseline WHERE p.enabled AND p.retired_at IS NULL
 ON CONFLICT(identity_key,entry_id) DO UPDATE SET available=true,round_offset=CASE
 WHEN ticket_borrow_candidate_proxy_usage.available THEN ticket_borrow_candidate_proxy_usage.round_offset
 ELSE GREATEST(ticket_borrow_candidate_proxy_usage.round_offset,EXCLUDED.round_offset-ticket_borrow_candidate_proxy_usage.uses) END`, identity)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE ticket_borrow_candidate_proxy_usage u SET available=false
 FROM proxy_pool_entries p WHERE u.identity_key=$1 AND p.id=u.entry_id AND (NOT p.enabled OR p.retired_at IS NOT NULL)`, identity)
	if err != nil {
		return nil, err
	}
	env := &service.BorrowCandidateEnvironment{ID: id, Fingerprint: f}
	var ciphertext, entryIdentity string
	// Unsent reservations participate in the round, so concurrent workers cannot
	// all choose the same unused entry. A cancelled unsent reservation disappears.
	err = tx.QueryRowContext(ctx, `WITH candidates AS (SELECT p.id,p.credential_ciphertext,p.identity_hash,
 COALESCE(u.uses+u.round_offset,0)+load.reserved AS uses,load.occupied,
 GREATEST(u.last_used_at,load.last_reserved) AS last_seen,
 (SELECT COALESCE(sum(g.uses),0) FROM ticket_borrow_candidate_proxy_usage g WHERE g.entry_id=p.id) AS global_uses
 FROM proxy_pool_entries p
 LEFT JOIN ticket_borrow_candidate_proxy_usage u ON u.entry_id=p.id AND u.identity_key=$1
 CROSS JOIN LATERAL (SELECT count(*) FILTER(WHERE e.identity_key=$1 AND e.sent_at IS NULL) reserved,
 count(*) occupied, max(e.created_at) FILTER(WHERE e.identity_key=$1) last_reserved
 FROM ticket_borrow_candidate_environments e WHERE e.entry_id=p.id AND e.released_at IS NULL AND e.lease_until>clock_timestamp()) load
 WHERE p.enabled AND p.retired_at IS NULL), ranked AS (
 SELECT *,row_number() OVER(ORDER BY last_seen DESC NULLS LAST) AS recency,count(*) OVER() AS pool_size FROM candidates)
 SELECT id,credential_ciphertext,identity_hash,uses+1 FROM ranked
 ORDER BY uses,
 COALESCE(last_seen>clock_timestamp()-INTERVAL '30 seconds' AND recency<=GREATEST(1,pool_size/4),false),
 occupied,global_uses,random()
 LIMIT 1`, identity).Scan(&env.EntryID, &ciphertext, &entryIdentity, &env.AllocationRound)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrBorrowRotationNoProxy
	}
	if err != nil {
		return nil, err
	}
	env.Proxy, err = r.encryptor.Decrypt(ciphertext)
	if err != nil {
		return nil, errors.New("borrow_encryption_unavailable")
	}
	env.Proxy, env.SessionRandomized, err = service.RandomizeBorrowProxySession(env.Proxy)
	if err != nil {
		return nil, err
	}
	ciphertext, err = r.encryptor.Encrypt(env.Proxy)
	if err != nil {
		return nil, errors.New("borrow_encryption_unavailable")
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_borrow_candidate_environments(id,target_id,batch_id,identity_key,entry_id,entry_identity,allocation_round,fingerprint,proxy_ciphertext,session_randomized,pool_candidate_id)
 VALUES($1,NULLIF($2,0),$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, a.TargetID, a.Lease, identity, env.EntryID, entryIdentity, env.AllocationRound, string(raw), ciphertext, env.SessionRandomized, poolID)
	if err != nil {
		return nil, err
	}
	return env, tx.Commit()
}

func (r *ticketBorrowRepository) StartCandidateEnvironment(ctx context.Context, id string) (time.Time, time.Time, error) {
	var start, deadline time.Time
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	p, pooled := service.BorrowPoolAcquisitionFromContext(ctx)
	if !ok && !pooled {
		return start, deadline, service.ErrBorrowUnavailable
	}
	var checkErr error
	if pooled {
		a = service.BorrowAcquisition{Lease: p.Lease}
		checkErr = r.CheckCandidatePoolAcquisition(ctx, p)
	} else {
		checkErr = r.CheckAcquisition(ctx, a)
	}
	if err := checkErr; err != nil {
		return start, deadline, err
	}
	err := r.db.QueryRowContext(ctx, `UPDATE ticket_borrow_candidate_environments e
 SET started_at=COALESCE(e.started_at,clock_timestamp()),
 admission_until=COALESCE(e.admission_until,e.lease_until)
 FROM ticket_borrow_settings cfg,proxy_pool_entries p
 WHERE cfg.id=1 AND e.id=$1 AND e.target_id IS NOT DISTINCT FROM NULLIF($2,0) AND e.batch_id=$3 AND e.released_at IS NULL AND e.lease_until>clock_timestamp()
 AND p.id=e.entry_id AND p.identity_hash=e.entry_identity AND p.enabled AND p.retired_at IS NULL
 RETURNING e.started_at,e.admission_until`, id, a.TargetID, a.Lease).Scan(&start, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrBorrowUnavailable
	}
	return start, deadline, err
}

func (r *ticketBorrowRepository) CheckCandidateEnvironment(ctx context.Context, id string) error {
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	p, pooled := service.BorrowPoolAcquisitionFromContext(ctx)
	if !ok && !pooled {
		return service.ErrBorrowUnavailable
	}
	var checkErr error
	if pooled {
		a = service.BorrowAcquisition{Lease: p.Lease}
		checkErr = r.CheckCandidatePoolAcquisition(ctx, p)
	} else {
		// HTTP reservations enforce budget separately. A valid completed probe
		// must still be publishable when another attempt exhausts the budget.
		checkErr = checkBorrowAcquisition(ctx, r.db, a, false)
	}
	if err := checkErr; err != nil {
		return err
	}
	var eligible bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ticket_borrow_candidate_environments e
 JOIN proxy_pool_entries p ON p.id=e.entry_id CROSS JOIN ticket_borrow_settings cfg
 WHERE cfg.id=1 AND e.id=$1 AND e.target_id IS NOT DISTINCT FROM NULLIF($2,0) AND e.batch_id=$3 AND e.released_at IS NULL AND e.lease_until>clock_timestamp()
 AND p.enabled AND p.retired_at IS NULL AND p.identity_hash=e.entry_identity
 AND (e.pool_candidate_id IS NULL OR EXISTS(SELECT 1 FROM ticket_borrow_candidate_pool cp WHERE cp.id=e.pool_candidate_id
 AND cp.settings_revision=cfg.revision AND cp.lease_until>clock_timestamp()
 AND ((cp.state='acquiring' AND cp.owner_token=e.batch_id AND e.target_id IS NULL)
 OR (cp.state='claimed' AND cp.claim_token=e.batch_id AND cp.target_id=e.target_id))))
 AND LEAST(e.admission_until,COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=e.cookie_hash),e.admission_until))>clock_timestamp())`, id, a.TargetID, a.Lease).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return service.ErrBorrowCandidateExpiry
	}
	return nil
}

// Transfer the provisional acquisition environment to Cookie-backed inventory.
// Re-observing the same Cookie may tighten its deadline, never extend it.
func (r *ticketBorrowRepository) SetCandidateEnvironmentDeadline(ctx context.Context, id, hash string, until time.Time) error {
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	p, pooled := service.BorrowPoolAcquisitionFromContext(ctx)
	if pooled {
		a = service.BorrowAcquisition{Lease: p.Lease}
	}
	if (!ok && !pooled) || hash == "" || until.IsZero() {
		return service.ErrBorrowUnavailable
	}
	res, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_candidate_environments
 SET cookie_hash=$4,admission_until=CASE WHEN cookie_hash IS NULL THEN $5 ELSE LEAST(admission_until,$5) END,
 lease_until=CASE WHEN cookie_hash IS NULL THEN $5 ELSE LEAST(admission_until,$5) END
 WHERE id=$1 AND target_id IS NOT DISTINCT FROM NULLIF($2,0) AND batch_id=$3
 AND released_at IS NULL AND lease_until>clock_timestamp() AND $5>clock_timestamp()
 AND (cookie_hash IS NULL OR cookie_hash=$4)`, id, a.TargetID, a.Lease, hash, until)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return service.ErrBorrowCandidateExpiry
	}
	return err
}

func (r *ticketBorrowRepository) MarkCandidateEnvironmentSent(ctx context.Context, id string) error {
	a, ok := service.BorrowAcquisitionFromContext(ctx)
	p, pooled := service.BorrowPoolAcquisitionFromContext(ctx)
	if !ok && !pooled {
		return service.ErrBorrowUnavailable
	}
	if pooled {
		a = service.BorrowAcquisition{Lease: p.Lease}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockBorrowCandidateAllocator(ctx, tx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `WITH sent AS (
 UPDATE ticket_borrow_candidate_environments SET sent_at=clock_timestamp()
 WHERE id=$1 AND target_id IS NOT DISTINCT FROM NULLIF($2,0) AND batch_id=$3 AND sent_at IS NULL
 RETURNING identity_key,entry_id,sent_at)
 INSERT INTO ticket_borrow_candidate_proxy_usage(identity_key,entry_id,uses,last_used_at)
 SELECT identity_key,entry_id,1,sent_at FROM sent
 ON CONFLICT(identity_key,entry_id) DO UPDATE SET uses=ticket_borrow_candidate_proxy_usage.uses+1,last_used_at=EXCLUDED.last_used_at`, id, a.TargetID, a.Lease)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ticketBorrowRepository) ReleaseCandidateEnvironment(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ticket_borrow_candidate_environments e SET released_at=COALESCE(released_at,clock_timestamp()) WHERE id=$1 AND NOT EXISTS(SELECT 1 FROM ticket_borrow_routes r WHERE r.candidate_id=e.id AND (LEAST(r.admission_until,r.expires_at)>clock_timestamp() OR EXISTS(SELECT 1 FROM ticket_borrow_route_leases l WHERE l.route_id=r.id AND l.lease_until>clock_timestamp())))`, id)
	return err
}

var _ service.BorrowCandidateEnvironmentRepository = (*ticketBorrowRepository)(nil)
