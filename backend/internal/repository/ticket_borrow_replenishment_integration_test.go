//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func replenishmentFixture(t *testing.T, shadow bool) (*ticketBorrowRepository, *service.ProxyPoolService, []service.BorrowSource, service.BorrowAcquisition) {
	t.Helper()
	r, pool, sources := rotationSourceFixture(t)
	_, id := borrowIntegrationFixture(t)
	cfg, err := r.Settings(t.Context())
	require.NoError(t, err)
	old := cfg
	t.Cleanup(func() {
		current, e := r.Settings(context.Background())
		if e == nil {
			old.Revision = current.Revision
			require.NoError(t, r.SaveSettings(context.Background(), old))
		}
	})
	cfg.Running = true
	cfg.AdmissionWindowSeconds, cfg.TTLSeconds, cfg.MinRouteRemainingSeconds = new(80), 0, nil
	cfg.Replenishment = &service.BorrowReplenishmentSettings{Enabled: true, Shadow: shadow}
	require.NoError(t, r.SaveSettings(t.Context(), cfg))
	target, err := r.Target(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, r.SetTargetIdentity(t.Context(), id, true, target.Revision, target.AcquisitionRevision, service.BorrowAcquisitionSource))
	a := service.BorrowAcquisition{TargetID: id, Revision: target.Revision, Identity: "identity", Lease: uuid.NewString()}
	ok, err := r.AcquireJob(service.WithBorrowAcquisition(t.Context(), a), "target", id, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	return r, pool, sources, a
}

func replenishmentRoute(t *testing.T, a service.BorrowAcquisition, source service.BorrowSource, ttl time.Duration) *service.BorrowRoute {
	start := time.Now()
	route := &service.BorrowRoute{AcquisitionIdentity: service.BorrowAcquisitionSource, AcquisitionAccountID: source.AccountID, AcquisitionStartedAt: &start, AdmissionUntil: start.Add(80 * time.Second), ID: uuid.NewString(), TargetID: a.TargetID, TargetRevision: a.Revision, IdentityHash: a.Identity,
		SourceID: source.ID, SourceRevision: source.Revision, SourceEnvironmentVersion: source.EnvironmentVersion, SourceRotationRevision: source.RotationRevision,
		CookieHash: uuid.NewString(), ExpiresAt: time.Now().Add(ttl)}
	require.NoError(t, integrationDB.QueryRow(`SELECT encode(sha256(convert_to(s.credentials->>'access_token','UTF8')),'hex'),encode(sha256(convert_to(COALESCE(s.credentials->>'chatgpt_account_id','')||':'||COALESCE(s.credentials->>'chatgpt_user_id',''),'UTF8')),'hex'),encode(sha256(convert_to(COALESCE(ta.credentials->>'chatgpt_account_id','')||':'||COALESCE(ta.credentials->>'chatgpt_user_id',''),'UTF8')),'hex') FROM accounts s CROSS JOIN accounts ta WHERE s.id=$1 AND ta.id=$2`, source.AccountID, a.TargetID).Scan(&route.SourceCredentialHash, &route.SourceSubjectHash, &route.TargetSubjectHash))
	return route
}

func TestTicketBorrowReplenishmentInventoryWithoutEnvironmentRetention(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	seed := replenishmentRoute(t, a, sources[0], 220*time.Second)
	require.NoError(t, r.Publish(ctx, seed, a.Lease))
	window, err := r.Replenishment(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Nil(t, window, "successful environments are never retained")
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "passed", 0))
	rotationExec(t, `UPDATE ticket_borrow_targets SET last_batch_started_at=clock_timestamp()-INTERVAL '2 seconds' WHERE account_id=$1`, a.TargetID)
	a.Lease = uuid.NewString()
	ctx = service.WithBorrowAcquisition(ctx, a)
	ok, err := r.AcquireJob(ctx, "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.True(t, ok, "a missing route is replenished without a maintenance interval")
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		route := replenishmentRoute(t, a, sources[0], 230*time.Second)
		wg.Add(1)
		go func() { defer wg.Done(); errs <- r.Publish(ctx, route, a.Lease) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var inventory, revoked, holds int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FILTER(WHERE inventory_eligible),count(*) FILTER(WHERE revoked_at IS NOT NULL) FROM ticket_borrow_routes WHERE target_id=$1`, a.TargetID).Scan(&inventory, &revoked))
	require.Equal(t, 2, inventory)
	require.Zero(t, revoked)
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_replenishment_holds WHERE account_id=$1`, sources[0].AccountID).Scan(&holds))
	require.Zero(t, holds)
	bound, err := r.Route(ctx, a.TargetID, a.Identity, seed.ID)
	require.NoError(t, err)
	require.Equal(t, seed.ID, bound.ID, "retiring inventory preserves existing sessions until cutoff")
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "passed", 0))
	inv, err := r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Equal(t, "healthy", inv.State)
	require.True(t, inv.NextAt.After(inv.ServerNow))
	ok, err = r.AcquireJob(ctx, "target", a.TargetID, "healthy-heartbeat")
	require.NoError(t, err)
	require.False(t, ok, "healthy inventory does not make periodic upstream requests")
}

func TestTicketBorrowReplenishmentPublicationUsesSingleDeadline(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := t.Context()
	short := replenishmentRoute(t, a, sources[0], 230*time.Second)
	deadline, err := r.ObserveCookie(ctx, short.CookieHash, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, r.Publish(ctx, short, a.Lease), "no second minimum-remaining threshold")
	got, err := r.Route(ctx, a.TargetID, a.Identity, short.ID)
	require.NoError(t, err)
	require.WithinDuration(t, deadline, got.UsableUntil, time.Millisecond)
	expired := replenishmentRoute(t, a, sources[0], 230*time.Second)
	expired.AdmissionUntil = time.Now().Add(-time.Second)
	require.ErrorIs(t, r.Publish(ctx, expired, a.Lease), service.ErrBorrowCandidateExpiry)
	legacy := replenishmentRoute(t, a, sources[0], 230*time.Second)
	legacy.Shadow = true
	require.EqualError(t, r.Publish(ctx, legacy, a.Lease), "borrow_shadow_unsupported")
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	cfg.Replenishment.Shadow = true
	require.EqualError(t, r.SaveSettings(ctx, cfg), "borrow_shadow_unsupported")
	rotationExec(t, `UPDATE ticket_borrow_routes SET validation_only=true,inventory_eligible=false WHERE id=$1`, short.ID)
	_, err = r.Route(ctx, a.TargetID, a.Identity, short.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
}

func TestTicketBorrowReplenishmentRejectsChangedCandidateIdentity(t *testing.T) {
	for _, role := range []string{"source", "target"} {
		t.Run(role, func(t *testing.T) {
			r, _, sources, a := replenishmentFixture(t, false)
			route := replenishmentRoute(t, a, sources[0], 230*time.Second)
			account := sources[0].AccountID
			if role == "target" {
				account = a.TargetID
			}
			rotationExec(t, `UPDATE accounts SET credentials=credentials||'{"chatgpt_account_id":"new-subject"}'::jsonb WHERE id=$1`, account)
			require.ErrorIs(t, r.Publish(t.Context(), route, a.Lease), service.ErrBorrowConflict)
			state, err := r.Replenishment(t.Context(), a.TargetID, a.Identity)
			require.NoError(t, err)
			require.Nil(t, state)
		})
	}
}

func TestTicketBorrowReplenishmentPreservesRequestBudgetsAndConcurrency(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	copy := *cfg.Recovery
	cfg.Recovery = &copy
	cfg.Recovery.ProbeRequestsPerMinute = 2
	cfg.Recovery.SourceRequestsPerMinute = 1
	require.NoError(t, r.SaveSettings(ctx, cfg))
	a = inventoryRestart(t, r, a)
	ctx = service.WithBorrowAcquisition(ctx, a)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	permit := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, sources[0].ID, permit))
	ok, err := r.GrantSourceAttempt(ctx, sources[0].ID, permit)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.SourceAttemptSent(ctx, permit))
	require.NoError(t, r.SourceAttemptCompleted(ctx, permit))
	require.NoError(t, r.FinishJob(ctx, "source", sources[0].ID, permit, "done", 0))
	second := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, sources[1].ID, second))
	_, err = r.GrantSourceAttempt(ctx, sources[1].ID, second)
	require.ErrorIs(t, err, service.ErrBorrowRequestBudget)
	requests := []service.BorrowRequest{}
	for range 2 {
		q := service.BorrowRequest{ID: uuid.NewString(), TargetID: a.TargetID, Revision: a.Revision, BatchID: a.Lease, Identity: a.Identity, Kind: "probe", Phase: "mint"}
		require.NoError(t, r.BeginTargetRequest(ctx, q))
		requests = append(requests, q)
	}
	third := requests[0]
	third.ID = uuid.NewString()
	require.ErrorIs(t, r.BeginTargetRequest(ctx, third), service.ErrBorrowProbeBusy)
	for _, q := range requests {
		require.NoError(t, r.EndTargetRequest(ctx, q, 200, "eof"))
	}
	require.ErrorIs(t, r.BeginTargetRequest(ctx, third), service.ErrBorrowRequestBudget)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease), "completed validation can publish when new HTTP budget is exhausted")
	_, err = r.Route(ctx, a.TargetID, a.Identity, "")
	require.NoError(t, err, "business admission is independent of acquisition budget")
}
