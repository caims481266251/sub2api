package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const borrowAccountSignature = `md5(jsonb_build_array(a.credentials,a.proxy_id,a.extra->'enable_tls_fingerprint',a.extra->'tls_fingerprint_profile_id')::text)`

// Must match service.BorrowRenewalLead. Computed in the same database snapshot.
const borrowRenewalLead = `GREATEST(1,LEAST(CASE WHEN t.acquisition_duration_ms>0 THEN ((t.acquisition_duration_ms*3/2+999)/1000)::int+2 ELSE 15 END,GREATEST(1,s.admission_window_seconds/2)))`

func borrowInventory(ctx context.Context, db borrowQueryer, id int64, identity string) (*service.BorrowInventory, error) {
	var f service.BorrowInventoryFacts
	var p service.BorrowReplenishmentSettings
	var raw []byte
	var running, enabled bool
	var next, nextProbe time.Time
	var upstream, budget, lease sql.NullTime
	err := db.QueryRowContext(ctx, `SELECT clock_timestamp(),s.replenishment,0,
 `+borrowRenewalLead+`,
 t.last_batch_started_at,t.last_inventory_state,t.refresh_generation,t.processed_generation,t.next_attempt_at,t.next_probe_at,
 t.upstream_wait_until,t.budget_wait_until,t.lease_until,s.running,t.enabled,
 (SELECT count(*) FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.target_id=t.account_id AND r.target_revision=t.revision AND r.identity_hash=$2 AND NOT r.validation_only AND r.inventory_eligible AND r.revoked_at IS NULL
 AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp()),
 (SELECT min(LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)) FROM ticket_borrow_routes r LEFT JOIN ticket_borrow_cookie_deadlines d ON d.cookie_hash=r.cookie_hash
 WHERE r.target_id=t.account_id AND r.target_revision=t.revision AND r.identity_hash=$2 AND NOT r.validation_only AND r.inventory_eligible AND r.revoked_at IS NULL
 AND LEAST(r.expires_at,COALESCE(d.expires_at,r.expires_at),r.admission_until)>clock_timestamp())
 FROM ticket_borrow_targets t CROSS JOIN ticket_borrow_settings s WHERE t.account_id=$1 AND s.id=1`, id, identity).Scan(
		&f.Now, &raw, &f.MinRemainingSeconds, &f.RenewBeforeSeconds, &f.LastStarted, &f.LastState, &f.RefreshGeneration, &f.ProcessedGeneration, &next, &nextProbe,
		&upstream, &budget, &lease, &running, &enabled, &f.Count, &f.EarliestExpiry)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	out := service.EvaluateBorrowInventory(f, &p)
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
		if out.BlockedReason == "" {
			out.BlockedReason = "source_request_budget"
		}
		if sourceUntil.Time.After(out.NextAt) {
			out.NextAt = sourceUntil.Time
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
	p, err := borrowReplenishmentPolicy(ctx, r.db, a.TargetID)
	if err != nil {
		return false, err
	}
	if p == nil {
		return true, nil
	}
	inv, err := borrowInventory(ctx, r.db, a.TargetID, a.Identity)
	if err != nil {
		return false, err
	}
	if inv.Count < inv.Desired {
		return true, nil
	}
	if expiry != nil {
		// Compare with the same business-usable snapshot as Count. A stale,
		// expired or retired row must not make a shorter candidate look useful.
		return inv.EarliestExpiry == nil || expiry.After(*inv.EarliestExpiry), nil
	}
	var state string
	err = r.db.QueryRowContext(ctx, `SELECT last_inventory_state FROM ticket_borrow_targets WHERE account_id=$1 AND lease_token=$2`, a.TargetID, a.Lease).Scan(&state)
	return state == "healthy" || state == "renew", err
}

// Completed verdicts remain deduplicated even when formal publication loses a
// configuration race. This ledger is independent of business route eligibility.
func (r *ticketBorrowRepository) RecordCandidate(ctx context.Context, a service.BorrowAcquisition, candidate, cookie, verdict, publication string, expiry time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO ticket_borrow_candidate_results(target_id,target_revision,identity_hash,cookie_hash,candidate_id,batch_id,verdict,publication,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(target_id,target_revision,identity_hash,cookie_hash) DO UPDATE SET publication=EXCLUDED.publication`, a.TargetID, a.Revision, a.Identity, cookie, candidate, a.Lease, verdict, publication, expiry)
	return err
}
