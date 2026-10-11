//go:build integration

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowProbeConcurrencyPerTargetAndBusinessIsolation(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	_, otherID, otherQ := borrowRecoveryFixture(t)
	renewBorrowTestJob(t, r, q)
	ctx := t.Context()
	for _, target := range []struct {
		id int64
		q  service.BorrowRequest
	}{{id, q}, {otherID, otherQ}} {
		status, err := r.Target(ctx, target.id)
		require.NoError(t, err)
		require.Equal(t, 2, status.ProbePolicy.Concurrency)
		var wg sync.WaitGroup
		results := make(chan error, 12)
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				q := target.q
				q.ID = uuid.NewString()
				results <- r.BeginTargetRequest(ctx, q)
			}()
		}
		wg.Wait()
		close(results)
		allowed := 0
		for err := range results {
			if err == nil {
				allowed++
			} else {
				require.ErrorIs(t, err, service.ErrBorrowProbeBusy)
			}
		}
		require.Equal(t, 2, allowed)
		status, err = r.Target(ctx, target.id)
		require.NoError(t, err)
		require.Equal(t, 2, status.Inflight)
		for range 5 {
			b := service.BorrowRequest{ID: uuid.NewString(), TargetID: target.id, Kind: "business"}
			require.NoError(t, r.BeginTargetRequest(ctx, b))
			require.NoError(t, r.MarkTargetRequestSent(ctx, b))
			require.NoError(t, r.EndTargetRequest(ctx, b, 200, "eof"))
		}
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM ticket_borrow_requests WHERE target_id IN ($1,$2) AND kind='probe' AND finished_at IS NULL`, id, otherID).Scan(&count))
	require.Equal(t, 4, count, "two targets must have four independent probe reservations")
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM ticket_borrow_requests WHERE target_id IN ($1,$2) AND kind='business'`, id, otherID).Scan(&count))
	require.Zero(t, count)
	// Other targets can start even when more than three target jobs already exist.
	for range 3 {
		_, targetID := borrowIntegrationFixture(t)
		ok, err := r.AcquireJob(ctx, "target", targetID, uuid.NewString())
		require.NoError(t, err)
		require.True(t, ok)
	}
	// Lowering a live target limit retains existing requests and blocks new ones.
	status, err := r.Target(ctx, id)
	require.NoError(t, err)
	p := status.ProbePolicy
	p.UseGlobal = false
	p.Concurrency = 1
	require.NoError(t, r.(service.BorrowProbePolicyRepository).SaveProbePolicy(ctx, id, p))
	q.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, q), service.ErrBorrowUnavailable, "policy edit fences old batch")
	_, err = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_requests SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE target_id=$1`, id)
	require.NoError(t, err)
	renewBorrowTestJob(t, r, q)
	require.NoError(t, r.BeginTargetRequest(ctx, q))
	status, err = r.Target(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 1, status.Inflight)
}

func TestTicketBorrowProbePolicyPersistsWithoutInvalidatingRoutes(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	borrowSetAdmissionFloor(t, r, 0) // Isolate the existing probe-policy behavior.
	ctx := t.Context()
	settings, err := r.Settings(ctx)
	require.NoError(t, err)
	original := settings
	t.Cleanup(func() {
		current, _ := r.Settings(context.Background())
		original.Revision = current.Revision
		_ = r.SaveSettings(context.Background(), original)
	})
	settings.RenewBeforeSeconds = new(15) // Exercise a saved, non-default global policy.
	require.NoError(t, r.SaveSettings(ctx, settings))
	before, err := r.Target(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 15, before.ProbePolicy.RenewBeforeSeconds)
	now, err := r.Now(ctx)
	require.NoError(t, err)
	renewBorrowTestJob(t, r, q)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, SourceID: 1, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(40 * time.Second)}
	require.NoError(t, r.Publish(ctx, route, q.BatchID))
	_, err = r.BindSession(ctx, id, "session", route.ID)
	require.NoError(t, err)
	require.NoError(t, r.FinishJob(ctx, "target", id, q.BatchID, "target_probe_passed", 0))
	ok, err := r.AcquireJob(ctx, "target", id, "too-early")
	require.NoError(t, err)
	require.False(t, ok)
	policy := before.ProbePolicy
	policy.UseGlobal = false
	policy.RenewBeforeSeconds = 60
	policy.Concurrency = 5
	require.NoError(t, r.(service.BorrowProbePolicyRepository).SaveProbePolicy(ctx, id, policy))
	require.ErrorIs(t, r.(service.BorrowProbePolicyRepository).SaveProbePolicy(ctx, id, policy), service.ErrBorrowConflict)
	fresh := NewTicketBorrowRepository(integrationDB, r.(*ticketBorrowRepository).encryptor)
	after, err := fresh.Target(ctx, id)
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, policy.Revision+1, after.ProbePolicy.Revision)
	require.Equal(t, 5, after.ProbePolicy.Concurrency)
	require.Equal(t, 60, after.ProbePolicy.RenewBeforeSeconds)
	old, err := fresh.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err)
	require.Equal(t, route.ID, old.ID)
	bound, found, err := fresh.LookupSession(ctx, id, "session")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, route.ID, bound)
	ok, err = fresh.AcquireJob(ctx, "target", id, "renewal")
	require.NoError(t, err)
	require.True(t, ok)
	q.ID, q.BatchID = uuid.NewString(), "renewal"
	require.NoError(t, fresh.BeginTargetRequest(ctx, q))
	require.NoError(t, fresh.EndTargetRequest(ctx, q, 200, "eof"))
	_, err = fresh.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err, "probing must preserve the old route")
	replacement := *route
	replacement.ID = uuid.NewString()
	replacement.CookieHash = uuid.NewString()
	replacement.ExpiresAt = now.Add(2 * time.Minute)
	require.NoError(t, fresh.Publish(ctx, &replacement, "renewal"))
	require.NoError(t, fresh.CheckAcquisition(ctx, service.BorrowAcquisition{TargetID: id, Revision: after.Revision, Lease: "renewal", Identity: "identity"}))
	_, err = fresh.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err, "publication must preserve old continuations")
}

func TestTicketBorrowProbeThresholdUsesCookieDeadlineAndZeroWindow(t *testing.T) {
	r, id, q := borrowRecoveryFixture(t)
	borrowSetAdmissionFloor(t, r, 0) // Both zero thresholds retain expiry-only behavior.
	ctx := t.Context()
	now, err := r.Now(ctx)
	require.NoError(t, err)
	renewBorrowTestJob(t, r, q)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: q.Revision, SourceID: 1, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(2 * time.Minute)}
	require.NoError(t, r.Publish(ctx, route, q.BatchID))
	require.NoError(t, r.FinishJob(ctx, "target", id, q.BatchID, "target_probe_passed", 0))
	_, err = r.ObserveCookie(ctx, route.CookieHash, 10*time.Second)
	require.NoError(t, err)
	ok, err := r.AcquireJob(ctx, "target", id, "short-cookie")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, err)
	require.NoError(t, r.FinishJob(ctx, "target", id, "short-cookie", "none", 0))
	status, err := r.Target(ctx, id)
	require.NoError(t, err)
	policy := status.ProbePolicy
	policy.UseGlobal = false
	policy.RenewBeforeSeconds = 0
	require.NoError(t, r.(service.BorrowProbePolicyRepository).SaveProbePolicy(ctx, id, policy))
	ok, err = r.AcquireJob(ctx, "target", id, "zero-window")
	require.NoError(t, err)
	require.False(t, ok)
	_, err = integrationDB.ExecContext(ctx, `UPDATE ticket_borrow_cookie_deadlines SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE cookie_hash=$1`, route.CookieHash)
	require.NoError(t, err)
	ok, err = r.AcquireJob(ctx, "target", id, "expired")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestTicketBorrowHistoryExcludesBusinessIncludingLegacyEvents(t *testing.T) {
	r, id := borrowIntegrationFixture(t)
	ctx := t.Context()
	require.NoError(t, r.Event(ctx, service.BorrowEvent{TargetID: id, Stage: "business", Reason: "business_completed"}))
	_, err := integrationDB.ExecContext(ctx, `INSERT INTO ticket_borrow_events(target_id,stage,reason) VALUES($1,'business','historical'),($1,'target','target_probe_passed'),($1,'route','target_route_changed')`, id)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM ticket_borrow_events WHERE target_id=$1`, id)
	})
	events, err := r.Events(ctx, 0, id)
	require.NoError(t, err)
	require.Len(t, events, 2)
	for _, event := range events {
		require.NotEqual(t, "business", event.Stage)
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT count(*) FROM ticket_borrow_events WHERE target_id=$1 AND reason='business_completed'`, id).Scan(&count))
	require.Zero(t, count)
	// Both repository and database reject invalid limits.
	target, err := r.Target(ctx, id)
	require.NoError(t, err)
	for _, p := range []service.BorrowProbePolicy{{Revision: target.ProbePolicy.Revision, Concurrency: 0}, {Revision: target.ProbePolicy.Revision, Concurrency: 33}, {Revision: target.ProbePolicy.Revision, Concurrency: 3, RenewBeforeSeconds: -1}, {Revision: target.ProbePolicy.Revision, Concurrency: 3, RenewBeforeSeconds: 241}} {
		require.True(t, errors.Is(r.(service.BorrowProbePolicyRepository).SaveProbePolicy(ctx, id, p), service.ErrBorrowProbePolicyInvalid))
	}
}
