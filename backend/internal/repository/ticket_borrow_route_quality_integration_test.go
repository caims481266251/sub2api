//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBorrowRouteQualityPendingFailedAndOverdueAdmission(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	old, lease := poolReserve(t, r, id, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_routes SET state='pending_quality',quality_result='{}',quality_valid_until=NULL WHERE id=$1`, old.ID)
	_, err := r.ReserveRoute(t.Context(), id, "identity", old.ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
	job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	o := completedPoolQuality(t, r, job)
	o.Success = false
	o.Quality.Status = "failed"
	o.Quality.Answer = "29"
	o.Quality.Reason = "route_quality_failed"
	o.Reason = o.Quality.Reason
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, o))
	var retrySeconds float64
	require.NoError(t, r.db.QueryRowContext(t.Context(), `SELECT EXTRACT(EPOCH FROM cooldown_until-clock_timestamp()) FROM ticket_borrow_routes WHERE id=$1`, old.ID).Scan(&retrySeconds))
	require.InDelta(t, 30, retrySeconds, 2, "quality failure retries after 30 seconds, independently of the business cooldown")
	require.NoError(t, r.ObserveRouteOutcome(t.Context(), service.BorrowRouteOutcome{Route: old, Lease: lease, Success: true}))
	require.NoError(t, r.RenewRoute(t.Context(), lease), "failure never cancels a sent request")
	_, err = r.ReserveRoute(t.Context(), id, "identity", old.ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.NoError(t, r.ReleaseRoute(t.Context(), lease))
	rotationExec(t, `UPDATE ticket_borrow_routes SET cooldown_until=clock_timestamp()-INTERVAL '1 second',health_checked_at=NULL WHERE id=$1`, old.ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
	job, err = r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, r, job)))
	_, next := poolReserve(t, r, id, old.ID)
	rotationExec(t, `UPDATE ticket_borrow_routes SET quality_valid_until=clock_timestamp()-INTERVAL '1 second',last_success_at=clock_timestamp() WHERE id=$1`, old.ID)
	_, err = r.ReserveRoute(t.Context(), id, "identity", old.ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "recent business cannot extend quality validity")
	require.NoError(t, r.RenewRoute(t.Context(), next))
	require.NoError(t, r.ReleaseRoute(t.Context(), next))
	rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
	job, err = r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, r, job)))
}

func TestBorrowRouteQualityConcurrentChecksAndConfigFence(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, id)
	rotationExec(t, `UPDATE ticket_borrow_targets SET probe_policy_override=true,probe_concurrency=2 WHERE account_id=$1`, id)
	rotationExec(t, `UPDATE ticket_borrow_routes SET quality_valid_until=clock_timestamp()-INTERVAL '1 second' WHERE target_id=$1`, id)
	jobs := []*service.BorrowRouteHealthJob{}
	for range 2 {
		rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
		job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
		require.NoError(t, err)
		require.NotNil(t, job)
		jobs = append(jobs, job)
	}
	require.NotEqual(t, jobs[0].Route.ID, jobs[1].Route.ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
	third, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, third)
	// A single slow route does not own a global verifier, but the target's
	// configured concurrency is still shared with candidate probing.
	o := completedPoolQuality(t, r, jobs[0])
	cfg, err := r.Settings(t.Context())
	require.NoError(t, err)
	old := *cfg.RouteQuality
	t.Cleanup(func() {
		current, e := r.Settings(context.Background())
		require.NoError(t, e)
		current.RouteQuality = &old
		require.NoError(t, r.SaveSettings(context.Background(), current))
	})
	updated := *cfg.RouteQuality
	updated.Model = "gpt-6.1-sol"
	cfg.RouteQuality = &updated
	require.NoError(t, r.SaveSettings(t.Context(), cfg))
	require.ErrorIs(t, r.CheckRouteHealth(t.Context(), jobs[0], "identity"), service.ErrBorrowUnavailable)
	require.NoError(t, r.FinishRouteHealth(t.Context(), jobs[0], o))
	_, err = r.ReserveRoute(t.Context(), id, "identity", routes[0].ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.NoError(t, r.FinishRouteHealth(t.Context(), jobs[1], service.BorrowRouteOutcome{}))
}

func TestBorrowRouteQualityRequiresRecordedRequest(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE ticket_borrow_routes SET state='pending_quality',quality_valid_until=NULL WHERE id=$1`, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
	job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	fake := service.BorrowRouteOutcome{Success: true, Quality: &service.BorrowRouteQualityResult{Status: "passed", Model: "gpt-6-astra", ReasoningEffort: "medium", Answer: "21", HTTPStatus: 200, CheckedAt: time.Now()}}
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, fake))
	_, err = r.ReserveRoute(t.Context(), id, "identity", job.Route.ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
}

func TestBorrowRouteQualityEarlyRefresh(t *testing.T) {
	for _, outcome := range []string{"passed", "expired_while_checking", "failed", "business_429", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			r, id, routes := borrowRouteLoadFixture(t)
			rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, id)
			rotationExec(t, `UPDATE ticket_borrow_routes SET quality_result='{"status":"passed","duration_ms":10000}',quality_valid_until=clock_timestamp()+INTERVAL '20 seconds' WHERE id=$1`, routes[0].ID)
			rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
			job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
			require.NoError(t, err)
			require.Nil(t, job, "a recent pass must not start immediate repeated checks")
			rotationExec(t, `UPDATE ticket_borrow_routes SET quality_valid_until=clock_timestamp()+INTERVAL '10 seconds' WHERE id=$1`, routes[0].ID)
			job, err = r.AcquireRouteHealth(t.Context(), uuid.NewString())
			require.NoError(t, err)
			require.NotNil(t, job, "start before expiry using the last duration")
			require.Equal(t, routes[0].ID, job.Route.ID)
			require.EqualValues(t, 1, job.Route.Generation)
			states, err := r.RouteStatuses(t.Context(), id, "identity")
			require.NoError(t, err)
			for _, state := range states {
				if state.ID == job.Route.ID {
					require.True(t, state.Checking)
					require.True(t, state.Usable, "checking is compatible with a current pass")
					require.Equal(t, "normal", state.State)
				}
			}
			business, lease := poolReserve(t, r, id, job.Route.ID)
			require.NoError(t, r.CheckRouteReservation(t.Context(), business, lease), "the reserved request does not need another free slot")
			o := service.BorrowRouteOutcome{}
			if outcome != "cancelled" {
				o = completedPoolQuality(t, r, job)
			}
			switch outcome {
			case "expired_while_checking":
				rotationExec(t, `UPDATE ticket_borrow_routes SET quality_valid_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, job.Route.ID)
				_, err = r.ReserveRoute(t.Context(), id, "identity", job.Route.ID, "", uuid.NewString())
				require.ErrorIs(t, err, service.ErrBorrowNoEligibleRoute)
				require.Equal(t, "pending_quality", service.BorrowAdmissionReason(err))
			case "failed":
				o.Success = false
				o.Reason = "route_quality_failed"
				o.Quality.Status = "failed"
				o.Quality.Answer = "29"
			case "business_429":
				require.NoError(t, r.ObserveRouteOutcome(t.Context(), service.BorrowRouteOutcome{Route: business, Lease: lease, Reason: "route_rate_limited"}))
			}
			require.NoError(t, r.RenewRoute(t.Context(), lease), "sent business remains independent of admission state")
			if outcome == "passed" {
				require.NoError(t, r.ObserveRouteOutcome(t.Context(), service.BorrowRouteOutcome{Route: business, Lease: lease, Success: true}))
			}
			require.NoError(t, r.ReleaseRoute(t.Context(), lease))
			require.NoError(t, r.FinishRouteHealth(t.Context(), job, o))
			var state string
			var generation int64
			var remaining float64
			require.NoError(t, r.db.QueryRowContext(t.Context(), `SELECT state,generation,COALESCE(EXTRACT(EPOCH FROM quality_valid_until-clock_timestamp()),0) FROM ticket_borrow_routes WHERE id=$1`, job.Route.ID).Scan(&state, &generation, &remaining))
			if outcome == "failed" || outcome == "business_429" {
				require.Equal(t, "cooling", state)
				require.EqualValues(t, 2, generation)
				_, err = r.ReserveRoute(t.Context(), id, "identity", job.Route.ID, "", uuid.NewString())
				require.ErrorIs(t, err, service.ErrBorrowNoEligibleRoute)
				require.Equal(t, "cooling", service.BorrowAdmissionReason(err))
			} else {
				require.Equal(t, "normal", state)
				require.EqualValues(t, 1, generation)
				if outcome == "cancelled" {
					require.Positive(t, remaining)
					require.LessOrEqual(t, remaining, 10.0, "cancellation preserves but never extends the old pass")
				} else {
					require.InDelta(t, 30, remaining, 2)
				}
				_, next := poolReserve(t, r, id, job.Route.ID)
				require.NoError(t, r.ReleaseRoute(t.Context(), next))
			}
		})
	}
}

func TestBorrowRouteAdmissionStatusMatchesCapacity(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=2 WHERE id=$1`, id)
	assertStatus := func(routeID, reason string, slots int) {
		t.Helper()
		states, err := r.RouteStatuses(t.Context(), id, "identity")
		require.NoError(t, err)
		for _, state := range states {
			if state.ID != routeID {
				continue
			}
			require.Equal(t, slots > 0, state.Usable)
			require.Equal(t, slots, state.AvailableSlots)
			require.Equal(t, reason, state.UnavailableReason)
			if reason != "" {
				_, err = r.ReserveRoute(t.Context(), id, "identity", routeID, "", uuid.NewString())
				require.ErrorIs(t, err, service.ErrBorrowNoEligibleRoute)
				require.Equal(t, reason, service.BorrowAdmissionReason(err))
			}
			return
		}
		t.Fatal("route missing from status")
	}
	assertStatus(routes[0].ID, "", 2)
	first, lease := poolReserve(t, r, id, routes[0].ID)
	assertStatus(first.ID, "", 1)
	assertStatus(routes[1].ID, "", 1)
	require.NoError(t, r.CheckRouteReservation(t.Context(), first, lease))
	probe := uuid.NewString()
	rotationExec(t, `INSERT INTO ticket_borrow_requests(id,target_id,kind,batch_id,phase,lease_until) VALUES($1,$2,'probe',$1,'route_health',clock_timestamp()+INTERVAL '60 seconds')`, probe, id)
	assertStatus(routes[1].ID, "account_busy", 0)
	require.NoError(t, r.CheckRouteReservation(t.Context(), first, lease))
	rotationExec(t, `UPDATE ticket_borrow_requests SET finished_at=clock_timestamp() WHERE id=$1`, probe)
	assertStatus(routes[1].ID, "", 1)
	require.NoError(t, r.ReleaseRoute(t.Context(), lease))
	assertStatus(first.ID, "", 2)
	rotationExec(t, `UPDATE ticket_borrow_targets SET upstream_wait_until=clock_timestamp()+INTERVAL '1 minute' WHERE account_id=$1`, id)
	assertStatus(first.ID, "account_cooling", 0)
	rotationExec(t, `UPDATE ticket_borrow_targets SET upstream_wait_until=NULL WHERE account_id=$1`, id)
	rotationExec(t, `UPDATE accounts SET rate_limit_reset_at=clock_timestamp()+INTERVAL '1 minute' WHERE id=$1`, id)
	assertStatus(first.ID, "account_cooling", 0)
	rotationExec(t, `UPDATE accounts SET rate_limit_reset_at=NULL WHERE id=$1`, id)
	rotationExec(t, `UPDATE ticket_borrow_routes SET quality_valid_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, first.ID)
	assertStatus(first.ID, "pending_quality", 0)
}
