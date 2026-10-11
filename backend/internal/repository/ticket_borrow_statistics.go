package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *ticketBorrowRepository) BorrowStatistics(ctx context.Context, target int64, minutes int) (*service.BorrowStatistics, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &service.BorrowStatistics{TargetID: target, Routes: []service.BorrowRouteStats{}}
	if err = tx.QueryRowContext(ctx, `SELECT NOW()`).Scan(&out.Until); err != nil {
		return nil, err
	}
	out.Since = out.Until.Add(-time.Duration(minutes) * time.Minute)
	// The persisted verdict ledger wins over secondary events. Repeated Cookie
	// checks share a key; cancellations without a Cookie use their candidate ID.
	rows, err := tx.QueryContext(ctx, `WITH outcomes AS (
 SELECT concat_ws(':',target_revision,identity_hash,cookie_hash) key,candidate_id,publication reason,verdict,'' failure,created_at,0 priority
 FROM ticket_borrow_candidate_results WHERE target_id=$1 AND created_at BETWEEN $2 AND $3
 UNION ALL
 SELECT concat_ws(':',details->>'target_revision',details->>'target_identity_hash',COALESCE(NULLIF(details->>'cookie_hash',''),details->>'candidate_id')),
 details->>'candidate_id',reason,COALESCE(details->>'probe_verdict',''),COALESCE(details->>'probe_failure',''),created_at,
 CASE WHEN stage='target' THEN 1 ELSE 2 END
 FROM ticket_borrow_events WHERE target_id=$1 AND created_at BETWEEN $2 AND $3 AND stage IN ('target','candidate') AND COALESCE(details->>'candidate_id','')<>''
 ) SELECT DISTINCT ON (key) candidate_id,reason,verdict,failure FROM outcomes ORDER BY key,priority,created_at DESC`, target, out.Since, out.Until)
	if err != nil {
		return nil, err
	}
	finalized := map[string]bool{}
	for rows.Next() {
		var candidate, reason, verdict, failure string
		if err = rows.Scan(&candidate, &reason, &verdict, &failure); err != nil {
			rows.Close()
			return nil, err
		}
		finalized[candidate] = true
		switch {
		case reason == "candidate_unused_after_success" || reason == "candidate_unused_after_stop" || failure == "cancelled":
			out.Cancelled++
		case reason == "target_probe_passed" || verdict == "healthy":
			out.Passed++
			if reason == "target_probe_passed" {
				out.Published++
			}
		case reason == "target_ticket_changed" || verdict == "degraded":
			out.TicketChanged++
		case reason == "target_continue_timeout" || failure == "target_continue_timeout":
			out.Timeout++
		case reason == "candidate_duplicate" || reason == "borrow_acquisition_paused" || reason == "target_route_ready" || reason == "target_request_budget" || reason == "borrow_probe_concurrency_full":
			out.Skipped++
		default:
			out.Other++
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT details->>'candidate_id' FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe' AND finished_at IS NULL AND lease_until>$2 AND COALESCE(details->>'candidate_id','')<>''`, target, out.Until)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if !finalized[id] {
			out.Inflight++
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// One request/route pair, not one event or one physical retry. Historical
	// timings without explicit route attribution are intentionally omitted.
	rows, err = tx.QueryContext(ctx, `WITH attributed AS (
 SELECT d.trace_id,d.usage_log_id,a->>'borrow_route_id' route_id,
 bool_or(a->>'terminal'='completed') completed,
 bool_or(COALESCE(a->>'terminal','') IN ('failed','incomplete') OR (COALESCE(a->>'error','')<>'' AND NOT COALESCE((a->>'cleanup_canceled')::boolean,false))) interrupted,
 MIN((a->'events'->>'first_visible')::double precision) visible_ms,
 MIN((a->'events'->>'first_semantic')::double precision) semantic_ms,
 MIN(EXTRACT(EPOCH FROM ((d.detail->>'started_at')::timestamptz-r.checked_at))*1000+(a->>'start_ms')::double precision) age_ms
 FROM request_timing_details d CROSS JOIN LATERAL jsonb_array_elements(d.detail->'attempts') a
 JOIN ticket_borrow_routes r ON r.id=a->>'borrow_route_id' AND r.target_id=$1
 WHERE d.created_at BETWEEN $2 AND $3 AND (d.detail->>'started_at')::timestamptz BETWEEN $2 AND $3 AND a->>'kind'='egress'
 GROUP BY d.trace_id,d.usage_log_id,a->>'borrow_route_id'
 ) SELECT x.route_id,r.gateway,count(*),count(*) FILTER(WHERE completed),count(*) FILTER(WHERE NOT COALESCE(completed,false) AND interrupted),
 count(*) FILTER(WHERE NOT COALESCE(completed,false) AND NOT COALESCE(interrupted,false)),count(visible_ms),avg(visible_ms),count(semantic_ms),avg(semantic_ms),avg(age_ms),max(usage_log_id)
 FROM attributed x JOIN ticket_borrow_routes r ON r.id=x.route_id GROUP BY x.route_id,r.gateway,r.checked_at ORDER BY r.checked_at DESC`, target, out.Since, out.Until)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v service.BorrowRouteStats
		if err = rows.Scan(&v.RouteID, &v.Gateway, &v.Requests, &v.Completed, &v.Interrupted, &v.Unconfirmed, &v.VisibleSamples, &v.VisibleAverageMS, &v.SemanticSamples, &v.SemanticAverageMS, &v.AverageAgeMS, &v.LatestUsageID); err != nil {
			rows.Close()
			return nil, err
		}
		out.Routes = append(out.Routes, v)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return out, tx.Commit()
}
