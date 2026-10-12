package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *ticketBorrowRepository) SaveCandidatePoolConfig(ctx context.Context, config service.BorrowCandidatePoolConfig) (*service.BorrowCandidatePoolConfig, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	// This UPDATE takes the same singleton row lock as producer reservations.
	// No global settings revision changes and no candidate/route is invalidated.
	var saved service.BorrowCandidatePoolConfig
	err := r.db.QueryRowContext(ctx, `UPDATE ticket_borrow_candidate_pool_control
 SET capacity=$1,revision=revision+1 WHERE id=1 AND revision=$2 RETURNING capacity,revision`, config.Capacity, config.Revision).Scan(&saved.Capacity, &saved.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrBorrowConflict
	}
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

const borrowPoolAdminFrom = ` FROM ticket_borrow_candidate_pool p
 JOIN ticket_borrow_sources s ON s.id=p.source_id
 LEFT JOIN accounts sa ON sa.id=s.account_id
 LEFT JOIN accounts ta ON ta.id=p.target_id`

const borrowPoolAdminMetadataJoins = `
 CROSS JOIN ticket_borrow_settings cfg
 LEFT JOIN ticket_borrow_candidate_environments env ON env.id=p.id
 LEFT JOIN ticket_borrow_cookie_deadlines cd ON cd.cookie_hash=p.cookie_hash
 LEFT JOIN LATERAL (SELECT id FROM ticket_borrow_routes WHERE candidate_id=p.id AND NOT validation_only ORDER BY checked_at DESC,id DESC LIMIT 1) route ON true`

const borrowPoolAdminColumns = `p.id,p.state,p.source_id,COALESCE(s.account_id,0),COALESCE(sa.name,s.name,''),
 COALESCE(p.target_id,0),COALESCE(ta.name,''),COALESCE(p.details->>'gateway',''),
 p.created_at,p.enqueued_at,p.claimed_at,p.finished_at,
 LEAST(p.admission_until,env.admission_until,env.started_at+cfg.admission_window_seconds*INTERVAL '1 second',cd.expires_at),
 CASE WHEN p.state='queued' THEN p.lease_until END,p.reason,COALESCE(route.id,'')`

func borrowPoolItemDest(v *service.BorrowCandidatePoolItem) []any {
	return []any{&v.ID, &v.State, &v.SourceID, &v.SourceAccountID, &v.SourceName, &v.TargetID, &v.TargetName, &v.Gateway,
		&v.CreatedAt, &v.EnqueuedAt, &v.ClaimedAt, &v.FinishedAt, &v.AdmissionUntil, &v.QueueUntil, &v.Reason, &v.RouteID}
}

func (r *ticketBorrowRepository) CandidatePoolPage(ctx context.Context, q service.BorrowCandidatePoolQuery) (*service.BorrowCandidatePoolPage, error) {
	if err := q.Normalize(); err != nil {
		return nil, err
	}
	// Counts, configuration and list rows must describe the same committed
	// snapshot. Observation never sweeps or modifies pool ownership.
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &service.BorrowCandidatePoolPage{Page: q.Page, PageSize: q.PageSize, Items: []service.BorrowCandidatePoolItem{}}
	summary := &out.Summary
	if err = tx.QueryRowContext(ctx, `SELECT ctl.capacity,ctl.revision,cfg.running,transaction_timestamp()
 FROM ticket_borrow_candidate_pool_control ctl CROSS JOIN ticket_borrow_settings cfg WHERE ctl.id=1 AND cfg.id=1`).Scan(&summary.Capacity, &summary.Revision, &summary.Running, &out.ServerNow); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE state='acquiring'),count(*) FILTER(WHERE state='queued'),count(*) FILTER(WHERE state='claimed'),count(*)
 FROM ticket_borrow_candidate_pool WHERE state<>'finished'`).Scan(&summary.Acquiring, &summary.Queued, &summary.Claimed, &summary.Occupied); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE slots>0),COALESCE(sum(slots),0) FROM (SELECT `+borrowPoolOpenDemandSlots+` AS slots`+borrowPoolDemandFrom+`) demand`).Scan(&summary.WaitingTargets, &summary.DemandSlots); err != nil {
		return nil, err
	}
	clauses, args := []string{"true"}, []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if q.State == "active" {
		clauses = append(clauses, "p.state<>'finished'")
	} else if q.State != "all" {
		add("p.state=$%d", q.State)
	}
	if q.SourceAccountID > 0 {
		add("s.account_id=$%d", q.SourceAccountID)
	}
	if q.TargetID > 0 {
		add("p.target_id=$%d", q.TargetID)
	}
	if q.Search != "" {
		add("strpos(lower(concat_ws(' ',p.id,sa.name,s.name,ta.name,p.details->>'gateway',p.reason)),lower($%d))>0", q.Search)
	}
	where := " WHERE " + strings.Join(clauses, " AND ")
	if err = tx.QueryRowContext(ctx, `SELECT count(*)`+borrowPoolAdminFrom+where, args...).Scan(&out.Total); err != nil {
		return nil, err
	}
	args = append(args, q.PageSize, (q.Page-1)*q.PageSize)
	rows, err := tx.QueryContext(ctx, `SELECT `+borrowPoolAdminColumns+borrowPoolAdminFrom+borrowPoolAdminMetadataJoins+where+fmt.Sprintf(` AND cfg.id=1 ORDER BY p.created_at DESC,p.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v service.BorrowCandidatePoolItem
		if err = rows.Scan(borrowPoolItemDest(&v)...); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *ticketBorrowRepository) CandidatePoolDetail(ctx context.Context, id string) (*service.BorrowCandidatePoolDetail, error) {
	out := &service.BorrowCandidatePoolDetail{}
	var raw []byte
	// Final pool transitions retain acquisition metadata. Prefer the completed
	// verification observation when available; pool cleanup events must not hide
	// its phase timings. Decode into the public allowlist instead of returning JSON.
	err := r.db.QueryRowContext(ctx, `SELECT `+borrowPoolAdminColumns+`,p.details||COALESCE(observed.details,'{}'::jsonb),statement_timestamp()`+
		borrowPoolAdminFrom+borrowPoolAdminMetadataJoins+`
 LEFT JOIN LATERAL (SELECT details FROM ticket_borrow_events e
 WHERE e.details->>'candidate_id'=p.id AND e.stage IN ('source','target','candidate','route')
 AND (e.details ? 'source_http' OR e.details ? 'mint' OR e.details ? 'probe_verdict' OR e.stage='source')
 ORDER BY (e.details ? 'probe_verdict') DESC,(e.details ? 'mint') DESC,e.id DESC LIMIT 1) observed ON true
 WHERE cfg.id=1 AND p.id=$1`, id).Scan(append(borrowPoolItemDest(&out.BorrowCandidatePoolItem), &raw, &out.ServerNow)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrBorrowPoolNotFound
	}
	if err != nil {
		return nil, err
	}
	out.Details = &service.BorrowCandidateDetails{}
	if err = json.Unmarshal(raw, out.Details); err != nil {
		return nil, err
	}
	out.Details.CandidateID = id
	out.Details.BatchID = ""
	out.Details.PoolEnqueuedAt = out.EnqueuedAt
	out.Details.PoolClaimedAt = out.ClaimedAt
	return out, nil
}

var _ service.BorrowCandidatePoolAdminRepository = (*ticketBorrowRepository)(nil)
