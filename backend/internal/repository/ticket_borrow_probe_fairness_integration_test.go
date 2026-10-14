//go:build integration

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func fairnessRoute(t *testing.T, f *candidatePoolFixture, state string) string {
	t.Helper()
	a := f.targets[0]
	secret, err := f.r.seal(service.BorrowRouteSecret{})
	require.NoError(t, err)
	id := uuid.NewString()
	rotationExec(t, `INSERT INTO ticket_borrow_routes(id,target_id,target_revision,source_id,identity_hash,cookie_hash,secret,gateway,expires_at,admission_until,checked_at,state,cooldown_until)
 VALUES($1,$2,$3,$4,$5,$6,$7,'gateway.example',clock_timestamp()+INTERVAL '1 hour',clock_timestamp()+INTERVAL '1 hour',clock_timestamp(),$8,clock_timestamp()-INTERVAL '1 minute')`, id, a.TargetID, a.Revision, f.sources[0].ID, a.Identity, uuid.NewString(), secret, state)
	return id
}

func fairnessCandidateShot(t *testing.T, f *candidatePoolFixture, c *service.BorrowPoolCandidate, phase string) {
	t.Helper()
	a := f.targets[0]
	q := service.BorrowRequest{ID: uuid.NewString(), Kind: "probe", Phase: phase, TargetID: a.TargetID, Revision: a.Revision, Identity: a.Identity, BatchID: a.Lease, CandidateID: c.ID, SourceID: c.SourceID}
	require.NoError(t, f.r.BeginTargetRequest(t.Context(), q))
	require.NoError(t, f.r.MarkTargetRequestSent(t.Context(), q))
	require.NoError(t, f.r.EndTargetRequest(t.Context(), q, 200, "closed"))
}

func fairnessClaim(t *testing.T, f *candidatePoolFixture) *service.BorrowPoolCandidate {
	t.Helper()
	f.heartbeat(t)
	c, err := f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.NoError(t, err)
	return c
}

func fairnessHealth(t *testing.T, f *candidatePoolFixture) *service.BorrowRouteHealthJob {
	t.Helper()
	rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
	job, err := f.r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	return job
}

func TestBorrowProbeFairnessContinuousCandidatesYieldToQuality(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, a.TargetID)
	for range 4 {
		f.enqueue(t, f.reserve(t))
	}
	first, second := fairnessClaim(t, f), fairnessClaim(t, f)
	require.NotNil(t, first)
	require.NotNil(t, second)
	pending := fairnessRoute(t, f, "pending_quality")
	fairnessRoute(t, f, "cooling")
	require.Nil(t, fairnessHealth(t, f), "already claimed candidates own their positions before mint")
	fairnessCandidateShot(t, f, first, "mint")
	require.Nil(t, fairnessHealth(t, f), "the mint/continue gap cannot lose the candidate's position")
	fairnessCandidateShot(t, f, first, "continue")
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), first.ID, a.Lease, "target_ticket_changed"))
	// Reproduce the live hot loop: immediately try to consume another candidate
	// before the periodic health worker gets a chance to run.
	require.Nil(t, fairnessClaim(t, f), "continuous candidates must leave the released position for quality")
	job := fairnessHealth(t, f)
	require.NotNil(t, job)
	require.Equal(t, pending, job.Route.ID, "new quality checks precede old cooling recovery")
	fairnessCandidateShot(t, f, second, "mint")
	fairnessCandidateShot(t, f, second, "continue")
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), second.ID, a.Lease, "target_ticket_changed"))
	third := fairnessClaim(t, f)
	require.NotNil(t, third, "slow quality checks must leave capacity for candidate supply")
	require.Nil(t, fairnessHealth(t, f), "another health job cannot steal the admitted candidate position")
	fairnessCandidateShot(t, f, third, "mint")
	require.NoError(t, f.r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, f.r, job)))
	fairnessCandidateShot(t, f, third, "continue")
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), third.ID, a.Lease, "target_ticket_changed"))
}

func TestBorrowProbeFairnessSinglePositionAlternates(t *testing.T) {
	t.Run("probe_limit", func(t *testing.T) { fairnessSinglePosition(t, 1, 10) })
	t.Run("account_limit", func(t *testing.T) { fairnessSinglePosition(t, 2, 1) })
}

func fairnessSinglePosition(t *testing.T, slots, accountCapacity int) {
	t.Helper()
	f := newCandidatePoolFixture(t, 1, slots)
	a := f.targets[0]
	rotationExec(t, `UPDATE accounts SET concurrency=$2 WHERE id=$1`, a.TargetID, accountCapacity)
	for range 3 {
		f.enqueue(t, f.reserve(t))
	}
	fairnessRoute(t, f, "pending_quality")
	fairnessRoute(t, f, "cooling")
	require.Nil(t, fairnessClaim(t, f))
	first := fairnessHealth(t, f)
	require.NotNil(t, first)
	require.NoError(t, f.r.FinishRouteHealth(t.Context(), first, completedPoolQuality(t, f.r, first)))
	require.Nil(t, fairnessHealth(t, f), "a waiting candidate gets the next single-position turn")
	c := fairnessClaim(t, f)
	require.NotNil(t, c)
	fairnessCandidateShot(t, f, c, "mint")
	fairnessCandidateShot(t, f, c, "continue")
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), c.ID, a.Lease, "target_ticket_changed"))
	require.Nil(t, fairnessClaim(t, f), "the next free turn returns to overdue route maintenance")
	second := fairnessHealth(t, f)
	require.NotNil(t, second)
	require.NotEqual(t, first.Route.ID, second.Route.ID)
	require.NoError(t, f.r.FinishRouteHealth(t.Context(), second, completedPoolQuality(t, f.r, second)))
	require.NotNil(t, fairnessClaim(t, f))
}

func TestBorrowProbeFairnessPreservesSingleQualityRequestBudget(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, a.TargetID)
	rotationExec(t, `UPDATE ticket_borrow_settings SET recovery=jsonb_set(recovery,'{probe_requests_per_minute}','3')`)
	for range 2 {
		f.enqueue(t, f.reserve(t))
	}
	fairnessRoute(t, f, "cooling")
	job := fairnessHealth(t, f)
	require.NotNil(t, job)
	candidate := fairnessClaim(t, f)
	require.NotNil(t, candidate, "one quality request leaves two requests for a candidate")
	require.NoError(t, f.r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, f.r, job)))
	fairnessCandidateShot(t, f, candidate, "mint")
	fairnessCandidateShot(t, f, candidate, "continue")
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), candidate.ID, a.Lease, "target_ticket_changed"))
	require.Nil(t, fairnessClaim(t, f), "the three recorded requests exhaust the shared budget")
	rotationExec(t, `UPDATE ticket_borrow_requests SET reserved_at=clock_timestamp()-INTERVAL '61 seconds' WHERE target_id=$1`, a.TargetID)
	require.NotNil(t, fairnessClaim(t, f), "supply resumes when the existing budget window frees")
}

func TestBorrowProbeFairnessSelfWaiterCannotBlockOtherTarget(t *testing.T) {
	f := newCandidatePoolFixture(t, 2, 2)
	intents := []string{uuid.NewString(), uuid.NewString()}
	for i, a := range f.targets {
		rotationExec(t, `UPDATE ticket_borrow_targets SET acquisition_identity='self' WHERE account_id=$1`, a.TargetID)
		require.NoError(t, f.r.QueueSourceAttempt(service.WithBorrowAcquisition(t.Context(), a), 0, intents[i]))
	}
	fairnessRoute(t, f, "pending_quality")
	granted, err := f.r.GrantSourceAttempt(service.WithBorrowAcquisition(t.Context(), f.targets[0]), 0, intents[0])
	require.NoError(t, err)
	require.False(t, granted, "the first target yields to its due quality check")
	granted, err = f.r.GrantSourceAttempt(service.WithBorrowAcquisition(t.Context(), f.targets[1]), 0, intents[1])
	require.NoError(t, err)
	require.True(t, granted, "the yielded target must also be excluded from source arbitration")
}

func TestBorrowProbeFairnessRecoveryBudgetCannotBlockOtherTarget(t *testing.T) {
	f := newCandidatePoolFixture(t, 2, 2)
	rotationExec(t, `UPDATE ticket_borrow_settings SET recovery=jsonb_set(recovery,'{probe_requests_per_minute}','4')`)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET last_target_id=0`)
	fairnessRoute(t, f, "cooling")
	for range 4 {
		rotationExec(t, `INSERT INTO ticket_borrow_requests(id,target_id,kind,batch_id,source_id,phase,lease_until,finished_at,status)
 VALUES($1,$2,'probe',$1,0,'mint',clock_timestamp(),clock_timestamp(),200)`, uuid.NewString(), f.targets[0].TargetID)
	}
	other := &candidatePoolFixture{r: f.r, targets: f.targets[1:], sources: f.sources}
	fairnessRoute(t, other, "pending_quality")
	job := fairnessHealth(t, f)
	require.NotNil(t, job)
	require.Equal(t, f.targets[1].TargetID, job.Route.TargetID, "an exhausted target cannot monopolize quality target selection")
}

func TestBorrowProbeFairnessCoordinatorStartsDuringQualityChecks(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, a.TargetID)
	f.enqueue(t, f.reserve(t))
	// Reproduce a restart with existing pooled routes and no candidate task.
	rotationExec(t, `UPDATE ticket_borrow_targets SET lease_token='',lease_until=NULL,last_batch_started_at=clock_timestamp()-INTERVAL '2 seconds' WHERE account_id=$1`, a.TargetID)
	for range 3 {
		fairnessRoute(t, f, "cooling")
	}
	first, second := fairnessHealth(t, f), fairnessHealth(t, f)
	require.NotNil(t, first)
	require.NotNil(t, second)
	a.Lease = uuid.NewString()
	ok, err := f.r.AcquireJob(service.WithBorrowAcquisition(t.Context(), a), "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.True(t, ok, "quality checks cannot prevent the candidate coordinator from registering demand")
	f.targets[0] = a
	f.heartbeat(t)
	require.Nil(t, fairnessClaim(t, f), "the coordinator cannot bypass occupied HTTP capacity")
	require.NoError(t, f.r.FinishRouteHealth(t.Context(), first, completedPoolQuality(t, f.r, first)))
	require.Nil(t, fairnessHealth(t, f), "the newly registered candidate demand gets the released position")
	candidate := fairnessClaim(t, f)
	require.NotNil(t, candidate)
	fairnessCandidateShot(t, f, candidate, "mint")
	fairnessCandidateShot(t, f, candidate, "continue")
	require.NoError(t, f.r.FinishCandidatePool(t.Context(), candidate.ID, a.Lease, "target_ticket_changed"))
	require.NoError(t, f.r.FinishRouteHealth(t.Context(), second, completedPoolQuality(t, f.r, second)))
}
