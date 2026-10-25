//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func candidateFixture(t *testing.T) (*ticketBorrowRepository, service.BorrowAcquisition) {
	t.Helper()
	_, pool, _ := poolFixture(t)
	_, err := pool.Import(t.Context(), service.ProxyPoolImport{Revision: 1, Mode: "append", Text: "a.example:80\nb.example:80\nc.example:80\nd.example:80"})
	require.NoError(t, err)
	repo, id := borrowIntegrationFixture(t)
	r := repo.(*ticketBorrowRepository)
	old, err := r.Settings(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() {
		current, e := r.Settings(context.Background())
		require.NoError(t, e)
		old.Revision = current.Revision
		require.NoError(t, r.SaveSettings(context.Background(), old))
		_, e = integrationDB.Exec(`DELETE FROM ticket_borrow_source_attempts WHERE target_id=$1`, id)
		require.NoError(t, e)
	})
	require.NoError(t, r.SaveSettings(t.Context(), service.BorrowSettings{Revision: old.Revision, Running: true}))
	target, err := r.Target(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, service.BorrowAcquisitionSelf, target.AcquisitionIdentity)
	a := service.BorrowAcquisition{TargetID: id, Revision: target.Revision, Identity: "business-identity", Lease: uuid.NewString()}
	ok, err := r.AcquireJob(service.WithBorrowAcquisition(t.Context(), a), "target", id, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	return r, a
}

func TestBorrowCandidateSelfPermitWithoutSourceInventory(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	permit := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, 0, permit))
	_, err := r.GrantSourceAttempt(ctx, 123, permit)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "a permit cannot be applied to a different acquisition account")
	ok, err := r.GrantSourceAttempt(ctx, 0, permit)
	require.NoError(t, err)
	require.True(t, ok)
	var account int64
	var source *int64
	require.NoError(t, integrationDB.QueryRow(`SELECT source_id,acquisition_account_id FROM ticket_borrow_source_attempts WHERE intent_id=$1`, permit).Scan(&source, &account))
	require.Nil(t, source)
	require.Equal(t, a.TargetID, account)
}

func TestBorrowCandidateConcurrentFairnessCancellationAndRestart(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	var wg sync.WaitGroup
	envs := make(chan *service.BorrowCandidateEnvironment, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "same-upstream-subject")
			if err != nil {
				errs <- err
				return
			}
			envs <- env
		}()
	}
	wg.Wait()
	close(envs)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	counts := map[int64]int{}
	devices := map[string]bool{}
	for env := range envs {
		counts[env.EntryID]++
		require.False(t, devices[env.Fingerprint.DeviceID])
		devices[env.Fingerprint.DeviceID] = true
		start, until, err := r.StartCandidateEnvironment(ctx, env.ID)
		require.NoError(t, err)
		require.WithinDuration(t, start.Add(180*time.Second), until, time.Second, "provisional acquisition lease is replaced by the observed Cookie deadline")
		require.NoError(t, r.MarkCandidateEnvironmentSent(ctx, env.ID))
		require.NoError(t, r.MarkCandidateEnvironmentSent(ctx, env.ID))
		require.NoError(t, r.ReleaseCandidateEnvironment(ctx, env.ID))
	}
	require.Len(t, counts, 4)
	for _, count := range counts {
		require.Equal(t, 2, count)
	}
	var uses int
	require.NoError(t, integrationDB.QueryRow(`SELECT sum(uses) FROM ticket_borrow_candidate_proxy_usage WHERE identity_key='same-upstream-subject'`).Scan(&uses))
	require.Equal(t, 8, uses)
	restarted := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	unused, err := restarted.AllocateCandidateEnvironment(ctx, uuid.NewString(), "same-upstream-subject")
	require.NoError(t, err)
	require.EqualValues(t, 3, unused.AllocationRound)
	require.NoError(t, restarted.ReleaseCandidateEnvironment(ctx, unused.ID))
	require.NoError(t, integrationDB.QueryRow(`SELECT sum(uses) FROM ticket_borrow_candidate_proxy_usage WHERE identity_key='same-upstream-subject'`).Scan(&uses))
	require.Equal(t, 8, uses)
	var bindings int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_proxy_bindings WHERE account_id=$1`, a.TargetID).Scan(&bindings))
	require.Zero(t, bindings)
}

func TestBorrowCandidateAdmissionModeFencesAndInflightCompletion(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	start := time.Now().Add(-5 * time.Second)
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: a.TargetID, TargetRevision: a.Revision, IdentityHash: a.Identity,
		AcquisitionIdentity: service.BorrowAcquisitionSelf, AcquisitionAccountID: a.TargetID, AcquisitionStartedAt: &start, AdmissionUntil: start.Add(80 * time.Second),
		CookieHash: uuid.NewString(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, r.Publish(ctx, route, a.Lease))
	passBorrowFixtureQuality(t)
	// Use one account slot to isolate preservation of the admitted lease.
	rotationExec(t, `UPDATE accounts SET concurrency=1 WHERE id=$1`, a.TargetID)
	got, err := r.Route(ctx, a.TargetID, a.Identity, route.ID)
	require.NoError(t, err)
	require.WithinDuration(t, route.ExpiresAt, got.UsableUntil, time.Millisecond)
	lease := uuid.NewString()
	_, err = r.ReserveRoute(ctx, a.TargetID, a.Identity, route.ID, "", lease)
	require.NoError(t, err)
	target, err := r.Target(ctx, a.TargetID)
	require.NoError(t, err)
	require.NoError(t, r.SetTargetIdentity(ctx, a.TargetID, true, target.Revision, target.AcquisitionRevision, service.BorrowAcquisitionSource))
	require.Error(t, r.CheckAcquisition(ctx, a), "mode change fences the old candidate batch")
	_, err = r.Route(ctx, a.TargetID, a.Identity, route.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "account capacity remains occupied; mode change does not cancel its lease")
	require.ErrorIs(t, r.SetTargetIdentity(ctx, a.TargetID, true, target.Revision, target.AcquisitionRevision, service.BorrowAcquisitionSelf), service.ErrBorrowConflict)
	rotationExec(t, `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, route.ID)
	_, err = r.Route(ctx, a.TargetID, a.Identity, route.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.NoError(t, r.RenewRoute(ctx, lease), "admitted response survives expiry")
	require.NoError(t, r.ReleaseRoute(ctx, lease))
}

func TestBorrowCandidateSourcePermitAndCredentialFence(t *testing.T) {
	r, a := candidateFixture(t)
	account := borrowNormalAccountFixture(t)
	selection := borrowSelectionFixture(t, r)
	selection.AccountIDs = []int64{account.ID}
	require.NoError(t, r.SaveSelection(t.Context(), selection))
	sources, err := r.Sources(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	source := sources[0]
	target, err := r.Target(t.Context(), a.TargetID)
	require.NoError(t, err)
	require.NoError(t, r.SetTargetIdentity(t.Context(), a.TargetID, true, target.Revision, target.AcquisitionRevision, service.BorrowAcquisitionSource))
	a.Lease = uuid.NewString()
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	ok, err := r.AcquireJob(ctx, "target", a.TargetID, a.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	permit := uuid.NewString()
	require.NoError(t, r.QueueSourceAttempt(ctx, source.ID, permit))
	ok, err = r.GrantSourceAttempt(ctx, source.ID, permit)
	require.NoError(t, err)
	require.True(t, ok)
	var actual int64
	require.NoError(t, integrationDB.QueryRow(`SELECT acquisition_account_id FROM ticket_borrow_source_attempts WHERE intent_id=$1`, permit).Scan(&actual))
	require.Equal(t, account.ID, actual)
	status, err := r.Target(ctx, a.TargetID)
	require.NoError(t, err)
	require.Zero(t, status.Inflight, "external source acquisition does not occupy the target background slots")
	request := probeRequest(a)
	require.NoError(t, r.BeginTargetRequest(ctx, request))
	require.NoError(t, r.EndTargetRequest(ctx, request, 200, "eof"))
	start := time.Now()
	route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: a.TargetID, TargetRevision: a.Revision, IdentityHash: a.Identity,
		AcquisitionIdentity: service.BorrowAcquisitionSource, AcquisitionAccountID: account.ID, SourceID: source.ID, SourceRevision: source.Revision,
		AcquisitionStartedAt: &start, AdmissionUntil: start.Add(80 * time.Second), ExpiresAt: start.Add(time.Hour), CookieHash: uuid.NewString()}
	require.NoError(t, integrationDB.QueryRow(`SELECT encode(sha256(convert_to(credentials->>'access_token','UTF8')),'hex'),encode(sha256(convert_to(COALESCE(credentials->>'chatgpt_account_id','')||':'||COALESCE(credentials->>'chatgpt_user_id',''),'UTF8')),'hex') FROM accounts WHERE id=$1`, account.ID).Scan(&route.SourceCredentialHash, &route.SourceSubjectHash))
	require.NoError(t, r.Publish(ctx, route, a.Lease))
	passBorrowFixtureQuality(t)
	require.NoError(t, r.FinishJob(ctx, "source", source.ID, permit, "source_cookie_acquired", 0))
	_, err = integrationDB.Exec(`UPDATE accounts SET credentials=jsonb_set(credentials,'{access_token}','"replacement"') WHERE id=$1`, account.ID)
	require.NoError(t, err)
	route.ID, route.CookieHash = uuid.NewString(), uuid.NewString()
	require.ErrorIs(t, r.Publish(ctx, route, a.Lease), service.ErrBorrowConflict, "credentials changed during target verification")
}

func TestBorrowCandidateSharedIdentityAndPoolChanges(t *testing.T) {
	r, a := candidateFixture(t)
	_, second := borrowIntegrationFixture(t)
	target, err := r.Target(t.Context(), second)
	require.NoError(t, err)
	b := service.BorrowAcquisition{TargetID: second, Revision: target.Revision, Lease: uuid.NewString(), Identity: "second-business"}
	ctxB := service.WithBorrowAcquisition(t.Context(), b)
	ok, err := r.AcquireJob(ctxB, "target", second, b.Lease)
	require.NoError(t, err)
	require.True(t, ok)
	ctxA := service.WithBorrowAcquisition(t.Context(), a)
	used := map[int64]bool{}
	for i := range 4 {
		ctx := ctxA
		if i%2 == 1 {
			ctx = ctxB
		}
		env, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "same-verified-subject")
		require.NoError(t, err)
		require.False(t, used[env.EntryID])
		used[env.EntryID] = true
		_, _, err = r.StartCandidateEnvironment(ctx, env.ID)
		require.NoError(t, err)
		require.NoError(t, r.MarkCandidateEnvironmentSent(ctx, env.ID))
		require.NoError(t, r.ReleaseCandidateEnvironment(ctx, env.ID))
	}
	env, err := r.AllocateCandidateEnvironment(ctxA, uuid.NewString(), "same-verified-subject")
	require.NoError(t, err)
	require.EqualValues(t, 2, env.AllocationRound)
	_, err = integrationDB.Exec(`UPDATE proxy_pool_entries SET enabled=false WHERE id=$1`, env.EntryID)
	require.NoError(t, err)
	_, _, err = r.StartCandidateEnvironment(ctxA, env.ID)
	require.Error(t, err)
	_, err = integrationDB.Exec(`UPDATE proxy_pool_entries SET enabled=(id=$1)`, env.EntryID)
	require.NoError(t, err)
	require.NoError(t, r.ReleaseCandidateEnvironment(ctxA, env.ID))
	first, err := r.AllocateCandidateEnvironment(ctxA, uuid.NewString(), "single-pool")
	require.NoError(t, err)
	next, err := r.AllocateCandidateEnvironment(ctxA, uuid.NewString(), "single-pool")
	require.NoError(t, err)
	require.Equal(t, first.EntryID, next.EntryID)
	require.NotEqual(t, first.Fingerprint.DeviceID, next.Fingerprint.DeviceID)
	_, err = integrationDB.Exec(`UPDATE proxy_pool_entries SET enabled=false`)
	require.NoError(t, err)
	_, err = r.AllocateCandidateEnvironment(ctxA, uuid.NewString(), "single-pool")
	require.ErrorIs(t, err, service.ErrBorrowRotationNoProxy)
}

func TestBorrowCandidateCookieDeadlineAndRollingInventory(t *testing.T) {
	for _, rolling := range []bool{false, true} {
		t.Run(fmt.Sprint(rolling), func(t *testing.T) {
			r, a := candidateFixture(t)
			settings, err := r.Settings(t.Context())
			require.NoError(t, err)
			size := 1
			if rolling {
				size = 2
			}
			settings.RoutePoolSize = &size
			require.NoError(t, r.SaveSettings(t.Context(), settings))
			a = inventoryRestart(t, r, a)
			ctx := service.WithBorrowAcquisition(t.Context(), a)
			for range 3 {
				start := time.Now()
				route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: a.TargetID, TargetRevision: a.Revision, IdentityHash: a.Identity,
					AcquisitionIdentity: service.BorrowAcquisitionSelf, AcquisitionAccountID: a.TargetID, AcquisitionStartedAt: &start,
					AdmissionUntil: start.Add(80 * time.Second), ExpiresAt: start.Add(time.Hour), CookieHash: uuid.NewString()}
				deadline, err := r.ObserveCookie(ctx, route.CookieHash, 30*time.Second)
				require.NoError(t, err)
				require.NoError(t, r.Publish(ctx, route, a.Lease))
				passBorrowFixtureQuality(t)
				got, err := r.Route(ctx, a.TargetID, a.Identity, route.ID)
				require.NoError(t, err)
				require.WithinDuration(t, deadline, got.UsableUntil, time.Millisecond)
				require.ErrorIs(t, r.Publish(ctx, route, a.Lease), service.ErrBorrowCandidateDuplicate)
			}
			require.NoError(t, r.FinishJob(ctx, "target", a.TargetID, a.Lease, "passed", 0))
			inventory, err := r.Inventory(ctx, a.TargetID, a.Identity)
			require.NoError(t, err)
			expected := 1
			if rolling {
				expected = 2
			}
			require.Equal(t, 3, inventory.Count)
			require.Equal(t, expected, inventory.Desired)
			require.Equal(t, "healthy", inventory.State)
			require.True(t, inventory.NextAt.After(inventory.ServerNow), "healthy inventory sends no periodic maintenance request")
		})
	}
}

func TestBorrowCandidateMigrationDefaultsAndLegacyCutoffs(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			tx, err := integrationDB.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.Exec(`CREATE SCHEMA candidate_migration_fixture; SET LOCAL search_path TO candidate_migration_fixture;
    CREATE TABLE ticket_borrow_targets(account_id BIGINT PRIMARY KEY,enabled BOOLEAN DEFAULT false);
    CREATE TABLE ticket_borrow_sources(id BIGINT PRIMARY KEY,account_id BIGINT);
    CREATE TABLE ticket_borrow_settings(id INTEGER PRIMARY KEY,revision BIGINT,ttl_seconds INTEGER,min_route_remaining_seconds INTEGER,replenishment JSONB);
    INSERT INTO ticket_borrow_settings VALUES(1,3,230,120,'{"enabled":false,"hold_seconds":150}');
    CREATE TABLE ticket_borrow_routes(id TEXT PRIMARY KEY,source_id BIGINT NOT NULL,cookie_hash TEXT,expires_at TIMESTAMPTZ);
    CREATE TABLE ticket_borrow_cookie_deadlines(cookie_hash TEXT,expires_at TIMESTAMPTZ);
    CREATE TABLE ticket_borrow_source_intents(source_id BIGINT NOT NULL);
    CREATE TABLE ticket_borrow_source_attempts(source_id BIGINT NOT NULL);
    CREATE TABLE proxy_pool_entries(id BIGINT PRIMARY KEY);
    CREATE TABLE ticket_borrow_replenishment(until_at TIMESTAMPTZ,reason TEXT);
    CREATE TABLE ticket_borrow_replenishment_holds(id INT);
    CREATE TABLE ticket_borrow_rotation_jobs(phase TEXT,reason TEXT,finished_at TIMESTAMPTZ);
    CREATE TABLE ticket_borrow_rotation_accounts(automatic BOOLEAN,active_job TEXT,phase TEXT,reason TEXT);`)
			require.NoError(t, err)
			if existing {
				_, err = tx.Exec(`INSERT INTO ticket_borrow_targets VALUES(1,true),(2,false);
     INSERT INTO ticket_borrow_sources VALUES(5,99);
     UPDATE ticket_borrow_settings SET revision=9,replenishment='{"enabled":true,"hold_seconds":150}';
     INSERT INTO ticket_borrow_routes VALUES('legacy',5,'known',NOW()+INTERVAL '230 seconds'),('no-cookie',5,'unknown',NOW()+INTERVAL '230 seconds');
     INSERT INTO ticket_borrow_cookie_deadlines VALUES('known',NOW()+INTERVAL '180 seconds');
     INSERT INTO ticket_borrow_replenishment VALUES(NOW()+INTERVAL '150 seconds','success');
     INSERT INTO ticket_borrow_replenishment_holds VALUES(1);
     INSERT INTO ticket_borrow_rotation_accounts VALUES(true,'job','draining','');
     INSERT INTO ticket_borrow_rotation_jobs VALUES('draining','',NULL);`)
				require.NoError(t, err)
			}
			migration, err := migrations.FS.ReadFile("295_ticket_borrow_candidate_environments.sql")
			require.NoError(t, err)
			_, err = tx.Exec(string(migration))
			require.NoError(t, err)
			var window int
			var rolling bool
			require.NoError(t, tx.QueryRow(`SELECT admission_window_seconds,(replenishment->>'enabled')::boolean FROM ticket_borrow_settings WHERE id=1`).Scan(&window, &rolling))
			expected := 80
			if existing {
				expected = 110
			}
			require.Equal(t, expected, window)
			require.Equal(t, existing, rolling)
			if existing {
				var mode string
				require.NoError(t, tx.QueryRow(`SELECT acquisition_identity FROM ticket_borrow_targets WHERE account_id=1`).Scan(&mode))
				require.Equal(t, "source", mode)
				require.NoError(t, tx.QueryRow(`SELECT acquisition_identity FROM ticket_borrow_targets WHERE account_id=2`).Scan(&mode))
				require.Equal(t, "self", mode)
				var remaining float64
				var noStart bool
				require.NoError(t, tx.QueryRow(`SELECT EXTRACT(EPOCH FROM admission_until-NOW()),acquisition_started_at IS NULL FROM ticket_borrow_routes WHERE id='legacy'`).Scan(&remaining, &noStart))
				require.InDelta(t, 60, remaining, 0.001)
				require.True(t, noStart)
				require.NoError(t, tx.QueryRow(`SELECT EXTRACT(EPOCH FROM admission_until-NOW()) FROM ticket_borrow_routes WHERE id='no-cookie'`).Scan(&remaining))
				require.InDelta(t, 110, remaining, 0.001)
				var holds int
				require.NoError(t, tx.QueryRow(`SELECT count(*) FROM ticket_borrow_replenishment_holds`).Scan(&holds))
				require.Zero(t, holds)
				require.NoError(t, tx.QueryRow(`SELECT count(*) FROM ticket_borrow_rotation_accounts WHERE automatic OR active_job IS NOT NULL`).Scan(&holds))
				require.Zero(t, holds)
			}
			_, err = tx.Exec(`INSERT INTO ticket_borrow_targets(account_id) VALUES(3)`)
			require.NoError(t, err)
			var mode string
			require.NoError(t, tx.QueryRow(`SELECT acquisition_identity FROM ticket_borrow_targets WHERE account_id=3`).Scan(&mode))
			require.Equal(t, "self", mode)
		})
	}
}

func TestBorrowCandidatePoolNewEntriesJoinCurrentRound(t *testing.T) {
	r, a := candidateFixture(t)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	env, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "long-history")
	require.NoError(t, err)
	require.NoError(t, r.ReleaseCandidateEnvironment(ctx, env.ID))
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_candidate_proxy_usage SET uses=100 WHERE identity_key='long-history'`)
	require.NoError(t, err)
	poolRepo := NewProxyPoolRepository(integrationDB, r.encryptor)
	state, err := poolRepo.Load(ctx)
	require.NoError(t, err)
	pool := service.NewProxyPoolService(poolRepo, r.encryptor)
	_, err = pool.Import(ctx, service.ProxyPoolImport{Revision: state.Revision, Mode: "append", Text: "new.example:80"})
	require.NoError(t, err)
	seen := map[int64]bool{}
	for range 5 {
		env, err := r.AllocateCandidateEnvironment(ctx, uuid.NewString(), "long-history")
		require.NoError(t, err)
		require.EqualValues(t, 101, env.AllocationRound)
		require.False(t, seen[env.EntryID], "a new entry must not be reused to catch up on old history")
		seen[env.EntryID] = true
		_, _, err = r.StartCandidateEnvironment(ctx, env.ID)
		require.NoError(t, err)
		require.NoError(t, r.MarkCandidateEnvironmentSent(ctx, env.ID))
		require.NoError(t, r.ReleaseCandidateEnvironment(ctx, env.ID))
	}
	require.Len(t, seen, 5)
}
