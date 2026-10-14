package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const borrowAccountSignature = `md5(jsonb_build_array(a.credentials,a.proxy_id,a.extra->'enable_tls_fingerprint',a.extra->'tls_fingerprint_profile_id')::text)`

// Must match service.BorrowRenewalLead. Computed in the same database snapshot.
const borrowRenewalLead = `GREATEST(1,LEAST(CASE WHEN t.acquisition_duration_ms>0 THEN ((t.acquisition_duration_ms*3/2+999)/1000)::int+2 ELSE 15 END,240))`

func borrowInventory(ctx context.Context, db borrowQueryer, id int64, identity string) (*service.BorrowInventory, error) {
	var f service.BorrowInventoryFacts
	var desired, stored, pending int

	var running, enabled bool
	var next, nextProbe time.Time
	var upstream, budget, lease sql.NullTime
	err := db.QueryRowContext(ctx, `SELECT clock_timestamp(),s.route_pool_size,
 `+borrowRenewalLead+`,
 t.last_batch_started_at,t.last_inventory_state,t.refresh_generation,t.processed_generation,t.next_attempt_at,t.next_probe_at,
 t.upstream_wait_until,t.budget_wait_until,t.lease_until,s.running,t.enabled,
 (SELECT count(*) FROM ticket_borrow_routes r WHERE r.target_id=t.account_id AND `+borrowStoredRoute+`),
 (SELECT count(*) FROM ticket_borrow_routes r WHERE r.target_id=t.account_id AND `+borrowStoredRoute+` AND r.state IN ('pending_quality','pending_probe')),
 (SELECT count(*) FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.target_id=t.account_id AND r.target_revision=t.revision AND r.identity_hash=$2 AND NOT r.validation_only AND r.inventory_eligible AND r.revoked_at IS NULL
 AND r.state='normal' AND `+borrowQualityEligible+` AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp()),
 (SELECT min(LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)) FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.target_id=t.account_id AND r.target_revision=t.revision AND r.identity_hash=$2 AND NOT r.validation_only AND r.inventory_eligible AND r.revoked_at IS NULL
 AND r.state='normal' AND `+borrowQualityEligible+` AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp()),
 (SELECT (array_agg(LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until) ORDER BY LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until) DESC))[s.route_pool_size] FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.target_id=t.account_id AND r.target_revision=t.revision AND r.identity_hash=$2 AND NOT r.validation_only AND r.inventory_eligible AND r.revoked_at IS NULL
 AND r.state='normal' AND `+borrowQualityEligible+` AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp())
 FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings s WHERE t.account_id=$1 AND s.id=1`, id, identity).Scan(
		&f.Now, &desired, &f.RenewBeforeSeconds, &f.LastStarted, &f.LastState, &f.RefreshGeneration, &f.ProcessedGeneration, &next, &nextProbe,
		&upstream, &budget, &lease, &running, &enabled, &stored, &pending, &f.Count, &f.EarliestExpiry, &f.RenewalExpiry)
	if err != nil {
		return nil, err
	}
	out := service.EvaluateBorrowInventory(f, desired)
	out.StoredCount = stored
	out.PendingCount = pending
	// Explicit no-source/backoff delays apply until a newer event arrives. The
	// periodic due time is computed from start, never from FinishJob completion.
	if f.RefreshGeneration <= f.ProcessedGeneration && next.After(out.NextAt) {
		out.NextAt = next
	}
	if upstream.Valid && upstream.Time.After(f.Now) {
		out.BlockedReason = "target_upstream_waiting"
		if upstream.Time.After(out.NextAt) {
			out.NextAt = upstream.Time
		}
	}
	if budget.Valid && budget.Time.After(f.Now) {
		out.BlockedReason = "target_request_budget"
		if budget.Time.After(out.NextAt) {
			out.NextAt = budget.Time
		}
	}
	var sourceUntil sql.NullTime
	err = db.QueryRowContext(ctx, `SELECT (SELECT (array_agg(reserved_at ORDER BY reserved_at DESC))[GREATEST(1,COALESCE((cfg.recovery->>'source_requests_per_minute')::int,12))]+INTERVAL '60 seconds'
 FROM ticket_borrow_source_attempts WHERE reserved_at>clock_timestamp()-INTERVAL '60 seconds') FROM ticket_borrow_settings cfg WHERE cfg.id=1`).Scan(&sourceUntil)
	if err != nil {
		return nil, err
	}
	if sourceUntil.Valid && sourceUntil.Time.After(f.Now) {
		// Source HTTP budget cannot block target verification of an already
		// acquired shared candidate. Self mode still needs a source HTTP permit.
		var canConsume bool
		err = db.QueryRowContext(ctx, `SELECT t.acquisition_identity='source' AND EXISTS(
 SELECT 1 FROM ticket_borrow_candidate_pool p JOIN ticket_borrow_candidate_environments e ON e.id=p.id
 WHERE p.state='queued' AND p.lease_until>clock_timestamp()
 AND `+borrowPoolCandidateValid+` AND `+borrowPoolSourceReady+`
 AND LEAST(p.admission_until,e.admission_until)>clock_timestamp()+(45+cfg.continue_timeout_seconds+$2)*INTERVAL '1 second')
 FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings cfg WHERE t.account_id=$1 AND cfg.id=1`, id, int(service.BorrowCandidatePoolPublishReserve/time.Second)).Scan(&canConsume)
		if err != nil {
			return nil, err
		}
		if !canConsume {
			if out.BlockedReason == "" {
				out.BlockedReason = "source_request_budget"
			}
			if sourceUntil.Time.After(out.NextAt) {
				out.NextAt = sourceUntil.Time
			}
		}
	}
	if lease.Valid && lease.Time.After(f.Now) {
		out.BlockedReason = "batch_running"
	}
	if !running {
		out.BlockedReason = "borrow_acquisition_paused"
	}
	if !enabled {
		out.BlockedReason = "target_unavailable"
	}
	return &out, nil
}

func (r *ticketBorrowRepository) Inventory(ctx context.Context, id int64, identity string) (*service.BorrowInventory, error) {
	return borrowInventory(ctx, r.db, id, identity)
}

func (r *ticketBorrowRepository) NeedsCandidate(ctx context.Context, a service.BorrowAcquisition, expiry *time.Time) (bool, error) {
	if err := checkBorrowAcquisition(ctx, r.db, a, true); err != nil {
		return false, err
	}
	inv, err := borrowInventory(ctx, r.db, a.TargetID, a.Identity)
	if err != nil {
		return false, err
	}
	if inv.Count+inv.PendingCount >= inv.Desired && inv.State != "renew" {
		return false, nil
	}
	if inv.Count < inv.Desired {
		return true, nil
	}
	if expiry != nil {
		// Compare with the same business-usable snapshot as Count. A stale,
		// expired or retired row must not make a shorter candidate look useful.
		return inv.State == "renew" && (inv.RenewalExpiry == nil || expiry.After(*inv.RenewalExpiry)), nil
	}
	return inv.State == "renew", nil
}

// Completed verdicts remain deduplicated even when formal publication loses a
// configuration race. This ledger is independent of business route eligibility.
func (r *ticketBorrowRepository) RecordCandidate(ctx context.Context, a service.BorrowAcquisition, candidate, cookie, verdict, publication string, expiry time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_candidate_results(target_id,target_revision,identity_hash,cookie_hash,candidate_id,batch_id,verdict,publication,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(target_id,target_revision,identity_hash,cookie_hash) DO UPDATE SET publication=EXCLUDED.publication`, a.TargetID, a.Revision, a.Identity, cookie, candidate, a.Lease, verdict, publication, expiry)
	return err
}

// Search time includes failed candidates, but excludes healthy inventory idle time.
// A successful publication within a long rolling job starts the next search.
const borrowSearchElapsedMS = `GREATEST(1,(EXTRACT(EPOCH FROM (clock_timestamp()-GREATEST(
 COALESCE(t.last_batch_started_at,clock_timestamp()),
 COALESCE((SELECT max(r.checked_at) FROM ticket_borrow_routes r WHERE r.target_id=t.account_id
 AND r.target_revision=t.revision AND r.identity_hash=t.lease_identity AND NOT r.validation_only),t.last_batch_started_at,clock_timestamp())
 )))*1000)::bigint)`

// Pending checks and cooldown retain their slot; quality failures must not
// turn a bounded route pool into an ever-growing queue of untested routes.
const borrowStoredRoute = `r.target_revision=t.revision AND r.inventory_eligible AND NOT r.validation_only AND r.revoked_at IS NULL
 AND r.state IN ('normal','pending_probe','pending_quality','cooling')
 AND LEAST(r.expires_at,r.admission_until,COALESCE((SELECT expires_at FROM ticket_borrow_cookie_deadlines WHERE cookie_hash=r.cookie_hash),r.expires_at))>clock_timestamp()`
