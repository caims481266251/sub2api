package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
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
	// Match the candidate allocator's upstream account+user grouping. Missing
	// identifiers remain isolated, and this observation never changes admission.
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum((`+strings.ReplaceAll(borrowBackgroundInflight, "$1", "peer.id")+`)),0)
 FROM accounts own JOIN accounts peer ON peer.id=own.id OR (
 COALESCE(own.credentials->>'chatgpt_account_id','')<>'' AND COALESCE(own.credentials->>'chatgpt_user_id','')<>''
 AND peer.credentials->>'chatgpt_account_id'=own.credentials->>'chatgpt_account_id'
 AND peer.credentials->>'chatgpt_user_id'=own.credentials->>'chatgpt_user_id')
 JOIN ticket_borrow_targets t ON t.account_id=peer.id
 WHERE own.id=$1 AND peer.deleted_at IS NULL`, target).Scan(&out.SharedTargetCount, &out.SharedTargetInflight); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT
 count(DISTINCT NULLIF(details->>'cookie_hash','')),
 count(DISTINCT NULLIF(details->>'gateway','')),
 count(DISTINCT details->>'candidate_id') FILTER(WHERE stage='source' AND reason IN ('source_failed','source_refresh_failed','source_rate_limited','source_credentials_unavailable','source_http_401','source_http_403','source_http_429','source_account_unavailable')),
 count(DISTINCT details->>'candidate_id') FILTER(WHERE reason='candidate_gateway_prioritized'),
 count(DISTINCT details->>'candidate_id') FILTER(WHERE reason='candidate_gateway_recheck')
 FROM ticket_borrow_events WHERE target_id=$1 AND created_at BETWEEN $2 AND $3 AND stage IN ('source','target','candidate')`, target, out.Since, out.Until).Scan(&out.Candidates, &out.Gateways, &out.SourceFailed, &out.Prioritized, &out.Rechecked); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT max(created_at) FROM ticket_borrow_events WHERE target_id=$1 AND stage='target' AND reason='target_probe_passed'`, target).Scan(&out.LastSuccessAt); err != nil {
		return nil, err
	}
	// The persisted verdict ledger wins over secondary events. Repeated Cookie
	// checks share a key; cancellations without a Cookie use their candidate ID.
	rows, err := tx.QueryContext(ctx, `WITH outcomes AS (
 SELECT concat_ws(':',target_revision,identity_hash,cookie_hash) key,candidate_id,publication reason,verdict,'' failure,created_at,0 priority
 FROM ticket_borrow_candidate_results WHERE target_id=$1 AND created_at BETWEEN $2 AND $3
 UNION ALL
 SELECT concat_ws(':',details->>'target_revision',details->>'target_identity_hash',COALESCE(NULLIF(details->>'cookie_hash',''),details->>'candidate_id')),
 details->>'candidate_id',reason,COALESCE(details->>'probe_verdict',''),COALESCE(details->>'probe_failure',''),created_at,
 CASE WHEN stage='target' THEN 1 ELSE 2 END
 FROM ticket_borrow_events WHERE target_id=$1 AND created_at BETWEEN $2 AND $3 AND stage IN ('target','candidate') AND reason NOT IN ('candidate_gateway_prioritized','candidate_gateway_recheck','gateway_feedback_unavailable','candidate_pool_enqueued','candidate_pool_claimed') AND COALESCE(details->>'candidate_id','')<>''
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
		case reason == "candidate_unused_after_success" || reason == "candidate_unused_after_stop" || reason == "candidate_pool_verification_abandoned" || failure == "cancelled":
			out.Cancelled++
		case reason == "target_probe_passed" || verdict == "healthy":
			out.Passed++
			if reason == "target_probe_passed" {
				out.Published++
			}
		case reason == "target_route_changed" || failure == "route_changed":
			out.RouteChanged++
		case reason == "target_probe_inconclusive":
			out.Inconclusive++
		case reason == "target_ticket_changed" || verdict == "degraded":
			out.TicketChanged++
		case reason == "target_continue_timeout" || failure == "target_continue_timeout":
			out.Timeout++
		case reason == "candidate_time_budget_insufficient" || reason == "candidate_duplicate" || reason == "borrow_acquisition_paused" || reason == "target_route_ready" || reason == "target_request_budget" || reason == "borrow_probe_concurrency_full":
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
	// Each registered probe HTTP attempt is counted once, independently of
	// candidate verdicts. Old rows contribute requests, but no invented timings.
	phaseRows, phaseErr := tx.QueryContext(ctx, `SELECT phase, details->'diagnostic'
 FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe' AND phase IN ('mint','continue') AND reserved_at BETWEEN $2 AND $3`, target, out.Since, out.Until)
	if phaseErr != nil {
		return nil, phaseErr
	}
	sums := map[string]int64{}
	for phaseRows.Next() {
		var name string
		var raw []byte
		if err = phaseRows.Scan(&name, &raw); err != nil {
			phaseRows.Close()
			return nil, err
		}
		phase := &out.Mint
		if name == "continue" {
			phase = &out.Continue
		}
		phase.Requests++
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var diagnostic service.BorrowHTTPAttempt
		if err = json.Unmarshal(raw, &diagnostic); err != nil {
			phaseRows.Close()
			return nil, err
		}
		phase.DiagnosticSamples++
		sums[name] += diagnostic.DurationMS
		if diagnostic.Number > 1 {
			phase.Retries++
		}
		if phase.ErrorClasses == nil {
			phase.ErrorClasses = map[string]int{}
			phase.TimeoutStages = map[string]int{}
			phase.Cancellations = map[string]int{}
		}
		if diagnostic.ErrorClass != "" {
			phase.ErrorClasses[diagnostic.ErrorClass]++
		}
		if diagnostic.TimeoutStage != "" {
			phase.Timeouts++
			phase.TimeoutStages[diagnostic.TimeoutStage]++
		}
		if diagnostic.CancellationCause != "" {
			phase.Cancellations[diagnostic.CancellationCause]++
		}
	}
	if err = phaseRows.Err(); err != nil {
		phaseRows.Close()
		return nil, err
	}
	phaseRows.Close()
	for name, phase := range map[string]*service.BorrowPhaseStatistics{"mint": &out.Mint, "continue": &out.Continue} {
		if phase.DiagnosticSamples > 0 {
			average := float64(sums[name]) / float64(phase.DiagnosticSamples)
			phase.AverageMS = &average
		}
	}
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
