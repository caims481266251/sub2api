//go:build integration

package repository

import (
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func enqueueAgedInventory(t *testing.T, f *candidatePoolFixture, age time.Duration) *service.BorrowPoolCandidate {
	t.Helper()
	p := f.reserve(t)
	ctx := service.WithBorrowPoolAcquisition(t.Context(), p)
	env, err := f.r.AllocateCandidateEnvironment(ctx, p.ID, "shared-candidate-pool")
	require.NoError(t, err)
	env.StartedAt, env.AdmissionUntil, err = f.r.StartCandidateEnvironment(ctx, p.ID)
	require.NoError(t, err)
	env.StartedAt = env.StartedAt.Add(-age)
	until := env.StartedAt.Add(time.Hour)
	hash := uuid.NewString()
	require.NoError(t, f.r.SetCandidateEnvironmentDeadline(ctx, p.ID, hash, until))
	env.AdmissionUntil = until
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_candidate_environments SET started_at=$2 WHERE id=$1`, p.ID, env.StartedAt)
	require.NoError(t, err)
	source, err := f.r.Source(ctx, p.SourceID)
	require.NoError(t, err)
	c := &service.BorrowPoolCandidate{ID: p.ID, SourceID: p.SourceID,
		Cookie: http.Cookie{Name: "__oailb", Value: uuid.NewString(), MaxAge: 3600}, Expires: until, Environment: env,
		Details: &service.BorrowCandidateDetails{CandidateID: p.ID, CookieHash: hash,
			AcquisitionStartedAt: &env.StartedAt, CookieObservedAt: &env.StartedAt, UpstreamExpiresAt: &until,
			AdmissionUntil: &until, SourceAccountID: source.AccountID, SourceRevision: source.Revision}}
	require.NoError(t, integrationDB.QueryRow(`SELECT encode(sha256(convert_to(credentials->>'access_token','UTF8')),'hex'),encode(sha256(convert_to(COALESCE(credentials->>'chatgpt_account_id','')||':'||COALESCE(credentials->>'chatgpt_user_id',''),'UTF8')),'hex') FROM accounts WHERE id=$1`, source.AccountID).Scan(&c.SourceCredentialHash, &c.SourceSubjectHash))
	require.NoError(t, f.r.EnqueueCandidatePool(ctx, p, c))
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_candidate_pool SET enqueued_at=$2 WHERE id=$1`, p.ID, env.StartedAt)
	require.NoError(t, err)
	return c
}

func TestBorrowInventoryOldCookieSurvivesTaskGapAndPublishesWithNewAnchor(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	a := f.targets[0]
	c := enqueueAgedInventory(t, f, 5*time.Minute)
	require.NoError(t, f.r.RemoveCandidatePoolDemand(t.Context(), a))
	require.NoError(t, f.r.SweepCandidatePool(t.Context()))
	page, err := f.r.CandidatePoolPage(t.Context(), service.BorrowCandidatePoolQuery{})
	require.NoError(t, err)
	require.Equal(t, 1, page.Summary.Queued)
	require.NotNil(t, page.Items[0].CookieExpiresAt)
	require.Equal(t, "filling", page.Summary.ProductionState)
	f.heartbeat(t)
	claimed, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, c.ID, claimed.ID)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	require.NoError(t, f.r.CheckCandidateEnvironment(ctx, c.ID))
	route := poolCandidateRoute(a, claimed)
	require.NoError(t, f.r.Publish(ctx, route, a.Lease))
	passBorrowFixtureQuality(t)
	var anchor, deadline time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT admission_started_at,admission_until FROM ticket_borrow_routes WHERE id=$1`, route.ID).Scan(&anchor, &deadline))
	require.WithinDuration(t, c.Expires, deadline, time.Millisecond)
	require.True(t, anchor.Before(deadline))
	require.NoError(t, f.r.FinishCandidatePool(ctx, c.ID, a.Lease, "target_probe_passed"))
	var released *time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT released_at FROM ticket_borrow_candidate_environments WHERE id=$1`, c.ID).Scan(&released))
	require.Nil(t, released, "the published route retains its frozen environment")

	settings, err := f.r.Settings(ctx)
	require.NoError(t, err)
	settings.Running = false
	require.NoError(t, f.r.SaveSettings(ctx, settings))
	var after time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT admission_until FROM ticket_borrow_routes WHERE id=$1`, route.ID).Scan(&after))
	require.Equal(t, deadline, after, "stopping does not reapply the source acquisition anchor")
}

func TestBorrowInventorySurvivesBudgetEditButRejectsRevokedEnvironment(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	c := enqueueAgedInventory(t, f, 3*time.Minute)
	settings, err := f.r.Settings(t.Context())
	require.NoError(t, err)
	settings.Recovery.SourceRequestsPerMinute++
	require.NoError(t, f.r.SaveSettings(t.Context(), settings))
	require.NoError(t, f.r.SweepCandidatePool(t.Context()))
	a := inventoryRestart(t, f.r, f.targets[0])
	require.NoError(t, f.r.RegisterCandidatePoolDemand(t.Context(), a, 2))
	_, err = integrationDB.Exec(`UPDATE proxy_pool_entries SET enabled=false WHERE id=$1`, c.Environment.EntryID)
	require.NoError(t, err)
	claimed, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	require.Nil(t, claimed)
	require.NoError(t, f.r.SweepCandidatePool(t.Context()))
	var reason string
	require.NoError(t, integrationDB.QueryRow(`SELECT reason FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID).Scan(&reason))
	require.Equal(t, "candidate_environment_invalid", reason)
}

func TestBorrowInventoryFIFOAndConfirmedDeadlineNeverRenews(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	old := enqueueAgedInventory(t, f, 5*time.Minute)
	f.enqueue(t, f.reserve(t))
	p := service.BorrowPoolAcquisition{ID: old.ID, SourceID: old.SourceID}
	require.NoError(t, integrationDB.QueryRow(`SELECT owner_token,settings_revision FROM ticket_borrow_candidate_pool WHERE id=$1`, old.ID).Scan(&p.Lease, &p.SettingsRevision))
	require.NoError(t, f.r.SetCandidateEnvironmentDeadline(service.WithBorrowPoolAcquisition(t.Context(), p), old.ID, old.Details.CookieHash, time.Now().Add(2*time.Hour)))
	var deadline time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT admission_until FROM ticket_borrow_candidate_environments WHERE id=$1`, old.ID).Scan(&deadline))
	require.WithinDuration(t, old.Expires, deadline, time.Microsecond)
	c, err := f.r.ClaimCandidatePool(t.Context(), f.targets[0])
	require.NoError(t, err)
	require.Equal(t, old.ID, c.ID, "new arrivals do not starve eligible older inventory")
}

func TestBorrowInventoryReapsAbandonedRequestsWhilePaused(t *testing.T) {
	f := newCandidatePoolFixture(t, 1, 2)
	f.enqueue(t, f.reserve(t))
	a := f.targets[0]
	c, err := f.r.ClaimCandidatePool(t.Context(), a)
	require.NoError(t, err)
	q := probeRequest(a)
	q.CandidateID = c.ID
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	require.NoError(t, f.r.BeginTargetRequest(ctx, q))
	require.NoError(t, f.r.MarkTargetRequestSent(ctx, q))
	settings, err := f.r.Settings(ctx)
	require.NoError(t, err)
	settings.Running = false
	require.NoError(t, f.r.SaveSettings(ctx, settings))
	require.NoError(t, f.r.SweepCandidatePool(ctx))
	var state string
	require.NoError(t, integrationDB.QueryRow(`SELECT state FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID).Scan(&state))
	require.Equal(t, "claimed", state, "sent work keeps its live HTTP lease")
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_requests SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, q.ID)
	require.NoError(t, err)
	require.NoError(t, f.r.SweepCandidatePool(ctx))
	var outcome string
	var finished *time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT outcome,finished_at FROM ticket_borrow_requests WHERE id=$1`, q.ID).Scan(&outcome, &finished))
	require.Equal(t, "lease_expired", outcome)
	require.NotNil(t, finished)
	require.NoError(t, f.r.SweepCandidatePool(ctx), "recovery is idempotent")
	require.NoError(t, integrationDB.QueryRow(`SELECT state FROM ticket_borrow_candidate_pool WHERE id=$1`, c.ID).Scan(&state))
	require.Equal(t, "finished", state)
}
