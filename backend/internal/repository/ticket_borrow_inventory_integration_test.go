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

func inventoryRestart(t *testing.T, r *ticketBorrowRepository, a service.BorrowAcquisition) service.BorrowAcquisition {
	t.Helper()
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "test", 0))
	require.NoError(t, r.RefreshTarget(ctx, a.TargetID))
	a.Lease = uuid.NewString()
	ok, err := r.AcquireJob(service.WithBorrowAcquisition(ctx, a), "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	return a
}

func TestTicketBorrowInventoryWakeupFencingAndPublicationBudget(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 235*time.Second), a.Lease))
	inv, err := r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Equal(t, 2, inv.Count)
	require.Equal(t, "healthy", inv.State)
	require.NoError(t, r.RefreshTarget(ctx, a.TargetID))
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "old-finish", time.Hour))
	inv, err = r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.False(t, inv.NextAt.After(inv.ServerNow), "old finish cannot overwrite new generation")
	a.Lease = uuid.NewString()
	ctx = service.WithBorrowAcquisition(ctx, a)
	ok, err := r.AcquireJob(ctx, "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	rotationExec(t, `UPDATE ticket_borrow_targets SET budget_wait_until=clock_timestamp()+INTERVAL '1 minute' WHERE account_id=$1`, a.TargetID)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 240*time.Second), a.Lease), "publication is not a new request")
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	require.NoError(t, r.SaveSettings(ctx, cfg))
	require.ErrorIs(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 240*time.Second), a.Lease), service.ErrBorrowConflict)
	require.Error(t, r.CheckAcquisition(ctx, a), "old settings permit is fenced")
}

func TestTicketBorrowInventoryAutomaticCapacityAndCandidateGates(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	for range 4 {
		require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	}
	need, err := r.NeedsCandidate(ctx, a, nil)
	require.NoError(t, err)
	require.False(t, need, "a full inventory stops speculative acquisition")
	short := time.Now().Add(10 * time.Second)
	need, err = r.NeedsCandidate(ctx, a, &short)
	require.NoError(t, err)
	require.False(t, need)
	longer := time.Now().Add(90 * time.Second)
	need, err = r.NeedsCandidate(ctx, a, &longer)
	require.NoError(t, err)
	require.True(t, need)
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	cfg.Replenishment.MaintenanceIntervalSeconds = new(45)
	cfg.Replenishment.Inventory = 4
	require.NoError(t, r.SaveSettings(ctx, cfg))
	cfg, err = r.Settings(ctx)
	require.NoError(t, err)
	require.Zero(t, cfg.Replenishment.MaintenanceSeconds(), "legacy periodic maintenance is retired")
	inv, err := r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Equal(t, 2, inv.Count)
	require.Equal(t, 2, inv.Desired, "rolling capacity is an internal policy")
	cfg.Replenishment.Enabled = false
	require.NoError(t, r.SaveSettings(ctx, cfg))
	inv, err = r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Equal(t, 1, inv.Count)
	require.Equal(t, 1, inv.Desired)
	cfg, err = r.Settings(ctx)
	require.NoError(t, err)
	cfg.Replenishment.Enabled = true
	require.NoError(t, r.SaveSettings(ctx, cfg))
	inv, err = r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Equal(t, 1, inv.Count, "enabling rolling cannot revive retired inventory")
	require.Equal(t, 2, inv.Desired)
	var revoked int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_routes WHERE target_id=$1 AND revoked_at IS NOT NULL`, a.TargetID).Scan(&revoked))
	require.Zero(t, revoked)
	a = inventoryRestart(t, r, a)
	ctx = service.WithBorrowAcquisition(ctx, a)
	candidate, cookie := uuid.NewString(), uuid.NewString()
	require.NoError(t, r.RecordCandidate(ctx, a, candidate, cookie, "healthy", "configuration_changed", time.Now().Add(time.Minute)))
	known, err := r.CandidateKnown(ctx, a.TargetID, a.Identity, cookie)
	require.NoError(t, err)
	require.True(t, known, "unpublished verdicts remain deduplicated")
}

func TestTicketBorrowInventoryRevocationAndCookieDeadlineWakeup(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	route := replenishmentRoute(t, a, sources[0], 230*time.Second)
	require.NoError(t, r.Publish(ctx, route, a.Lease))
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 235*time.Second), a.Lease))
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "passed", 0))
	inv, err := r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.True(t, inv.NextAt.After(inv.ServerNow))
	_, err = r.ObserveCookie(ctx, route.CookieHash, -time.Second)
	require.NoError(t, err)
	inv, err = r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Equal(t, 1, inv.Count)
	require.False(t, inv.NextAt.After(inv.ServerNow))
	a = inventoryRestart(t, r, a)
	route = replenishmentRoute(t, a, sources[0], 240*time.Second)
	require.NoError(t, r.Publish(ctx, route, a.Lease))
	require.NoError(t, r.Revoke(ctx, a.TargetID, route.ID, "business_invalid"))
	require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "old", time.Hour))
	inv, err = r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.False(t, inv.NextAt.After(inv.ServerNow))
}

func TestTicketBorrowSourcePermitGlobalBudgetAndNoQueuedLease(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := t.Context()
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	copy := *cfg.Recovery
	cfg.Recovery = &copy
	cfg.Recovery.SourceRequestsPerMinute = 1
	require.NoError(t, r.SaveSettings(ctx, cfg))
	a = inventoryRestart(t, r, a)
	ctx = service.WithBorrowAcquisition(ctx, a)
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_source_attempts WHERE target_id=$1`, a.TargetID)
	})
	// Queueing doesn't claim a source or consume budget.
	first, second := uuid.NewString(), uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, sources[0].ID, first))
	require.NoError(t, r.QueueSourceAttempt(ctx, sources[1].ID, second))
	var leases int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_sources WHERE id IN($1,$2) AND lease_until>clock_timestamp()`, sources[0].ID, sources[1].ID).Scan(&leases))
	require.Zero(t, leases)
	other := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	type result struct {
		granted bool
		err     error
		id      string
		source  int64
	}
	out := make(chan result, 2)
	var wg sync.WaitGroup
	for i, id := range []string{first, second} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			ok, e := other.GrantSourceAttempt(ctx, sources[i].ID, id)
			out <- result{ok, e, id, sources[i].ID}
		}(i, id)
	}
	wg.Wait()
	close(out)
	grants := 0
	var winner result
	for result := range out {
		if result.granted {
			winner = result
			grants++
		}
		if result.err != nil {
			require.ErrorIs(t, result.err, service.ErrBorrowRequestBudget)
		}
	}
	require.Equal(t, 1, grants)
	// Unsent reservations are refunded; a real send remains charged after completion.
	require.NoError(t, r.FinishJob(ctx, "source", winner.source, winner.id, "cancelled", 0))
	require.NoError(t, r.SourceAttemptCompleted(ctx, winner.id))
	loser, source := first, sources[0].ID
	if winner.id == first {
		loser, source = second, sources[1].ID
	}
	ok, err := other.GrantSourceAttempt(ctx, source, loser)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, r.SourceAttemptSent(ctx, loser))
	require.NoError(t, r.SourceAttemptCompleted(ctx, loser))
	require.NoError(t, r.FinishJob(ctx, "source", source, loser, "failed_after_send", 0))
	next := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, source, next))
	_, err = other.GrantSourceAttempt(ctx, source, next)
	require.ErrorIs(t, err, service.ErrBorrowRequestBudget)
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_source_attempts WHERE target_id=$1`, a.TargetID).Scan(&count))
	require.Equal(t, 1, count)

}

func TestTicketBorrowSourcePermitWeightedFairnessAndStaleHead(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := t.Context()
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	copy := *cfg.Recovery
	cfg.Recovery = &copy
	cfg.Recovery.SourceRequestsPerMinute = 120
	require.NoError(t, r.SaveSettings(ctx, cfg))
	a = inventoryRestart(t, r, a)
	targets := []service.BorrowAcquisition{a}
	for range 2 {
		_, id := borrowIntegrationFixture(t)
		target, e := r.Target(ctx, id)
		require.NoError(t, e)
		require.NoError(t, r.SetTargetIdentity(ctx, id, true, target.Revision, target.AcquisitionRevision, service.BorrowAcquisitionSource))
		b := service.BorrowAcquisition{TargetID: id, Revision: target.Revision, Identity: "identity", Lease: uuid.NewString()}
		ok, e := r.AcquireJob(service.WithBorrowAcquisition(ctx, b), "target", id, b.Lease)
		require.NoError(t, e)
		require.True(t, ok)
		targets = append(targets, b)
	}
	categories := []string{"search", "refill", "healthy"}
	for i, b := range targets {
		rotationExec(t, `UPDATE ticket_borrow_targets SET last_inventory_state=$2 WHERE account_id=$1`, b.TargetID, categories[i])
	}
	rotationExec(t, `UPDATE ticket_borrow_source_arbiter SET cursor=0 WHERE id=1`)
	ids := make([]string, 3)
	counts := make([]int, 3)
	queue := func(i int) {
		ids[i] = uuid.NewString()
		require.NoError(t, r.QueueSourceAttempt(service.WithBorrowAcquisition(ctx, targets[i]), sources[0].ID, ids[i]))
	}
	for i := range targets {
		queue(i)
	}
	// An expired row cannot be chosen or block an otherwise eligible target.
	rotationExec(t, `INSERT INTO ticket_borrow_source_intents(id,target_id,batch_id,source_id,category,expires_at) VALUES('stale',$1,'stale',$2,'search',clock_timestamp()-INTERVAL '1 second')`, a.TargetID, sources[1].ID)
	for range 14 {
		granted := -1
		for i, b := range targets {
			ok, e := r.GrantSourceAttempt(service.WithBorrowAcquisition(ctx, b), sources[0].ID, ids[i])
			require.NoError(t, e)
			if ok {
				granted = i
				break
			}
		}
		require.NotEqual(t, -1, granted)
		counts[granted]++
		require.NoError(t, r.SourceAttemptSent(ctx, ids[granted]))
		require.NoError(t, r.SourceAttemptCompleted(ctx, ids[granted]))
		require.NoError(t, r.FinishJob(ctx, "source", sources[0].ID, ids[granted], "done", 0))
		queue(granted)
	}
	require.Equal(t, []int{8, 4, 2}, counts)
	t.Cleanup(func() {
		for _, b := range targets {
			_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_source_attempts WHERE target_id=$1`, b.TargetID)
		}
	})
}

func TestTicketBorrowInventoryConcurrentSettingsPublicationAllocationAndPermits(t *testing.T) {
	r, pool, sources, a := replenishmentFixture(t, false)
	rotationAppend(t, pool, "inventory-spare.example:84")
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	ctx = service.WithBorrowAcquisition(ctx, a)
	cfg, err := r.Settings(ctx)
	require.NoError(t, err)
	route := replenishmentRoute(t, a, sources[0], 230*time.Second)
	attempt := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, sources[1].ID, attempt))
	out := make(chan error, 4)
	go func() { out <- r.SaveSettings(ctx, cfg) }()
	go func() { out <- r.Publish(ctx, route, a.Lease) }()
	go func() { _, e := r.GrantSourceAttempt(ctx, sources[1].ID, attempt); out <- e }()
	go func() {
		_, e := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "concurrent-identity")
		out <- e
	}()
	for range 4 {
		e := <-out
		if e != nil {
			require.True(t, errors.Is(e, service.ErrBorrowConflict) || errors.Is(e, service.ErrBorrowUnavailable), "unexpected concurrent failure: %v", e)
		}
	}
	require.NoError(t, ctx.Err(), "lock order must complete without deadlock/timeouts")
	require.NoError(t, r.FinishJob(ctx, "source", sources[1].ID, attempt, "cleanup", 0))
	require.NoError(t, r.CancelSourceAttempt(ctx, attempt))
	require.NoError(t, r.ProcessRotations(ctx))
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM ticket_borrow_source_attempts WHERE target_id=$1`, a.TargetID)
	})
}
