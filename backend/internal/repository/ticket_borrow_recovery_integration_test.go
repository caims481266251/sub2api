//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func borrowRecoveryFixture(t *testing.T) (service.TicketBorrowRepository, int64, service.BorrowRequest) {
	t.Helper()
	r, id := borrowIntegrationFixture(t)
	cfg, e := r.Settings(t.Context())
	require.NoError(t, e)
	original := cfg
	t.Cleanup(func() {
		latest, _ := r.Settings(context.Background())
		original.Revision = latest.Revision
		_ = r.SaveSettings(context.Background(), original)
	})
	cfg.Running = true
	cfg.Recovery = service.DefaultBorrowRecoverySettings()
	cfg.Recovery.FailureThreshold = 2
	require.NoError(t, r.SaveSettings(t.Context(), cfg))
	target, e := r.Target(t.Context(), id)
	require.NoError(t, e)
	ok, e := r.AcquireJob(t.Context(), "target", id, "worker")
	require.NoError(t, e)
	require.True(t, ok)
	return r, id, service.BorrowRequest{TargetID: id, Kind: "probe", BatchID: "worker", Revision: target.Revision, Phase: "mint"}
}
func recoveryRequest(t *testing.T, r service.TicketBorrowRepository, q service.BorrowRequest) service.BorrowRequest {
	t.Helper()
	q.ID = uuid.NewString()
	require.NoError(t, r.BeginTargetRequest(t.Context(), q))
	require.NoError(t, r.MarkTargetRequestSent(t.Context(), q))
	return q
}
func TestTicketBorrowRecoveryPauseAtEachPhaseAndBusinessAllowed(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	q = recoveryRequest(t, r, q)
	status, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, 1, status.Inflight)
	require.NotNil(t, status.LastRequestStartedAt)
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	cfg.Running = false
	require.NoError(t, r.SaveSettings(ctx, cfg))
	require.Error(t, r.CheckTargetRequest(ctx, q))
	require.Error(t, r.MarkTargetRequestSent(ctx, q))
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "eof"))
	require.NoError(t, r.EndTargetRequest(ctx, q, 200, "closed"))
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowPaused)
	business := recoveryRequest(t, r, service.BorrowRequest{TargetID: id, Kind: "business"})
	require.NoError(t, r.EndTargetRequest(ctx, business, 200, "eof"))
	status, e = r.Target(ctx, id)
	require.NoError(t, e)
	require.Zero(t, status.Inflight)
	require.NotNil(t, status.LastRequestFinishedAt)
}
func TestTicketBorrowOrdinaryFailuresNeverCooldownAndHardWaitCannotBeBypassed(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	for range 12 {
		require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_ticket_changed", 0))
	}
	status, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Nil(t, status.UpstreamWaitUntil)
	require.Nil(t, status.BudgetWaitUntil)
	require.Zero(t, status.RecoveryLevel)
	require.Equal(t, 12, status.ConsecutiveFailures)
	request := recoveryRequest(t, r, q)
	require.NoError(t, r.EndTargetRequest(ctx, request, 200, "eof"))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_rate_limited", 60*time.Second))
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowRecoveryWaiting)
	require.NoError(t, r.FinishJob(ctx, "target", id, "worker", "target_rate_limited", 0))
	require.NoError(t, r.RefreshTarget(ctx, id))
	require.NoError(t, r.ScanTarget(ctx, id, false))
	ok, e := r.AcquireJob(ctx, "target", id, "bypass")
	require.NoError(t, e)
	require.False(t, ok)
}
func TestTicketBorrowBusinessDoesNotExtendUpstreamWaitOrRouteExpiry(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, e := r.Now(ctx)
	require.NoError(t, e)
	route := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(220 * time.Second)})
	require.NoError(t, r.Publish(ctx, route, "worker"))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_rate_limited", 60500*time.Millisecond))
	before, e := r.Target(ctx, id)
	require.NoError(t, e)
	business := recoveryRequest(t, r, service.BorrowRequest{TargetID: id, Kind: "business"})
	require.NoError(t, r.EndTargetRequest(ctx, business, 200, "eof"))
	after, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, before.UpstreamWaitUntil, after.UpstreamWaitUntil)
	got, e := r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, e)
	require.WithinDuration(t, route.ExpiresAt, got.ExpiresAt, time.Millisecond)
	require.NoError(t, r.FinishJob(ctx, "target", id, "worker", "target_probe_passed", 0))
	_, e = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_targets SET upstream_wait_until=clock_timestamp()-INTERVAL '1 second' WHERE account_id=$1`, id)
	require.NoError(t, e)
	ok, e := r.AcquireJob(ctx, "target", id, "still-ready")
	require.NoError(t, e)
	require.False(t, ok, "a valid route prevents acquisition even when the upstream wait ended")
	_, e = r.ObserveCookie(ctx, route.CookieHash, -time.Second)
	require.NoError(t, e)
	ok, e = r.AcquireJob(ctx, "target", id, "expired")
	require.NoError(t, e)
	require.True(t, ok)
}
func TestTicketBorrowBudgetIndependentOfLegacySwitchAndCrashDoesNotExtendWait(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	cfg.Recovery.Enabled = false
	cfg.Recovery.ProbeRequestsPerMinute = 2
	require.NoError(t, r.SaveSettings(ctx, cfg))
	renewBorrowTestJob(t, r, q)
	for range 2 {
		request := recoveryRequest(t, r, q)
		require.NoError(t, r.EndTargetRequest(ctx, request, 200, "eof"))
	}
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowRequestBudget)
	status, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.Equal(t, 2, status.ProbeRequestsLastMinute)
	require.NotNil(t, status.BudgetWaitUntil)
	require.Nil(t, status.UpstreamWaitUntil)
	business := recoveryRequest(t, r, service.BorrowRequest{TargetID: id, Kind: "business"})
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_rate_limited", 120*time.Second))
	before, e := r.Target(ctx, id)
	require.NoError(t, e)
	require.NoError(t, r.FinishJob(ctx, "target", id, "worker", "target_rate_limited", 0))
	_, e = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_requests SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, business.ID)
	require.NoError(t, e)
	ok, e := r.AcquireJob(ctx, "target", id, "after-crash")
	require.NoError(t, e)
	require.False(t, ok)
	status, e = r.Target(ctx, id)
	require.NoError(t, e)
	require.Zero(t, status.Inflight)
	require.Equal(t, before.UpstreamWaitUntil, status.UpstreamWaitUntil)
}

func TestTicketBorrowRecoveryFiveBusinessRequestsDoNotSerializeOrLoseActivity(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	ctx := t.Context()
	now, err := r.Now(ctx)
	require.NoError(t, err)
	route := borrowAdmissionFixture(&service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(3 * time.Minute)})
	require.NoError(t, r.Publish(ctx, route, "worker"))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_ticket_changed", 0))
	require.NoError(t, r.RecordProbeOutcome(ctx, id, "worker", "target_ticket_changed", 0))
	var wg sync.WaitGroup
	errors := make(chan error, 10)
	requests := make([]service.BorrowRequest, 5)
	// Valid-route business remains concurrent while acquisition is stopped.
	for i := range requests {
		requests[i] = service.BorrowRequest{ID: uuid.NewString(), TargetID: id, Kind: "business"}
		wg.Add(1)
		go func(i int) { defer wg.Done(); errors <- r.BeginTargetRequest(ctx, requests[i]) }(i)
	}
	wg.Wait()
	for range requests {
		require.NoError(t, <-errors)
	}
	status, err := r.Target(ctx, id)
	require.NoError(t, err)
	require.Zero(t, status.Inflight, "business requests do not enter probe tracking")
	_, err = r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err)
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := r.MarkTargetRequestSent(ctx, requests[i]); err != nil {
				errors <- err
				return
			}
			if err := r.EndTargetRequest(ctx, requests[i], 200, "eof"); err != nil {
				errors <- err
				return
			}
			errors <- r.EndTargetRequest(ctx, requests[i], 200, "closed")
		}(i)
	}
	wg.Wait()
	for range requests {
		require.NoError(t, <-errors)
	}
	status, err = r.Target(ctx, id)
	require.NoError(t, err)
	require.Zero(t, status.Inflight)
	require.Nil(t, status.UpstreamWaitUntil)
	got, err := r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err)
	require.WithinDuration(t, route.ExpiresAt, got.ExpiresAt, time.Millisecond)
}

// Configuration edits fence the old batch. Tests focused on budgets/policy
// start a fresh permit after draining, rather than silently rebasing a lease.
func renewBorrowTestJob(t *testing.T, r service.TicketBorrowRepository, q service.BorrowRequest) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, r.FinishJob(ctx, "target", q.TargetID, q.BatchID, "configuration_changed", 0))
	require.NoError(t, r.RefreshTarget(ctx, q.TargetID))
	ok, err := r.AcquireJob(service.WithBorrowAcquisition(ctx, service.BorrowAcquisition{TargetID: q.TargetID, Revision: q.Revision, Identity: q.Identity}), "target", q.TargetID, q.BatchID)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestTicketBorrowProbeDiagnosticPersistenceAndPhaseStatistics(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	q.Environment = &service.BorrowEnvironment{IdentityHash: "reservation-identity"}
	q = recoveryRequest(t, r, q)
	// The connected identity must survive final diagnostic merging.
	q.Environment = &service.BorrowEnvironment{IdentityHash: "connected-identity"}
	_, err := integrationDB.Exec(`UPDATE ticket_borrow_requests SET details=jsonb_set(details,'{environment}', '{"identity_hash":"connected-identity"}') WHERE id=$1`, q.ID)
	require.NoError(t, err)
	q.Diagnostic = &service.BorrowHTTPAttempt{Phase: "mint", Number: 2, DurationMS: 1500, Status: 200, Events: map[string]int64{"response_headers": 300, "completed": 1400, "eof": 1500}, Terminal: "completed"}
	require.NoError(t, r.EndTargetRequest(t.Context(), q, 200, "eof"))
	// Idempotent completion cannot replace evidence with later cleanup.
	q.Diagnostic.ErrorClass = "cancelled"
	require.NoError(t, r.EndTargetRequest(t.Context(), q, 200, "closed"))
	var raw []byte
	require.NoError(t, integrationDB.QueryRow(`SELECT details FROM ticket_borrow_requests WHERE id=$1`, q.ID).Scan(&raw))
	var saved struct {
		Diagnostic  service.BorrowHTTPAttempt `json:"diagnostic"`
		Environment service.BorrowEnvironment `json:"environment"`
	}
	require.NoError(t, json.Unmarshal(raw, &saved))
	require.Empty(t, saved.Diagnostic.ErrorClass)
	require.Equal(t, "connected-identity", saved.Environment.IdentityHash)
	next := q
	next.ID = uuid.NewString()
	next.Phase = "continue"
	next.Diagnostic = nil
	require.NoError(t, r.BeginTargetRequest(t.Context(), next))
	next.Diagnostic = &service.BorrowHTTPAttempt{Phase: "continue", Number: 1, DurationMS: 7005, ErrorClass: "timeout", TimeoutStage: "eof", CancellationCause: "continue_deadline", Events: map[string]int64{"completed": 6000}}
	require.NoError(t, r.EndTargetRequest(t.Context(), next, 200, "target_continue_timeout"))
	// A historical attempt counts as a request, without fabricated timing/retries.
	_, err = integrationDB.Exec(`INSERT INTO ticket_borrow_requests(id,target_id,kind,phase,lease_until,finished_at,details) VALUES($1,$2,'probe','mint',NOW()+INTERVAL '1 minute',NOW(),'{}')`, uuid.NewString(), id)
	require.NoError(t, err)
	stats, err := r.(service.BorrowStatisticsRepository).BorrowStatistics(t.Context(), id, 15)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Mint.Requests)
	require.Equal(t, 1, stats.Mint.DiagnosticSamples)
	require.Equal(t, 1, stats.Mint.Retries)
	require.Equal(t, 1500.0, *stats.Mint.AverageMS)
	require.Equal(t, 1, stats.Continue.Requests)
	require.Equal(t, 1, stats.Continue.Timeouts)
	require.Equal(t, 1, stats.Continue.TimeoutStages["eof"])
	require.Equal(t, 1, stats.Continue.Cancellations["continue_deadline"])
}
