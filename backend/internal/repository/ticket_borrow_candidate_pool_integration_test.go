//go:build integration

package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type candidatePoolFixture struct {
	r       *ticketBorrowRepository
	targets []service.BorrowAcquisition
	sources []service.BorrowSource
	slots   int
}

func newCandidatePoolFixture(t *testing.T, targets, slots int) *candidatePoolFixture {
	t.Helper()
	r, original := candidateFixture(t)
	settings, err := r.Settings(t.Context())
	require.NoError(t, err)
	window, cont := 180, 7
	settings.AdmissionWindowSeconds, settings.ContinueTimeoutSeconds, settings.ProbeConcurrency = &window, &cont, &slots
	settings.TTLSeconds, settings.MinRouteRemainingSeconds = 0, nil
	settings.Recovery = &service.BorrowRecoverySettings{SourceRequestsPerMinute: 120, ProbeRequestsPerMinute: 120}
	require.NoError(t, r.SaveSettings(t.Context(), settings))
	selection := borrowSelectionFixture(t, r)
	for range 4 {
		selection.AccountIDs = append(selection.AccountIDs, borrowNormalAccountFixture(t).ID)
	}
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	sources, err := r.Sources(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 4)
	f := &candidatePoolFixture{r: r, sources: sources, slots: slots}
	ids := []int64{original.TargetID}
	for len(ids) < targets {
		_, id := borrowIntegrationFixture(t)
		ids = append(ids, id)
	}
	for _, id := range ids {
		target, e := r.Target(t.Context(), id)
		require.NoError(t, e)
		require.NoError(t, r.SetTargetIdentity(t.Context(), id, true, target.Revision, target.AcquisitionRevision, service.BorrowAcquisitionSource))
		a := service.BorrowAcquisition{TargetID: id, Revision: target.Revision, Identity: uuid.NewString(), Lease: uuid.NewString()}
		ok, e := r.AcquireJob(service.WithBorrowAcquisition(t.Context(), a), "target", id, a.Lease)
		require.NoError(t, e)
		require.True(t, ok)
		f.targets = append(f.targets, a)
	}
	f.heartbeat(t)
	t.Cleanup(func() {
		// Isolated Testcontainers database only. Source fixtures must be deleted
		// after their neutral tasks and audit foreign keys have been removed.
		for _, source := range sources {
			_, e := integrationDB.Exec(`DELETE FROM ticket_borrow_candidate_environments WHERE pool_candidate_id IN (SELECT id FROM ticket_borrow_candidate_pool WHERE source_id=$1)`, source.ID)
			require.NoError(t, e)
			_, e = integrationDB.Exec(`DELETE FROM ticket_borrow_source_intents WHERE source_id=$1`, source.ID)
			require.NoError(t, e)
			_, e = integrationDB.Exec(`DELETE FROM ticket_borrow_source_attempts WHERE source_id=$1`, source.ID)
			require.NoError(t, e)
			_, e = integrationDB.Exec(`DELETE FROM ticket_borrow_candidate_pool WHERE source_id=$1`, source.ID)
			require.NoError(t, e)
			_, e = integrationDB.Exec(`DELETE FROM ticket_borrow_events WHERE source_id=$1`, source.ID)
			require.NoError(t, e)
		}
	})
	return f
}

func (f *candidatePoolFixture) heartbeat(t *testing.T) {
	t.Helper()
	for _, a := range f.targets {
		require.NoError(t, f.r.RegisterCandidatePoolDemand(t.Context(), a, f.slots))
	}
}

func (f *candidatePoolFixture) reserve(t *testing.T) service.BorrowPoolAcquisition {
	t.Helper()
	f.heartbeat(t)
	a, err := f.r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, a)
	return *a
}

func (f *candidatePoolFixture) enqueue(t *testing.T, a service.BorrowPoolAcquisition) *service.BorrowPoolCandidate {
	t.Helper()
	ctx := service.WithBorrowPoolAcquisition(t.Context(), a)
	env, err := f.r.AllocateCandidateEnvironment(ctx, a.ID, "shared-candidate-pool")
	require.NoError(t, err)
	env.StartedAt, env.AdmissionUntil, err = f.r.StartCandidateEnvironment(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, f.r.MarkCandidateEnvironmentSent(ctx, a.ID))
	source, err := f.r.Source(ctx, a.SourceID)
	require.NoError(t, err)
	value := uuid.NewString()
	h := sha256.Sum256([]byte(value))
	c := &service.BorrowPoolCandidate{ID: a.ID, SourceID: a.SourceID,
		Cookie: http.Cookie{Name: "__oailb", Value: value}, Expires: env.AdmissionUntil, Environment: env,
		Details: &service.BorrowCandidateDetails{CandidateID: a.ID, BatchID: a.Lease, AcquisitionIdentity: service.BorrowAcquisitionSource,
			SourceAccountID: source.AccountID, SourceRevision: source.Revision, SourceModel: source.Model,
			AcquisitionStartedAt: &env.StartedAt, AdmissionUntil: &env.AdmissionUntil, CookieHash: hex.EncodeToString(h[:])}}
	require.NoError(t, integrationDB.QueryRow(`SELECT encode(sha256(convert_to(credentials->>'access_token','UTF8')),'hex'),encode(sha256(convert_to(COALESCE(credentials->>'chatgpt_account_id','')||':'||COALESCE(credentials->>'chatgpt_user_id',''),'UTF8')),'hex') FROM accounts WHERE id=$1`, source.AccountID).Scan(&c.SourceCredentialHash, &c.SourceSubjectHash))
	require.NoError(t, f.r.EnqueueCandidatePool(ctx, a, c))
	return c
}

func poolCandidateRoute(a service.BorrowAcquisition, c *service.BorrowPoolCandidate) *service.BorrowRoute {
	return &service.BorrowRoute{ID: uuid.NewString(), CandidateID: c.ID, TargetID: a.TargetID, TargetRevision: a.Revision, IdentityHash: a.Identity,
		SourceID: c.SourceID, SourceRevision: c.Details.SourceRevision, AcquisitionIdentity: service.BorrowAcquisitionSource,
		AcquisitionAccountID: c.Details.SourceAccountID, SourceCredentialHash: c.SourceCredentialHash, SourceSubjectHash: c.SourceSubjectHash,
		CookieHash: c.Details.CookieHash, AcquisitionStartedAt: c.Details.AcquisitionStartedAt, AdmissionUntil: c.Expires, ExpiresAt: c.Expires}
}

func TestBorrowSharedPoolNeutralSourceSurvivesTargetFailure(t *testing.T) {
	f := newCandidatePoolFixture(t, 2, 2)
	p := f.reserve(t)
	ctx := service.WithBorrowPoolAcquisition(t.Context(), p)
	permit := uuid.NewString()
	require.NoError(t, f.r.QueueSourceAttempt(ctx, p.SourceID, permit))
	ok, err := f.r.GrantSourceAttempt(ctx, p.SourceID, permit)
	require.NoError(t, err)
	require.True(t, ok)
	var target *int64
	require.NoError(t, integrationDB.QueryRow(`SELECT target_id FROM ticket_borrow_source_attempts WHERE intent_id=$1`, permit).Scan(&target))
	require.Nil(t, target, "source ownership cannot depend on a target lease")
	_, err = integrationDB.Exec(`UPDATE accounts SET status='error' WHERE id=$1`, f.targets[0].TargetID)
	require.NoError(t, err)
	require.NoError(t, f.r.CheckCandidatePoolAcquisition(ctx, p))
	c := f.enqueue(t, p)
	require.NoError(t, f.r.SourceAttemptSent(ctx, permit))
	require.NoError(t, f.r.SourceAttemptCompleted(ctx, permit))
	require.NoError(t, f.r.FinishJob(ctx, "source", p.SourceID, permit, "source_cookie_acquired", 0))
	_, err = f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	got, err := f.r.ClaimCandidatePool(t.Context(), f.targets[1])
	require.NoError(t, err)
	require.Equal(t, c.ID, got.ID)
	require.Equal(t, c.Environment, got.Environment, "source and target use the same frozen environment")
	require.Equal(t, c.Cookie, got.Cookie)
	require.Equal(t, c.Expires.UnixMicro(), got.Expires.UnixMicro(), "claim never renews the original cutoff")
}

func TestBorrowSharedPoolUniqueClaimPublicationAndOwnerFence(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	p := f.reserve(t)
	original := f.enqueue(t, p)
	a := f.targets[0]
	restarted := NewTicketBorrowRepository(integrationDB, f.r.encryptor).(*ticketBorrowRepository)
	var wg sync.WaitGroup
	claimed := make(chan *service.BorrowPoolCandidate, 8)
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.r
			if i%2 == 1 {
				r = restarted
			}
			c, e := r.ClaimCandidatePool(t.Context(), a)
			errs <- e
			if c != nil {
				claimed <- c
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	require.Len(t, claimed, 1)
	c := <-claimed
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), c.ID, p.Lease, "stale_source_result"))
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	require.NoError(t, f.r.CheckCandidateEnvironment(ctx, c.ID))
	require.Error(t, f.r.CheckCandidateEnvironment(service.WithBorrowPoolAcquisition(t.Context(), p), c.ID))
	route := poolCandidateRoute(a, c)
	require.NoError(t, f.r.Publish(ctx, route, a.Lease))
	require.ErrorIs(t, restarted.Publish(ctx, route, a.Lease), service.ErrBorrowCandidateDuplicate)
	published, err := restarted.PublishedPoolCandidate(ctx, a, c.ID)
	require.NoError(t, err)
	require.Equal(t, route.ID, published, "lost acknowledgement is resolved from the committed candidate key")
	require.NoError(t, f.r.FinishCandidatePool(ctx, c.ID, a.Lease, "target_probe_passed"))
	require.NoError(t, f.r.FinishCandidatePool(ctx, c.ID, a.Lease, "late_failure"))
	var state, secret, reason string
	require.NoError(t, integrationDB.QueryRow(`SELECT state,secret,reason FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID).Scan(&state, &secret, &reason))
	require.Equal(t, "finished", state)
	require.Empty(t, secret)
	require.Equal(t, "target_probe_passed", reason)
	require.Equal(t, original.Details.CookieHash, c.Details.CookieHash)
}

func TestBorrowSharedPoolFreshestAndExpiredQueueRecovery(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 3)
	old := f.enqueue(t, f.reserve(t))
	_, err := integrationDB.Exec(`UPDATE ticket_borrow_candidate_pool SET admission_until=clock_timestamp()+INTERVAL '65 seconds' WHERE id=$1`, old.ID)
	require.NoError(t, err)
	fresh := f.enqueue(t, f.reserve(t))
	got, err := f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.NoError(t, err)
	require.Equal(t, fresh.ID, got.ID)
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), got.ID, f.targets[0].Lease, "test_complete"))
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_candidate_pool SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, old.ID)
	require.NoError(t, err)
	require.NoError(t, f.r.SweepCandidatePool(t.Context()))
	var state string
	require.NoError(t, integrationDB.QueryRow(`SELECT state FROM ticket_borrow_candidate_pool WHERE id=$1`, old.ID).Scan(&state))
	require.Equal(t, "finished", state)
	newest := f.enqueue(t, f.reserve(t))
	got, err = f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.NoError(t, err)
	require.Equal(t, newest.ID, got.ID, "expired queue entries do not block fresh supply")
}

func TestBorrowSharedPoolBackpressureCooldownAndSourceBudget(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 1)
	p := f.reserve(t)
	f.enqueue(t, p)
	reserve := func() *service.BorrowPoolAcquisition {
		p, e := f.r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
		require.NoError(t, e)
		return p
	}
	require.Nil(t, reserve(), "queued work satisfies the only available verification slot")
	c, err := f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.NoError(t, err)
	standby := reserve()
	require.NotNil(t, standby, "busy validation may prefetch one bounded standby")
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), standby.ID, standby.Lease, "test_cleanup"))
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), c.ID, f.targets[0].Lease, "target_ticket_changed"))
	_, err = integrationDB.Exec(`UPDATE accounts SET rate_limit_reset_at=clock_timestamp()+INTERVAL '60 seconds' WHERE id=$1`, f.targets[0].TargetID)
	require.NoError(t, err)
	require.Nil(t, reserve())
	_, err = integrationDB.Exec(`UPDATE accounts SET rate_limit_reset_at=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, f.targets[0].TargetID)
	require.NoError(t, err)
	p = f.reserve(t)
	f.enqueue(t, p)
	_, err = integrationDB.Exec(`INSERT INTO ticket_borrow_source_attempts(source_id,reserved_at) SELECT $1,clock_timestamp() FROM generate_series(1,120)`, p.SourceID)
	require.NoError(t, err)
	require.Nil(t, reserve(), "new source HTTP respects its own exhausted budget")
	require.NoError(t, f.r.FinishJob(t.Context(), "target", f.targets[0].TargetID, f.targets[0].Lease, "test_restart", 0))
	inv, err := f.r.Inventory(t.Context(), f.targets[0].TargetID, f.targets[0].Identity)
	require.NoError(t, err)
	require.NotEqual(t, "source_request_budget", inv.BlockedReason, "an existing queued candidate does not require new source HTTP")
	a := f.targets[0]
	a.Lease = uuid.NewString()
	require.NoError(t, f.r.RefreshTarget(t.Context(), a.TargetID))
	ok, err := f.r.AcquireJob(service.WithBorrowAcquisition(t.Context(), a), "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.r.RegisterCandidatePoolDemand(t.Context(), a, 1))
	c, err = f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestBorrowSharedPoolAbandonedSendNeverRequeuesOrPublishes(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	f.enqueue(t, f.reserve(t))
	a := f.targets[0]
	c, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	q := probeRequest(a)
	q.CandidateID = c.ID
	require.NoError(t, f.r.BeginTargetRequest(ctx, q))
	require.NoError(t, f.r.MarkTargetRequestSent(ctx, q))
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_candidate_pool SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, c.ID)
	require.NoError(t, err)
	require.NoError(t, f.r.SweepCandidatePool(ctx))
	var state string
	require.NoError(t, integrationDB.QueryRow(`SELECT state FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID).Scan(&state))
	require.Equal(t, "claimed", state, "physical HTTP ownership survives an expired task lease")
	require.NoError(t, f.r.EndTargetRequest(ctx, q, 200, "eof"))
	require.NoError(t, f.r.SweepCandidatePool(ctx))
	require.NoError(t, integrationDB.QueryRow(`SELECT state FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID).Scan(&state))
	require.Equal(t, "finished", state)
	require.ErrorIs(t, f.r.Publish(ctx, poolCandidateRoute(a, c), a.Lease), service.ErrBorrowConflict)
	got, err := f.r.ClaimCandidatePool(ctx, a)
	require.NoError(t, err)
	require.Nil(t, got, "an unknown send result must never become a fresh shared candidate")
}

func TestBorrowSharedPoolGlobalCapacityAndSourceReservationsAcrossInstances(t *testing.T) {
	f := newCandidatePoolFixture(t, 3, 32)
	restarted := NewTicketBorrowRepository(integrationDB, f.r.encryptor).(*ticketBorrowRepository)
	for wave := 0; wave < service.DefaultBorrowCandidatePoolCapacity/4; wave++ {
		f.heartbeat(t)
		var wg sync.WaitGroup
		reservations := make(chan *service.BorrowPoolAcquisition, 12)
		errs := make(chan error, 12)
		for i := range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := f.r
				if i%2 == 1 {
					r = restarted
				}
				p, err := r.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
				errs <- err
				if p != nil {
					reservations <- p
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.Len(t, reservations, 4, "source tasks are globally bounded, not four per instance")
		for range 4 {
			f.enqueue(t, *<-reservations)
		}
	}
	f.heartbeat(t)
	p, err := restarted.ReserveCandidatePoolFetch(t.Context(), uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, p, "96 consumer slots still cannot expand the shared pool beyond 64")
	var total int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_candidate_pool WHERE state<>'finished'`).Scan(&total))
	require.Equal(t, service.DefaultBorrowCandidatePoolCapacity, total)
}

func TestBorrowSharedPoolRenewalGetsBoundedFairTurn(t *testing.T) {
	f := newCandidatePoolFixture(t, 2, 1)
	_, err := integrationDB.Exec(`UPDATE ticket_borrow_candidate_pool_control SET consumer_cursor=0 WHERE id=1`)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET last_inventory_state='renew' WHERE account_id=$1`, f.targets[1].TargetID)
	require.NoError(t, err)
	served := map[int64]int{}
	for range 3 {
		f.enqueue(t, f.reserve(t))
		var selected *service.BorrowPoolCandidate
		for _, a := range f.targets {
			c, e := f.r.ClaimCandidatePool(t.Context(), a)
			require.NoError(t, e)
			if c != nil {
				selected = c
				served[a.TargetID]++
				require.NoError(t, f.r.FinishCandidatePool(t.Context(), c.ID, a.Lease, "target_ticket_changed"))
				break
			}
		}
		require.NotNil(t, selected)
	}
	require.Equal(t, 2, served[f.targets[0].TargetID])
	require.Equal(t, 1, served[f.targets[1].TargetID], "an endless search cannot starve an eligible renewal")
}

func TestBorrowSharedPoolCompletedResultsPublishAfterInventoryAndBudgetFill(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	f.enqueue(t, f.reserve(t))
	f.enqueue(t, f.reserve(t))
	a := f.targets[0]
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	first, err := f.r.ClaimCandidatePool(ctx, a)
	require.NoError(t, err)
	second, err := f.r.ClaimCandidatePool(ctx, a)
	require.NoError(t, err)
	old := poolCandidateRoute(a, first)
	// An upstream response may tighten the first route's cutoff. The second
	// completed candidate then replaces it under the existing inventory policy.
	old.AdmissionUntil = old.AdmissionUntil.Add(-20 * time.Second)
	require.NoError(t, f.r.Publish(ctx, old, a.Lease))
	businessLease := uuid.NewString()
	_, err = f.r.ReserveRoute(ctx, a.TargetID, a.Identity, "", "", businessLease)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET budget_wait_until=clock_timestamp()+INTERVAL '1 minute' WHERE account_id=$1`, a.TargetID)
	require.NoError(t, err)
	require.NoError(t, f.r.CheckCandidateEnvironment(ctx, second.ID), "publication does not request a new upstream HTTP permit")
	latest := poolCandidateRoute(a, second)
	require.NoError(t, f.r.Publish(ctx, latest, a.Lease), "another success filling inventory cannot reject this completed result")
	got, err := f.r.Route(ctx, a.TargetID, a.Identity, "")
	require.NoError(t, err)
	require.Equal(t, latest.ID, got.ID)
	require.NoError(t, f.r.RenewRoute(ctx, businessLease), "inventory replacement does not interrupt an admitted business request")
	require.NoError(t, f.r.ReleaseRoute(ctx, businessLease))
	require.NoError(t, f.r.FinishCandidatePool(ctx, first.ID, a.Lease, "target_probe_passed"))
	require.NoError(t, f.r.FinishCandidatePool(ctx, second.ID, a.Lease, "target_probe_passed"))
	stats, err := f.r.BorrowStatistics(ctx, a.TargetID, 15)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Passed, "claim and completion audit events do not duplicate candidate results")
	require.Equal(t, 2, stats.Published)
}

func TestBorrowSharedPoolPauseConfigurationFenceAndWindowFloor(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	p := f.reserve(t)
	queued := f.enqueue(t, p)
	settings, err := f.r.Settings(t.Context())
	require.NoError(t, err)
	settings.Running = false
	require.NoError(t, f.r.SaveSettings(t.Context(), settings))
	require.NoError(t, f.r.SweepCandidatePool(t.Context()))
	var state, secret string
	require.NoError(t, integrationDB.QueryRow(`SELECT state,secret FROM ticket_borrow_candidate_pool WHERE id=$1`, queued.ID).Scan(&state, &secret))
	require.Equal(t, "finished", state)
	require.Empty(t, secret)
	require.Error(t, f.r.CheckCandidatePoolAcquisition(t.Context(), p))
	settings, err = f.r.Settings(t.Context())
	require.NoError(t, err)
	settings.Running = true
	short := int(service.BorrowPoolVerificationBudget(7, "mint") / time.Second)
	settings.AdmissionWindowSeconds, settings.TTLSeconds, settings.MinRouteRemainingSeconds = &short, 0, nil
	require.NoError(t, f.r.SaveSettings(t.Context(), settings))
	a := inventoryRestart(t, f.r, f.targets[0])
	require.ErrorIs(t, f.r.RegisterCandidatePoolDemand(t.Context(), a, 2), service.ErrBorrowCandidatePoolWindow)
	_, err = f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.Error(t, err, "the prior target lease cannot claim after configuration change")
}

func TestBorrowSharedPoolLastConsumerGonePreventsUnsentSource(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	p := f.reserve(t)
	ctx := service.WithBorrowPoolAcquisition(t.Context(), p)
	env, err := f.r.AllocateCandidateEnvironment(ctx, p.ID, "shared-candidate-pool")
	require.NoError(t, err)
	_, _, err = f.r.StartCandidateEnvironment(ctx, env.ID)
	require.NoError(t, err)
	require.NoError(t, f.r.RemoveCandidatePoolDemand(t.Context(), f.targets[0]))
	require.ErrorIs(t, f.r.CheckCandidateEnvironment(ctx, env.ID), service.ErrBorrowUnavailable,
		"a source waiting for a connection cannot send after the final consumer leaves")
	require.NoError(t, f.r.FinishCandidatePool(ctx, p.ID, p.Lease, "candidate_pool_no_consumers"))
}
