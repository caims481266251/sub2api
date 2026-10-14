//go:build integration

package repository

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowAdminDisabledTargetsStayOutOfScheduler(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	current, err := r.Target(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, r.SetTarget(t.Context(), id, false, current.Revision))
	rows, err := r.Targets(t.Context())
	require.NoError(t, err)
	for _, target := range rows {
		require.NotEqual(t, id, target.AccountID)
	}
	all, err := r.(service.BorrowAdminTargetRepository).AdminTargets(t.Context())
	require.NoError(t, err)
	found := false
	for _, target := range all {
		if target.AccountID == id {
			found = true
			require.False(t, target.Enabled)
			require.Positive(t, target.ProbePolicy.Revision)
		}
	}
	require.True(t, found)
}

func TestTicketBorrowEventQueriesFilterBeforePagination(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	ctx := t.Context()
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,stage,reason,details) VALUES($1,'target','target_ticket_changed','{"source_account_id":917,"candidate_id":"needle","gateway":"literal_%"}')`, id)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,stage,reason,details) SELECT $1,'rotation','rotation_environment_changed','{"rotation":{"trigger":"automatic"}}'::jsonb FROM generate_series(1,110)`, id)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,stage,reason,details) VALUES($1,'rotation','rotation_environment_changed','{}'),($1,'business','legacy','{}')`, id)
	require.NoError(t, err)
	query := r.(service.BorrowEventQueryRepository)
	rows, err := query.QueryEvents(ctx, service.BorrowEventQuery{TargetID: id, SourceAccount: 917, Stage: "target", Reason: "target_ticket_changed", CandidateID: "needle", Search: "_%", Since: time.Now().Add(-time.Minute)})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(917), rows[0].SourceAccountID)
	more, err := query.QueryEvents(ctx, service.BorrowEventQuery{TargetID: id, Before: rows[0].ID})
	require.NoError(t, err)
	require.Empty(t, more)
	rows, err = query.QueryEvents(ctx, service.BorrowEventQuery{TargetID: id, HideRotations: true})
	require.NoError(t, err)
	require.Len(t, rows, 2, "hide only known normal automatic rotations; retain unknown trigger and exclude business")
}

func TestTicketBorrowStatisticsCountUniqueVerdictsAndAttributedBusiness(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	ctx := t.Context()
	acq := service.BorrowAcquisition{TargetID: id, Revision: 1, Identity: "identity", Lease: "batch"}
	ledger := r.(service.BorrowCandidateRepository)
	require.NoError(t, ledger.RecordCandidate(ctx, acq, "good", "cookie-good", "healthy", "target_probe_passed", time.Now().Add(time.Minute)))
	event := func(candidate, cookie, reason, verdict string) {
		detail, _ := json.Marshal(map[string]any{"target_revision": 1, "target_identity_hash": "identity", "cookie_hash": cookie, "candidate_id": candidate, "probe_verdict": verdict})
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,stage,reason,details) VALUES($1,'target',$2,$3)`, id, reason, string(detail))
		require.NoError(t, err)
	}
	event("good", "cookie-good", "target_probe_passed", "healthy")
	event("different-id-same-cookie", "cookie-good", "target_probe_passed", "healthy")
	event("mismatch", "cookie-bad", "target_ticket_changed", "degraded")
	event("timeout", "cookie-timeout", "target_continue_timeout", "")
	event("cancel", "cookie-cancel", "candidate_unused_after_success", "")
	event("pending", "cookie-pending", "candidate_gateway_prioritized", "")
	event("pending", "cookie-pending", "candidate_gateway_recheck", "")
	event("pending", "cookie-pending", "gateway_feedback_unavailable", "")
	// Use a real published route and persisted timing schema; physical attempts
	// and duplicate egress records must not inflate the request count.
	target, err := r.Target(ctx, id)
	require.NoError(t, err)
	ok, err := r.AcquireJob(ctx, "target", id, "stats-lease")
	require.NoError(t, err)
	require.True(t, ok)
	route := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: target.Revision, IdentityHash: "identity", CookieHash: uuid.NewString(), Gateway: "gateway", ExpiresAt: time.Now().Add(230 * time.Second)})
	require.NoError(t, r.Publish(ctx, route, "stats-lease"))
	passBorrowFixtureQuality(t)
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_requests(id,target_id,kind,lease_until,details) VALUES($1,$2,'probe',NOW()+INTERVAL '1 minute','{"candidate_id":"pending"}'),($3,$2,'probe',NOW()+INTERVAL '1 minute','{"candidate_id":"pending"}')`, uuid.NewString(), id, uuid.NewString())
	require.NoError(t, err)

	timing := map[string]any{"started_at": time.Now().Format(time.RFC3339Nano), "attempts": []map[string]any{
		{"kind": "egress", "borrow_route_id": route.ID, "terminal": "completed", "start_ms": 5, "events": map[string]float64{"first_visible": 2000, "first_semantic": 1000}},
		{"kind": "http", "borrow_route_id": route.ID, "terminal": "completed", "start_ms": 6, "events": map[string]float64{"first_visible": 2000}},
	}}
	raw, err := json.Marshal(timing)
	require.NoError(t, err)
	trace := uuid.NewString()
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO request_timing_details(usage_log_id,trace_id,detail) VALUES($1,$2,$3)`, id, trace, string(raw))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = integrationDB.Exec(`DELETE FROM request_timing_details WHERE trace_id=$1`, trace) })
	stats, err := r.(service.BorrowStatisticsRepository).BorrowStatistics(ctx, id, 15)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Passed)
	require.Equal(t, 1, stats.Published)
	require.Equal(t, 1, stats.TicketChanged)
	require.Equal(t, 1, stats.Timeout)
	require.Equal(t, 1, stats.Cancelled)
	require.Equal(t, 1, stats.Inflight)
	require.Zero(t, stats.Other)
	require.Equal(t, 1, stats.Prioritized)
	require.Equal(t, 1, stats.Rechecked)
	require.NotNil(t, stats.LastSuccessAt)
	require.Len(t, stats.Routes, 1)
	require.Equal(t, 1, stats.Routes[0].Requests)
	require.Equal(t, 1, stats.Routes[0].Completed)
	require.Equal(t, 2000.0, *stats.Routes[0].VisibleAverageMS)
	require.Equal(t, 1, stats.Routes[0].SemanticSamples)
}
