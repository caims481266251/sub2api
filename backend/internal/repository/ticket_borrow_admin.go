package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The scheduler's Targets method intentionally continues to return only enabled
// accounts. The management page also needs previously configured targets.
func (r *ticketBorrowRepository) AdminTargets(ctx context.Context) ([]service.BorrowTarget, error) {
	rows, err := r.db.QueryContext(ctx, borrowTargetSelect+`WHERE t.account_id IS NOT NULL AND a.deleted_at IS NULL ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.BorrowTarget{}
	for rows.Next() {
		t, err := scanBorrowTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *ticketBorrowRepository) QueryEvents(ctx context.Context, q service.BorrowEventQuery) ([]service.BorrowEvent, error) {
	clauses := []string{"e.stage<>'business'", "e.created_at>=NOW()-INTERVAL '24 hours'"}
	args := []any{}
	add := func(expr string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if q.Before > 0 {
		add("e.id<$%d", q.Before)
	}
	if q.After > 0 {
		add("e.id>$%d", q.After)
	}
	if q.TargetID > 0 {
		add("e.target_id=$%d", q.TargetID)
	}
	if q.SourceAccount > 0 {
		add("COALESCE(s.account_id,NULLIF(e.details->>'source_account_id','')::bigint,0)=$%d", q.SourceAccount)
	}
	if q.SourceID > 0 {
		add("e.source_id=$%d", q.SourceID)
	}
	if q.Stage != "" {
		add("e.stage=$%d", q.Stage)
	}
	if q.Reason != "" {
		add("e.reason=$%d", q.Reason)
	}
	if q.CandidateID != "" {
		add("e.details->>'candidate_id'=$%d", q.CandidateID)
	}
	if q.RouteID != "" {
		add("e.route_id=$%d", q.RouteID)
	}
	if !q.Since.IsZero() {
		add("e.created_at>=$%d", q.Since)
	}
	if !q.Until.IsZero() {
		add("e.created_at<=$%d", q.Until)
	}
	if q.HideRotations {
		clauses = append(clauses, "NOT (e.stage='rotation' AND e.reason IN ('rotation_completed','rotation_environment_changed') AND COALESCE(e.details->'rotation'->>'trigger','')='automatic')")
	}
	if q.Search != "" {
		// strpos treats %, _, quotes and slashes as literal search characters.
		add("strpos(lower(concat_ws(' ',e.id,e.target_id,s.account_id,sa.name,ta.name,e.reason,e.details->>'candidate_id',e.route_id,e.details->>'gateway')),lower($%d))>0", q.Search)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT e.id,COALESCE(e.target_id,0),COALESCE(e.source_id,0),COALESCE(e.route_id,''),e.stage,e.reason,e.created_at,
 COALESCE(s.account_id,NULLIF(e.details->>'source_account_id','')::bigint,0),e.details
 FROM ticket_borrow_events e LEFT JOIN ticket_borrow_sources s ON s.id=e.source_id
 LEFT JOIN accounts sa ON sa.id=COALESCE(s.account_id,NULLIF(e.details->>'source_account_id','')::bigint)
 LEFT JOIN accounts ta ON ta.id=e.target_id WHERE `+strings.Join(clauses, " AND ")+` ORDER BY e.id DESC LIMIT 100`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []service.BorrowEvent{}
	for rows.Next() {
		var e service.BorrowEvent
		var details []byte
		if err = rows.Scan(&e.ID, &e.TargetID, &e.SourceID, &e.RouteID, &e.Stage, &e.Reason, &e.CreatedAt, &e.SourceAccountID, &details); err != nil {
			return nil, err
		}
		if len(details) > 0 && string(details) != "{}" && string(details) != "null" {
			if err = json.Unmarshal(details, &e.Details); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
