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
	cfg.Replenishment = &service.BorrowReplenishmentSettings{Enabled: true, Inventory: 2, IntervalSeconds: 10, HoldSeconds: 150, Shadow: shadow}
	require.NoError(t, r.SaveSettings(t.Context(), cfg))
	target, err := r.Target(t.Context(), id)
	require.NoError(t, err)
	a := service.BorrowAcquisition{TargetID: id, Revision: target.Revision, Identity: "identity", Lease: uuid.NewString()}
	ok, err := r.AcquireJob(service.WithBorrowAcquisition(t.Context(), a), "target", id, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	return r, pool, sources, a
}

func replenishmentRoute(t *testing.T, a service.BorrowAcquisition, source service.BorrowSource, ttl time.Duration) *service.BorrowRoute {
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: a.TargetID, TargetRevision: a.Revision, IdentityHash: a.Identity,
		SourceID: source.ID, SourceRevision: source.Revision, SourceEnvironmentVersion: source.EnvironmentVersion, SourceRotationRevision: source.RotationRevision,
		CookieHash: uuid.NewString(), ExpiresAt: time.Now().Add(ttl)}
	require.NoError(t, integrationDB.QueryRow(`SELECT encode(sha256(convert_to(COALESCE(s.credentials->>'chatgpt_account_id','')||':'||COALESCE(s.credentials->>'chatgpt_user_id',''),'UTF8')),'hex'),encode(sha256(convert_to(COALESCE(ta.credentials->>'chatgpt_account_id','')||':'||COALESCE(ta.credentials->>'chatgpt_user_id',''),'UTF8')),'hex') FROM accounts s CROSS JOIN accounts ta WHERE s.id=$1 AND ta.id=$2`, source.AccountID, a.TargetID).Scan(&route.SourceSubjectHash, &route.TargetSubjectHash))
	return route
}

func TestTicketBorrowReplenishmentInventoryAndCadence(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	seed := replenishmentRoute(t, a, sources[0], 220*time.Second)
	require.NoError(t, r.Publish(ctx, seed, a.Lease))
	window, err := r.Replenishment(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.NotNil(t, window)
	require.WithinDuration(t, time.Now().Add(150*time.Second), window.Until, 2*time.Second)
	require.NoError(t, r.CheckAcquisition(ctx, a), "full inventory must allow bounded replenishment")
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "target_probe_passed", 0))
	a.Lease = uuid.NewString()
	ok, err := r.AcquireJob(service.WithBorrowAcquisition(ctx, a), "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.False(t, ok, "refill uses cadence from batch start")
	rotationExec(t, `UPDATE ticket_borrow_targets SET last_batch_started_at=clock_timestamp()-INTERVAL '11 seconds' WHERE account_id=$1`, a.TargetID)
	ok, err = r.AcquireJob(service.WithBorrowAcquisition(ctx, a), "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		route := replenishmentRoute(t, a, sources[0], time.Duration(225+i)*time.Second)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.Publish(ctx, route, a.Lease)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var inventory, revoked int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FILTER (WHERE inventory_eligible),count(*) FILTER (WHERE revoked_at IS NOT NULL) FROM ticket_borrow_routes WHERE target_id=$1`, a.TargetID).Scan(&inventory, &revoked))
	require.Equal(t, 2, inventory)
	require.Zero(t, revoked)
	bound, err := r.Route(ctx, a.TargetID, a.Identity, seed.ID)
	require.NoError(t, err)
	require.Equal(t, seed.ID, bound.ID, "retirement must preserve a bound session's route")
	latest, err := r.Route(ctx, a.TargetID, a.Identity, "")
	require.NoError(t, err)
	require.NotEqual(t, seed.ID, latest.ID)
	restarted := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	after, err := restarted.Replenishment(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Equal(t, window.Until, after.Until, "successes/restarts cannot extend a hold")
	require.EqualValues(t, 1, after.Attempts)
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "target_probe_passed", 0))
	ok, err = r.AcquireJob(ctx, "target", a.TargetID, "too-early")
	require.NoError(t, err)
	require.False(t, ok)
	rotationExec(t, `UPDATE ticket_borrow_targets SET last_batch_started_at=clock_timestamp()-INTERVAL '31 seconds' WHERE account_id=$1`, a.TargetID)
	ok, err = r.AcquireJob(ctx, "target", a.TargetID, "on-time")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestTicketBorrowReplenishmentFormalPublicationAndExpiry(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := t.Context()
	seed := replenishmentRoute(t, a, sources[0], 220*time.Second)
	require.NoError(t, r.Publish(ctx, seed, a.Lease))
	second := replenishmentRoute(t, a, sources[0], 230*time.Second)
	require.NoError(t, r.Publish(ctx, second, a.Lease))
	route, err := r.Route(ctx, a.TargetID, a.Identity, second.ID)
	require.NoError(t, err)
	require.Equal(t, second.ID, route.ID)
	short := replenishmentRoute(t, a, sources[0], 230*time.Second)
	_, err = r.ObserveCookie(ctx, short.CookieHash, 30*time.Second)
	require.NoError(t, err)
	require.ErrorIs(t, r.Publish(ctx, short, a.Lease), service.ErrBorrowCandidateExpiry)
	rotationExec(t, `UPDATE ticket_borrow_replenishment SET until_at=clock_timestamp()-INTERVAL '1 second' WHERE target_id=$1`, a.TargetID)
	state, err := r.Replenishment(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Nil(t, state)
	require.NoError(t, r.CheckAcquisition(ctx, a), "hold expiry must not stop inventory maintenance")
	legacy := replenishmentRoute(t, a, sources[0], 230*time.Second)
	legacy.Shadow = true
	require.EqualError(t, r.Publish(ctx, legacy, a.Lease), "borrow_shadow_unsupported")
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	cfg.Replenishment.Shadow = true
	require.EqualError(t, r.SaveSettings(ctx, cfg), "borrow_shadow_unsupported")
	rotationExec(t, `UPDATE ticket_borrow_routes SET validation_only=true,inventory_eligible=false WHERE id=$1`, seed.ID)
	_, err = r.Route(ctx, a.TargetID, a.Identity, seed.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
}

func TestTicketBorrowReplenishmentInvalidationAndMismatch(t *testing.T) {
	for _, change := range []string{"settings", "policy", "selection", "source_version", "source_identity", "target_identity", "target_tls", "source_429", "target_429", "mismatch"} {
		t.Run(change, func(t *testing.T) {
			r, _, sources, a := replenishmentFixture(t, false)
			ctx := t.Context()
			seed := replenishmentRoute(t, a, sources[0], 230*time.Second)
			require.NoError(t, r.Publish(ctx, seed, a.Lease))
			switch change {
			case "settings":
				cfg, err := r.Settings(ctx)
				require.NoError(t, err)
				cfg.Replenishment = nil // older clients must preserve saved policy
				require.NoError(t, r.SaveSettings(ctx, cfg))
				cfg, err = r.Settings(ctx)
				require.NoError(t, err)
				require.True(t, cfg.Replenishment.Enabled)
			case "policy":
				rotationExec(t, `UPDATE ticket_borrow_targets SET probe_policy_revision=probe_policy_revision+1 WHERE account_id=$1`, a.TargetID)
			case "selection":
				rotationExec(t, `UPDATE ticket_borrow_selection SET revision=revision+1 WHERE id=1`)
			case "source_version":
				rotationExec(t, `UPDATE ticket_borrow_proxy_bindings SET environment_version=environment_version+1 WHERE account_id=$1`, sources[0].AccountID)
			case "source_identity":
				rotationExec(t, `UPDATE accounts SET credentials=credentials||'{"chatgpt_account_id":"changed"}'::jsonb WHERE id=$1`, sources[0].AccountID)
			case "target_identity":
				rotationExec(t, `UPDATE accounts SET credentials=credentials||'{"chatgpt_account_id":"changed"}'::jsonb WHERE id=$1`, a.TargetID)
			case "target_tls":
				rotationExec(t, `UPDATE accounts SET extra=COALESCE(extra,'{}'::jsonb)||'{"enable_tls_fingerprint":true}'::jsonb WHERE id=$1`, a.TargetID)
			case "source_429":
				rotationExec(t, `UPDATE accounts SET rate_limit_reset_at=clock_timestamp()+INTERVAL '60 seconds' WHERE id=$1`, sources[0].AccountID)
			case "target_429":
				require.NoError(t, r.RecordProbeOutcome(ctx, a.TargetID, a.Lease, "target_rate_limited", time.Minute))
			case "mismatch":
				require.NoError(t, r.EndReplenishment(ctx, a, sources[0].ID, "target_ticket_changed"))
			}
			state, err := r.Replenishment(ctx, a.TargetID, a.Identity)
			require.NoError(t, err)
			require.Nil(t, state)
			_, err = r.Route(ctx, a.TargetID, a.Identity, seed.ID)
			require.NoError(t, err, "priority loss does not revoke existing routes")
		})
	}
}

func TestTicketBorrowReplenishmentDoesNotHoldChangedCandidateIdentity(t *testing.T) {
	for _, role := range []string{"source", "target"} {
		t.Run(role, func(t *testing.T) {
			r, _, sources, a := replenishmentFixture(t, false)
			route := replenishmentRoute(t, a, sources[0], 230*time.Second)
			account := sources[0].AccountID
			if role == "target" {
				account = a.TargetID
			}
			rotationExec(t, `UPDATE accounts SET credentials=credentials||'{"chatgpt_account_id":"new-subject"}'::jsonb WHERE id=$1`, account)
			if role == "target" {
				require.ErrorIs(t, r.Publish(t.Context(), route, a.Lease), service.ErrBorrowConflict)
			} else {
				require.NoError(t, r.Publish(t.Context(), route, a.Lease))
			}
			state, err := r.Replenishment(t.Context(), a.TargetID, a.Identity)
			require.NoError(t, err)
			require.Nil(t, state, "in-flight results cannot establish priority for a new subject")
		})
	}
}

func TestTicketBorrowReplenishmentExpiredHoldReleasesFullPool(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := t.Context()
	for _, s := range sources {
		require.NoError(t, r.SaveRotationPolicy(ctx, s.AccountID, service.BorrowRotationPolicy{Automatic: true, IntervalSeconds: 30}))
	}
	sources, err := r.Sources(ctx)
	require.NoError(t, err)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	rotationExec(t, `UPDATE ticket_borrow_rotation_accounts SET next_at=clock_timestamp()-INTERVAL '1 second' WHERE account_id IN ($1,$2)`, sources[0].AccountID, sources[1].AccountID)
	// With two occupied endpoints there is no alternative for the unheld
	// source alone. The pool must not steal the retained source's binding.
	require.NoError(t, r.ProcessRotations(ctx))
	retained, err := r.Source(ctx, sources[0].ID)
	require.NoError(t, err)
	require.Equal(t, sources[0].EnvironmentVersion, retained.EnvironmentVersion)
	rotationExec(t, `UPDATE ticket_borrow_replenishment SET until_at=clock_timestamp()-INTERVAL '1 second' WHERE target_id=$1`, a.TargetID)
	rotationExec(t, `UPDATE ticket_borrow_rotation_accounts SET next_at=clock_timestamp()-INTERVAL '1 second' WHERE account_id IN ($1,$2)`, sources[0].AccountID, sources[1].AccountID)
	require.NoError(t, r.ProcessRotations(ctx))
	require.NoError(t, r.ProcessRotations(ctx))
	for _, old := range sources {
		current, err := r.Source(ctx, old.ID)
		require.NoError(t, err)
		require.NotEqual(t, old.EnvironmentVersion, current.EnvironmentVersion)
	}
}

func TestTicketBorrowReplenishmentAutomaticRotationAndManualOverride(t *testing.T) {
	r, pool, sources, a := replenishmentFixture(t, false)
	ctx := t.Context()
	rotationAppend(t, pool, "spare.example:82")
	for _, s := range sources {
		require.NoError(t, r.SaveRotationPolicy(ctx, s.AccountID, service.BorrowRotationPolicy{Automatic: true, IntervalSeconds: 30}))
	}
	var err error
	sources, err = r.Sources(ctx)
	require.NoError(t, err)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	rotationExec(t, `UPDATE ticket_borrow_rotation_accounts SET next_at=clock_timestamp()-INTERVAL '1 second' WHERE account_id IN ($1,$2)`, sources[0].AccountID, sources[1].AccountID)
	require.NoError(t, r.ProcessRotations(ctx))
	require.NoError(t, r.ProcessRotations(ctx))
	retained, err := r.Source(ctx, sources[0].ID)
	require.NoError(t, err)
	changed, err := r.Source(ctx, sources[1].ID)
	require.NoError(t, err)
	require.Equal(t, sources[0].EnvironmentVersion, retained.EnvironmentVersion)
	require.NotEqual(t, sources[1].EnvironmentVersion, changed.EnvironmentVersion)
	job, err := r.QueueRotation(ctx, rotationInput("source", sources[0].AccountID), "manual")
	require.NoError(t, err)
	require.NoError(t, r.ProcessRotations(ctx))
	require.Equal(t, "completed", rotationPhase(t, job.ID))
	state, err := r.Replenishment(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Nil(t, state)
	var automatic bool
	var interval int
	require.NoError(t, integrationDB.QueryRow(`SELECT automatic,interval_seconds FROM ticket_borrow_rotation_accounts WHERE account_id=$1`, sources[0].AccountID).Scan(&automatic, &interval))
	require.True(t, automatic)
	require.Equal(t, 30, interval, "temporary hold never rewrites saved rotation policy")
}

func TestTicketBorrowReplenishmentSharesFixedHoldAcrossTargets(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := t.Context()
	_, second := borrowIntegrationFixture(t)
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	require.NoError(t, r.SaveSettings(ctx, cfg))
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "configuration_changed", 0))
	a.Lease = uuid.NewString()
	acquired, e := r.AcquireJob(service.WithBorrowAcquisition(ctx, a), "target", a.TargetID, a.Lease)
	require.NoError(t, e)
	require.True(t, acquired)
	ctx = service.WithBorrowAcquisition(ctx, a)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	first, err := r.Replenishment(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	target, err := r.Target(ctx, second)
	require.NoError(t, err)
	b := service.BorrowAcquisition{TargetID: second, Revision: target.Revision, Identity: a.Identity, Lease: uuid.NewString()}
	ok, err := r.AcquireJob(service.WithBorrowAcquisition(ctx, b), "target", second, b.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, b, sources[0], 230*time.Second), b.Lease))
	next, err := r.Replenishment(ctx, b.TargetID, b.Identity)
	require.NoError(t, err)
	require.Equal(t, first.Until, next.Until, "another target cannot refresh the same source/environment deadline")
	rotationExec(t, `UPDATE ticket_borrow_replenishment SET until_at=clock_timestamp()-INTERVAL '1 second' WHERE target_id IN ($1,$2)`, a.TargetID, b.TargetID)
	require.NoError(t, r.expireBorrowReplenishment(ctx))
	require.NoError(t, r.expireBorrowReplenishment(ctx))
	var ended int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_events WHERE target_id IN ($1,$2) AND reason='replenishment_ended'`, a.TargetID, b.TargetID).Scan(&ended))
	require.Equal(t, 2, ended, "expiry emits once, even after repeated scheduler ticks")
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
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "configuration_changed", 0))
	a.Lease = uuid.NewString()
	acquired, e := r.AcquireJob(service.WithBorrowAcquisition(ctx, a), "target", a.TargetID, a.Lease)
	require.NoError(t, e)
	require.True(t, acquired)
	ctx = service.WithBorrowAcquisition(ctx, a)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_requests WHERE target_id=$1`, a.TargetID)
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_source_attempts WHERE source_id=$1`, sources[0].ID)
	})
	ok, err := r.AcquireJob(ctx, "source", sources[0].ID, "source-worker")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.ReserveSourceAttempt(ctx, sources[0].ID, "source-worker"))
	require.ErrorIs(t, r.ReserveSourceAttempt(ctx, sources[0].ID, "source-worker"), service.ErrBorrowRequestBudget)
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
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease), "completed validation may publish even when new HTTP budget is exhausted")
	_, err = r.Route(ctx, a.TargetID, a.Identity, "")
	require.NoError(t, err, "business admission remains independent of replenishment request budget")
}
