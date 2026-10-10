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

func rotationExec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, e := integrationDB.ExecContext(context.Background(), query, args...)
	require.NoError(t, e)
}
func rotationInput(role string, ids ...int64) service.BorrowRotationInput {
	return service.BorrowRotationInput{ID: uuid.NewString(), Role: role, AccountIDs: ids}
}
func rotationPhase(t *testing.T, id string) string {
	t.Helper()
	var phase string
	require.NoError(t, integrationDB.QueryRow(`SELECT phase FROM ticket_borrow_rotation_jobs WHERE id=$1`, id).Scan(&phase))
	return phase
}
func rotationSourceFixture(t *testing.T) (*ticketBorrowRepository, *service.ProxyPoolService, []service.BorrowSource) {
	t.Helper()
	_, pool, _ := poolFixture(t)
	repo, _ := borrowIntegrationFixture(t)
	r := repo.(*ticketBorrowRepository)
	a, b := borrowNormalAccountFixture(t), borrowNormalAccountFixture(t)
	selection := borrowSelectionFixture(t, r)
	selection.AccountIDs = []int64{a.ID, b.ID}
	selection.UseProxyPool = true
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	_, e := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "a.example:80:user:secret-password\nb.example:81"})
	require.NoError(t, e)
	sources, e := r.Sources(t.Context())
	require.NoError(t, e)
	require.Len(t, sources, 2)
	return r, pool, sources
}
func TestTicketBorrowRotationFullPoolAtomicPermutationDrainsAndRestarts(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	ctx := t.Context()
	ids := []int64{sources[0].AccountID, sources[1].AccountID}
	_, e := r.QueueRotation(ctx, rotationInput("source", ids[0]), "manual")
	require.ErrorIs(t, e, service.ErrBorrowRotationNoProxy)
	ok, e := r.AcquireJob(ctx, "source", sources[0].ID, "source-worker")
	require.NoError(t, e)
	require.True(t, ok)
	in := rotationInput("source", ids...)
	job, e := r.QueueRotation(ctx, in, "manual")
	require.NoError(t, e)
	again, e := r.QueueRotation(ctx, in, "manual")
	require.NoError(t, e)
	require.Equal(t, job, again)
	pending, e := r.QueueRotation(ctx, rotationInput("source", ids...), "manual")
	require.NoError(t, e)
	require.Empty(t, pending.AccountIDs)
	require.Len(t, pending.Skipped, 2)
	require.Equal(t, service.ErrBorrowRotationPending.Error(), pending.Skipped[0].Reason)
	_, e = r.Source(ctx, sources[0].ID)
	require.ErrorIs(t, e, service.ErrBorrowUnavailable)
	ok, e = r.AcquireJob(ctx, "source", sources[1].ID, "new-worker")
	require.NoError(t, e)
	require.False(t, ok)
	require.NoError(t, r.ProcessRotations(ctx))
	require.Equal(t, "draining", rotationPhase(t, in.ID))
	var entry int64
	require.NoError(t, integrationDB.QueryRow(`SELECT entry_id FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, ids[0]).Scan(&entry))
	require.Equal(t, sources[0].PoolEntryID, entry)
	require.NoError(t, r.FinishJob(ctx, "source", sources[0].ID, "source-worker", "done", 0))
	restarted := NewTicketBorrowRepository(integrationDB, r.encryptor).(service.BorrowRotationRepository)
	require.NoError(t, restarted.ProcessRotations(ctx))
	require.Equal(t, "completed", rotationPhase(t, in.ID))
	for i, old := range sources {
		changed, e := r.Source(ctx, old.ID)
		require.NoError(t, e)
		require.Equal(t, sources[1-i].PoolEntryID, changed.PoolEntryID)
		require.NotEqual(t, old.FingerprintID, changed.FingerprintID)
	}
	states, e := r.RotationStates(ctx)
	require.NoError(t, e)
	require.Len(t, states, 2)
	raw, e := json.Marshal(states)
	require.NoError(t, e)
	require.NotContains(t, string(raw), "secret-password")
	require.NotContains(t, string(raw), "user:")
	var unchanged bool
	require.NoError(t, integrationDB.QueryRow(`SELECT bool_and(proxy_id IS NULL) FROM accounts WHERE id IN ($1,$2)`, ids[0], ids[1]).Scan(&unchanged))
	require.True(t, unchanged)
}
func TestTicketBorrowRotationReservationInvalidationAndDrainTimeout(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "disabled"}[disable], func(t *testing.T) {
			r, pool, sources := rotationSourceFixture(t)
			ctx := t.Context()
			in := rotationInput("source", sources[0].AccountID, sources[1].AccountID)
			_, e := r.QueueRotation(ctx, in, "manual")
			require.NoError(t, e)
			if disable {
				state, e := pool.State(ctx)
				require.NoError(t, e)
				_, e = pool.SetEnabled(ctx, state.Revision, sources[1].PoolEntryID, false)
				require.NoError(t, e)
			} else {
				rotationExec(t, `UPDATE ticket_borrow_rotation_jobs SET deadline=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, in.ID)
			}
			require.NoError(t, r.ProcessRotations(ctx))
			require.Equal(t, "failed", rotationPhase(t, in.ID))
			var entry int64
			require.NoError(t, integrationDB.QueryRow(`SELECT entry_id FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, sources[0].AccountID).Scan(&entry))
			require.Equal(t, sources[0].PoolEntryID, entry)
			var count int
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_assignments`).Scan(&count))
			require.Zero(t, count)
		})
	}
}
func rotationPublish(t *testing.T, r *ticketBorrowRepository, id, rev int64, lease string) *service.BorrowRoute {
	t.Helper()
	now, e := r.Now(t.Context())
	require.NoError(t, e)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: rev, SourceID: 1, IdentityHash: "identity", CookieHash: uuid.NewString(), ExpiresAt: now.Add(3 * time.Minute)}
	require.NoError(t, r.Publish(t.Context(), route, lease))
	return route
}
func TestTicketBorrowRotationSourceKeepsPublishedTargetRoute(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	ctx := t.Context()
	_, id := borrowIntegrationFixture(t)
	target, e := r.Target(ctx, id)
	require.NoError(t, e)
	ok, e := r.AcquireJob(ctx, "target", id, "worker")
	require.NoError(t, e)
	require.True(t, ok)
	route := rotationPublish(t, r, id, target.Revision, "worker")
	rotationExec(t, `UPDATE ticket_borrow_routes SET source_id=$2 WHERE id=$1`, route.ID, sources[0].ID)
	in := rotationInput("source", sources[0].AccountID, sources[1].AccountID)
	_, e = r.QueueRotation(ctx, in, "manual")
	require.NoError(t, e)
	require.NoError(t, r.ProcessRotations(ctx))
	require.Equal(t, "completed", rotationPhase(t, in.ID))
	saved, e := r.Route(ctx, id, "identity", route.ID)
	require.NoError(t, e)
	require.Equal(t, route.ID, saved.ID)
	require.WithinDuration(t, route.ExpiresAt, saved.ExpiresAt, time.Millisecond)
}

func rotationSetRunning(t *testing.T, r *ticketBorrowRepository, running bool) {
	t.Helper()
	old, e := r.Settings(t.Context())
	require.NoError(t, e)
	t.Cleanup(func() {
		cur, e := r.Settings(context.Background())
		if e == nil {
			old.Revision = cur.Revision
			_ = r.SaveSettings(context.Background(), old)
		}
	})
	next := old
	next.Running = running
	require.NoError(t, r.SaveSettings(t.Context(), next))
}
func rotationAppend(t *testing.T, pool *service.ProxyPoolService, text string) map[string]int64 {
	t.Helper()
	state, e := pool.State(t.Context())
	require.NoError(t, e)
	state, e = pool.Import(t.Context(), service.ProxyPoolImport{Revision: state.Revision, Mode: "append", Text: text})
	require.NoError(t, e)
	result := map[string]int64{}
	for _, entry := range state.Entries {
		result[entry.Host] = entry.ID
	}
	return result
}
func rotationAssigned(t *testing.T, r *ticketBorrowRepository, source service.BorrowSource) (int64, string) {
	t.Helper()
	saved, e := r.Source(t.Context(), source.ID)
	require.NoError(t, e)
	return saved.PoolEntryID, saved.Secret.DeviceID
}
func rotationApply(t *testing.T, r *ticketBorrowRepository, ids ...int64) string {
	t.Helper()
	in := rotationInput("source", ids...)
	_, e := r.QueueRotation(t.Context(), in, "manual")
	require.NoError(t, e)
	require.NoError(t, r.ProcessRotations(t.Context()))
	require.Equal(t, "completed", rotationPhase(t, in.ID))
	return in.ID
}
func TestTicketBorrowRotationReusesAllHistoryForManualAndAutomatic(t *testing.T) {
	for _, trigger := range []string{"manual", "automatic"} {
		t.Run(trigger, func(t *testing.T) {
			r, _, sources := rotationSourceFixture(t)
			rotationSetRunning(t, r, trigger == "automatic")
			ids := []int64{sources[0].AccountID, sources[1].AccountID}
			for _, source := range sources {
				for _, entry := range sources {
					rotationExec(t, `INSERT INTO ticket_borrow_proxy_usage(account_id,entry_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, source.AccountID, entry.PoolEntryID)
				}
				if trigger == "automatic" {
					require.NoError(t, r.SaveRotationPolicy(t.Context(), source.AccountID, service.BorrowRotationPolicy{Automatic: true, IntervalSeconds: 60}))
				}
			}
			for round := 0; round < 3; round++ {
				if trigger == "manual" {
					rotationApply(t, r, ids...)
				} else {
					rotationExec(t, `UPDATE ticket_borrow_rotation_accounts SET next_at=clock_timestamp()-INTERVAL '1 second'`)
					require.NoError(t, r.ProcessRotations(t.Context()))
					require.NoError(t, r.ProcessRotations(t.Context()))
				}
				for i, old := range sources {
					changed, err := r.Source(t.Context(), old.ID)
					require.NoError(t, err)
					expected := sources[1-i].PoolEntryID
					if round%2 == 1 {
						expected = old.PoolEntryID
					}
					require.Equal(t, expected, changed.PoolEntryID)
					require.NotEqual(t, old.FingerprintID, changed.FingerprintID)
				}
				states, err := r.RotationStates(t.Context())
				require.NoError(t, err)
				for _, state := range states {
					require.Equal(t, "random", state.SelectionKind)
				}
			}
		})
	}
}

func TestTicketBorrowRotationAutomaticSourcesBatchCASPauseAndNoOverlap(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	ctx := t.Context()
	rotationSetRunning(t, r, false)
	for _, source := range sources {
		p := service.BorrowRotationPolicy{Automatic: true, IntervalSeconds: 60}
		require.NoError(t, r.SaveRotationPolicy(ctx, source.AccountID, p))
		require.ErrorIs(t, r.SaveRotationPolicy(ctx, source.AccountID, p), service.ErrBorrowConflict)
	}
	rotationExec(t, `UPDATE ticket_borrow_rotation_accounts SET next_at=clock_timestamp()-INTERVAL '1 hour'`)
	require.NoError(t, r.ProcessRotations(ctx))
	var count int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_jobs WHERE trigger_kind='automatic'`).Scan(&count))
	require.Zero(t, count)
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	cfg.Running = true
	require.NoError(t, r.SaveSettings(ctx, cfg))
	// Keep one source in flight so concurrent workers all see the same pending batch.
	ok, e := r.AcquireJob(ctx, "source", sources[0].ID, "lease")
	require.NoError(t, e)
	require.True(t, ok)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- r.ProcessRotations(ctx) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_jobs WHERE trigger_kind='automatic'`).Scan(&count))
	require.Equal(t, 1, count)
	states, e := r.RotationStates(ctx)
	require.NoError(t, e)
	require.Equal(t, states[0].ActiveJob, states[1].ActiveJob)
	job := states[0].ActiveJob
	require.NotEmpty(t, job)
	pending, e := r.QueueRotation(ctx, rotationInput("source", sources[0].AccountID), "manual")
	require.NoError(t, e)
	require.Empty(t, pending.AccountIDs)
	require.Equal(t, []service.BorrowRotationSkipped{{AccountID: sources[0].AccountID, Reason: service.ErrBorrowRotationPending.Error()}}, pending.Skipped)
	require.NoError(t, r.FinishJob(ctx, "source", sources[0].ID, "lease", "done", 0))
	require.NoError(t, r.ProcessRotations(ctx))
	require.Equal(t, "completed", rotationPhase(t, job))
	states, e = r.RotationStates(ctx)
	require.NoError(t, e)
	for _, state := range states {
		require.True(t, state.Automatic, "queueing must not reset the source policy")
		require.WithinDuration(t, time.Now().Add(time.Minute), *state.NextAt, 2*time.Second)
	}
	require.NoError(t, r.ProcessRotations(ctx))
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_jobs WHERE trigger_kind='automatic'`).Scan(&count))
	require.Equal(t, 1, count)
}
func TestTicketBorrowRotationAutoPausedCancelsButManualSourceAllowed(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	ctx := t.Context()
	rotationSetRunning(t, r, true)
	for _, s := range sources {
		require.NoError(t, r.SaveRotationPolicy(ctx, s.AccountID, service.BorrowRotationPolicy{Automatic: true, IntervalSeconds: 60}))
	}
	rotationExec(t, `UPDATE ticket_borrow_rotation_accounts SET next_at=clock_timestamp()-INTERVAL '1 second'`)
	require.NoError(t, r.ProcessRotations(ctx))
	states, e := r.RotationStates(ctx)
	require.NoError(t, e)
	job := states[0].ActiveJob
	cfg, e := r.Settings(ctx)
	require.NoError(t, e)
	cfg.Running = false
	require.NoError(t, r.SaveSettings(ctx, cfg))
	require.NoError(t, r.ProcessRotations(ctx))
	require.Equal(t, "cancelled", rotationPhase(t, job))
	for _, source := range sources {
		entry, _ := rotationAssigned(t, r, source)
		require.Equal(t, source.PoolEntryID, entry)
	}
	rotationApply(t, r, sources[0].AccountID, sources[1].AccountID)
}
func TestTicketBorrowRotationHistoryNotClearedOnFailedPlanOrEventCleanup(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	ctx := t.Context()
	var before int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_usage`).Scan(&before))
	require.Equal(t, 2, before)
	in := rotationInput("source", sources[0].AccountID, sources[1].AccountID)
	_, e := r.QueueRotation(ctx, in, "manual")
	require.NoError(t, e)
	rotationExec(t, `UPDATE ticket_borrow_rotation_jobs SET deadline=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, in.ID)
	require.NoError(t, r.ProcessRotations(ctx))
	require.Equal(t, "failed", rotationPhase(t, in.ID))
	require.NoError(t, r.Cleanup(ctx))
	var after int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_usage`).Scan(&after))
	require.Equal(t, before, after)
	rotationApply(t, r, sources[0].AccountID, sources[1].AccountID)
	selection, e := r.Selection(ctx)
	require.NoError(t, e)
	selection.AccountIDs = []int64{}
	selection.GroupIDs = []int64{}
	require.NoError(t, r.SaveSelection(ctx, selection))
	_, e = r.Sources(ctx)
	require.NoError(t, e)
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_usage`).Scan(&after))
	require.Equal(t, 4, after)
	// Reimporting retired proxies keeps identities and their history.
	pool := service.NewProxyPoolService(NewProxyPoolRepository(integrationDB, r.encryptor), r.encryptor)
	state, e := pool.State(ctx)
	require.NoError(t, e)
	state, e = pool.Import(ctx, service.ProxyPoolImport{Revision: state.Revision, Mode: "replace", Text: "never.example:99"})
	require.NoError(t, e)
	state, e = pool.Import(ctx, service.ProxyPoolImport{Revision: state.Revision, Mode: "append", Text: "a.example:80:user:secret-password\nb.example:81"})
	require.NoError(t, e)
	for _, source := range sources {
		found := false
		for _, entry := range state.Entries {
			found = found || entry.ID == source.PoolEntryID
		}
		require.True(t, found)
	}
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_usage`).Scan(&after))
	require.Equal(t, 4, after)
}
func TestTicketBorrowRotationRejectsTargetsAndPreservesTargetState(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	ctx := t.Context()
	_, target := borrowIntegrationFixture(t)
	before, e := r.Target(ctx, target)
	require.NoError(t, e)
	_, e = r.QueueRotation(ctx, rotationInput("target", target), "manual")
	require.ErrorIs(t, e, service.ErrBorrowRotationInvalid)
	skipped, e := r.QueueRotation(ctx, rotationInput("source", target), "manual")
	require.NoError(t, e)
	require.Empty(t, skipped.AccountIDs)
	require.Len(t, skipped.Skipped, 1)
	require.ErrorIs(t, r.SaveRotationPolicy(ctx, target, service.BorrowRotationPolicy{Automatic: true, IntervalSeconds: 60}), service.ErrBorrowRotationInvalid)
	rotationApply(t, r, sources[0].AccountID, sources[1].AccountID)
	after, e := r.Target(ctx, target)
	require.NoError(t, e)
	require.Equal(t, before.Revision, after.Revision)
	require.Equal(t, before.UpstreamWaitUntil, after.UpstreamWaitUntil)
	require.Equal(t, before.BudgetWaitUntil, after.BudgetWaitUntil)
	env, e := r.TargetEnvironment(ctx, target)
	require.NoError(t, e)
	require.Zero(t, env.EntryID)
}

func TestTicketBorrowRotationChangedBindingCannotPartiallyApplyBatch(t *testing.T) {
	for _, mutation := range []string{"missing", "version"} {
		t.Run(mutation, func(t *testing.T) {
			r, _, sources := rotationSourceFixture(t)
			ctx := t.Context()
			in := rotationInput("source", sources[0].AccountID, sources[1].AccountID)
			_, e := r.QueueRotation(ctx, in, "manual")
			require.NoError(t, e)
			if mutation == "missing" {
				rotationExec(t, `DELETE FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, sources[0].AccountID)
			} else {
				rotationExec(t, `UPDATE ticket_borrow_proxy_bindings SET environment_version=environment_version+1 WHERE account_id=$1`, sources[0].AccountID)
			}
			require.NoError(t, r.ProcessRotations(ctx))
			require.Equal(t, "failed", rotationPhase(t, in.ID))
			var entry int64
			require.NoError(t, integrationDB.QueryRow(`SELECT entry_id FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, sources[1].AccountID).Scan(&entry))
			require.Equal(t, sources[1].PoolEntryID, entry)
		})
	}
}

func TestTicketBorrowRotationOneDuePolicyReshufflesAllSources(t *testing.T) {
	r, _, sources := rotationSourceFixture(t)
	rotationSetRunning(t, r, true)
	require.NoError(t, r.SaveRotationPolicy(t.Context(), sources[0].AccountID, service.BorrowRotationPolicy{Automatic: true, IntervalSeconds: 60}))
	for range 2 {
		before := make([]service.BorrowSource, len(sources))
		for i, source := range sources {
			saved, err := r.Source(t.Context(), source.ID)
			require.NoError(t, err)
			before[i] = *saved
		}
		rotationExec(t, `UPDATE ticket_borrow_rotation_accounts SET next_at=clock_timestamp()-INTERVAL '1 second' WHERE account_id=$1`, sources[0].AccountID)
		require.NoError(t, r.ProcessRotations(t.Context()))
		states, err := r.RotationStates(t.Context())
		require.NoError(t, err)
		require.Len(t, states, 2)
		require.NotEmpty(t, states[0].ActiveJob)
		require.Equal(t, states[0].ActiveJob, states[1].ActiveJob)
		require.NoError(t, r.ProcessRotations(t.Context()))
		for i, old := range before {
			changed, err := r.Source(t.Context(), old.ID)
			require.NoError(t, err)
			require.Equal(t, before[1-i].PoolEntryID, changed.PoolEntryID)
			require.NotEqual(t, old.FingerprintID, changed.FingerprintID)
		}
		states, err = r.RotationStates(t.Context())
		require.NoError(t, err)
		require.True(t, states[0].Automatic)
		require.False(t, states[1].Automatic)
		require.Nil(t, states[1].NextAt)
	}
}
