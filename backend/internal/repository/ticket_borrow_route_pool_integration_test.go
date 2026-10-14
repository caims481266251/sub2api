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

func poolReserve(t *testing.T, r *ticketBorrowRepository, id int64, route string) (*service.BorrowRoute, string) {
	t.Helper()
	lease := uuid.NewString()
	got, err := r.ReserveRoute(t.Context(), id, "identity", route, "", lease)
	require.NoError(t, err)
	return got, lease
}
func completedPoolQuality(t *testing.T, r *ticketBorrowRepository, job *service.BorrowRouteHealthJob) service.BorrowRouteOutcome {
	t.Helper()
	q := service.BorrowRequest{ID: job.Token, Kind: "probe", Phase: "quality", TargetID: job.Route.TargetID, Identity: job.Route.IdentityHash, BatchID: job.Token, RouteID: job.Route.ID, RouteHealth: job}
	require.NoError(t, r.BeginTargetRequest(t.Context(), q))
	require.NoError(t, r.MarkTargetRequestSent(t.Context(), q))
	require.NoError(t, r.EndTargetRequest(t.Context(), q, 200, "closed"))
	return service.BorrowRouteOutcome{Success: true, Quality: &service.BorrowRouteQualityResult{Status: "passed", Model: job.QualityConfig.Model, ReasoningEffort: job.QualityConfig.ReasoningEffort, Answer: "21", HTTPStatus: 200}}
}

func TestBorrowRoutePoolCooldownGenerationAndNaturalDrain(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, id)
	a, la := poolReserve(t, r, id, routes[0].ID)
	b, lb := poolReserve(t, r, id, routes[0].ID)
	c, lc := poolReserve(t, r, id, routes[0].ID)
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, o := range []service.BorrowRouteOutcome{{Route: a, Lease: la, Reason: "route_rate_limited"}, {Route: b, Lease: lb, Reason: "route_rate_limited"}} {
		wg.Add(1)
		go func(o service.BorrowRouteOutcome) { defer wg.Done(); errs <- r.ObserveRouteOutcome(t.Context(), o) }(o)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var state string
	var gen int64
	var until time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT state,generation,cooldown_until FROM ticket_borrow_routes WHERE id=$1`, a.ID).Scan(&state, &gen, &until))
	require.Equal(t, "cooling", state)
	require.EqualValues(t, 2, gen)
	require.WithinDuration(t, start.Add(120*time.Second), until, 2*time.Second)
	require.NoError(t, r.ObserveRouteOutcome(t.Context(), service.BorrowRouteOutcome{Route: c, Lease: lc, Success: true}))
	require.ErrorIs(t, r.CheckRouteReservation(t.Context(), a, la), service.ErrBorrowUnavailable)
	require.NoError(t, r.RenewRoute(t.Context(), la), "cooling never interrupts previously admitted streams")
	other, lo := poolReserve(t, r, id, "")
	require.NotEqual(t, a.ID, other.ID)
	require.NoError(t, r.ReleaseRoute(t.Context(), lo))
	for _, l := range []string{la, lb, lc} {
		require.NoError(t, r.ReleaseRoute(t.Context(), l))
	}
	// Persistent cooldown survives a new repository instance.
	fresh := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	_, err := fresh.ReserveRoute(t.Context(), id, "identity", a.ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	rotationExec(t, `UPDATE ticket_borrow_routes SET cooldown_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, a.ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET lease_until=NULL,last_started_at=NULL,token=''`)
	job, err := fresh.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, a.ID, job.Route.ID)
	duplicate, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, duplicate)
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, r, job)))
	resumed, lr := poolReserve(t, r, id, a.ID)
	require.EqualValues(t, 3, resumed.Generation)
	leases := []string{lr}
	for range 9 {
		_, lease := poolReserve(t, r, id, a.ID)
		leases = append(leases, lease)
	}
	_, err = r.ReserveRoute(t.Context(), id, "identity", a.ID, "", uuid.NewString())
	require.Equal(t, "account_busy", service.BorrowAdmissionReason(err), "a recovered route immediately shares the full account capacity")
	require.NoError(t, r.ObserveRouteOutcome(t.Context(), service.BorrowRouteOutcome{Route: resumed, Lease: lr, Success: true}))
	for _, lease := range leases {
		require.NoError(t, r.ReleaseRoute(t.Context(), lease))
	}
}
func TestBorrowRoutePoolSharedAccountCapacityAcrossInstances(t *testing.T) {
	for _, capacity := range []int{2, 10} {
		for _, background := range []int{0, 2} {
			for _, multipleRoutes := range []bool{false, true} {
				t.Run(fmt.Sprintf("capacity=%d/background=%d/multiple=%t", capacity, background, multipleRoutes), func(t *testing.T) {
					r, id, routes := borrowRouteLoadFixture(t)
					rotationExec(t, `UPDATE accounts SET concurrency=$2 WHERE id=$1`, id, capacity)
					for range background {
						probe := uuid.NewString()
						rotationExec(t, `INSERT INTO ticket_borrow_requests(id,target_id,kind,batch_id,phase,lease_until) VALUES($1,$2,'probe',$1,'quality',clock_timestamp()+INTERVAL '60 seconds')`, probe, id)
					}
					fresh := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
					type result struct {
						lease string
						err   error
					}
					results := make(chan result, 16)
					for i := range 16 {
						go func() {
							repo := r
							if i%2 == 0 {
								repo = fresh
							}
							preferred := routes[0].ID
							if multipleRoutes {
								preferred = ""
							}
							lease := uuid.NewString()
							_, err := repo.ReserveRoute(t.Context(), id, "identity", preferred, "", lease)
							results <- result{lease, err}
						}()
					}
					var leases []string
					for range 16 {
						got := <-results
						if got.err == nil {
							leases = append(leases, got.lease)
						} else {
							require.ErrorIs(t, got.err, service.ErrBorrowNoEligibleRoute)
							require.Equal(t, "account_busy", service.BorrowAdmissionReason(got.err))
						}
					}
					require.Len(t, leases, capacity-background)
					states, err := r.RouteStatuses(t.Context(), id, "identity")
					require.NoError(t, err)
					require.Len(t, states, len(routes))
					total := 0
					for _, state := range states {
						total += state.Inflight
						require.Zero(t, state.AvailableSlots)
						require.False(t, state.Usable)
						require.Equal(t, "account_busy", state.UnavailableReason)
						if !multipleRoutes && state.ID == routes[0].ID {
							require.Equal(t, capacity-background, state.Inflight)
						}
					}
					require.Equal(t, capacity-background, total)
					if len(leases) > 0 {
						require.NoError(t, r.ReleaseRoute(t.Context(), leases[0]))
						states, err = r.RouteStatuses(t.Context(), id, "identity")
						require.NoError(t, err)
						for _, state := range states {
							require.True(t, state.Usable)
							require.Equal(t, 1, state.AvailableSlots, "one released account slot is shared by all routes")
						}
					}
					for _, lease := range leases {
						require.NoError(t, r.ReleaseRoute(t.Context(), lease))
					}
				})
			}
		}
	}
}

func TestBorrowRoutePoolInitialConcurrencyMigrationPreservesState(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA route_capacity_fixture; SET LOCAL search_path TO route_capacity_fixture;
 CREATE TABLE ticket_borrow_routes(id TEXT,state TEXT,generation INT,dispatch_limit INT DEFAULT 1,
 revoked_at TIMESTAMPTZ,inventory_eligible BOOL DEFAULT true,validation_only BOOL DEFAULT false,
 quality_valid_until TIMESTAMPTZ,cooldown_until TIMESTAMPTZ);
 INSERT INTO ticket_borrow_routes(id,state,generation,dispatch_limit,quality_valid_until,cooldown_until) VALUES
 ('new','pending_quality',1,1,NULL,NULL),('grown','normal',2,7,NOW()+INTERVAL '30 seconds',NULL),
 ('cooling','cooling',3,1,NULL,NOW()+INTERVAL '2 minutes'),('expired','expired',4,1,NULL,NULL);
 CREATE TABLE ticket_borrow_route_leases(id TEXT,generation INT); INSERT INTO ticket_borrow_route_leases VALUES('sent',2);`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("308_ticket_borrow_route_initial_concurrency.sql")
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	for id, want := range map[string]int{"new": 3, "grown": 7, "cooling": 3, "expired": 1} {
		var limit int
		require.NoError(t, tx.QueryRow(`SELECT dispatch_limit FROM ticket_borrow_routes WHERE id=$1`, id).Scan(&limit))
		require.Equal(t, want, limit)
	}
	var preserved bool
	require.NoError(t, tx.QueryRow(`SELECT state='cooling' AND generation=3 AND quality_valid_until IS NULL AND cooldown_until>NOW() FROM ticket_borrow_routes WHERE id='cooling'`).Scan(&preserved))
	require.True(t, preserved, "capacity does not bypass cooldown or create a quality pass")
	var generation, limit int
	require.NoError(t, tx.QueryRow(`SELECT generation FROM ticket_borrow_route_leases WHERE id='sent'`).Scan(&generation))
	require.Equal(t, 2, generation)
	require.NoError(t, tx.QueryRow(`INSERT INTO ticket_borrow_routes(id,state,generation) VALUES('after','pending_quality',1) RETURNING dispatch_limit`).Scan(&limit))
	require.Equal(t, 3, limit)
}

func TestBorrowRoutePoolAtomicCapacityAndExpiry(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=3 WHERE id=$1`, id)
	var leases []string
	seen := map[string]bool{}
	for range 3 {
		route, l := poolReserve(t, r, id, "")
		require.False(t, seen[route.ID])
		seen[route.ID] = true
		leases = append(leases, l)
	}
	_, err := r.ReserveRoute(t.Context(), id, "identity", "", "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	rotationExec(t, `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE target_id=$1`, id)
	require.NoError(t, r.SweepRoutePool(t.Context()))
	for _, l := range leases {
		require.NoError(t, r.RenewRoute(t.Context(), l))
		require.NoError(t, r.ReleaseRoute(t.Context(), l))
	}
	_, err = r.ReserveRoute(t.Context(), id, "identity", routes[0].ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
}
func TestBorrowRoutePoolAllCoolingAndLongRetryAfter(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, id)
	wait := time.Now().Add(5 * time.Minute)
	for _, route := range routes {
		got, l := poolReserve(t, r, id, route.ID)
		require.NoError(t, r.ObserveRouteOutcome(t.Context(), service.BorrowRouteOutcome{Route: got, Lease: l, Reason: "route_rate_limited", RetryAt: &wait}))
		require.NoError(t, r.ReleaseRoute(t.Context(), l))
	}
	_, err := r.Route(t.Context(), id, "identity", "")
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	inv, err := r.Inventory(t.Context(), id, "identity")
	require.NoError(t, err)
	require.Zero(t, inv.Count)
	require.Equal(t, service.DefaultBorrowRoutePoolSize, inv.Desired)
	var until time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT cooldown_until FROM ticket_borrow_routes WHERE id=$1`, routes[0].ID).Scan(&until))
	require.WithinDuration(t, wait, until, time.Millisecond)
	cfg, err := r.Settings(t.Context())
	require.NoError(t, err)
	cfg.RoutePoolSize = new(1)
	cfg.RouteCooldownSeconds = new(1)
	require.NoError(t, r.SaveSettings(t.Context(), cfg))
	var after time.Time
	require.NoError(t, integrationDB.QueryRow(`SELECT cooldown_until FROM ticket_borrow_routes WHERE id=$1`, routes[0].ID).Scan(&after))
	require.Equal(t, until, after)
	// Restore defaults for other fixtures without touching any route deadlines.
	cfg, err = r.Settings(t.Context())
	require.NoError(t, err)
	cfg.RoutePoolSize = new(service.DefaultBorrowRoutePoolSize)
	cfg.RouteCooldownSeconds = new(120)
	require.NoError(t, r.SaveSettings(t.Context(), cfg))
}

func TestBorrowRoutePoolStaleHealthCannotRecover(t *testing.T) {
	for _, kind := range []string{"lease_expired", "route_owner", "credential", "generation", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			r, id, routes := borrowRouteLoadFixture(t)
			rotationExec(t, `UPDATE ticket_borrow_routes SET state='cooling',cooldown_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, routes[0].ID)
			rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
			job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
			require.NoError(t, err)
			require.NotNil(t, job)
			require.NoError(t, r.CheckRouteHealth(t.Context(), job, "identity"))
			switch kind {
			case "lease_expired":
				rotationExec(t, `UPDATE ticket_borrow_routes SET health_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, routes[0].ID)
			case "route_owner":
				rotationExec(t, `UPDATE ticket_borrow_routes SET health_token='new-owner' WHERE id=$1`, routes[0].ID)
			case "credential":
				rotationExec(t, `UPDATE accounts SET credentials=credentials||'{"access_token":"changed"}' WHERE id=$1`, id)
			case "generation":
				rotationExec(t, `UPDATE ticket_borrow_routes SET generation=generation+1 WHERE id=$1`, routes[0].ID)
			case "expiry":
				rotationExec(t, `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, routes[0].ID)
			}
			require.Error(t, r.CheckRouteHealth(t.Context(), job, "identity"))
			require.NoError(t, r.FinishRouteHealth(t.Context(), job, service.BorrowRouteOutcome{Success: true}))
			var state string
			require.NoError(t, integrationDB.QueryRow(`SELECT state FROM ticket_borrow_routes WHERE id=$1`, routes[0].ID).Scan(&state))
			require.NotEqual(t, "normal", state)
			rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
		})
	}
}
func TestBorrowRoutePoolHealthSharesCapacityAndBudget(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=1 WHERE id=$1`, id)
	rotationExec(t, `UPDATE ticket_borrow_routes SET state='cooling',cooldown_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
	_, lease := poolReserve(t, r, id, routes[1].ID)
	job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, job)
	require.NoError(t, r.ReleaseRoute(t.Context(), lease))
	job, err = r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	_, err = r.ReserveRoute(t.Context(), id, "identity", routes[1].ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, r, job)))
	var checks int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_requests WHERE target_id=$1 AND kind='probe' AND phase='quality' AND finished_at IS NOT NULL`, id).Scan(&checks))
	require.Equal(t, 1, checks)
}
func TestBorrowRoutePoolDoesNotOverproduceWhileChecksPending(t *testing.T) {
	r, _, sources, a := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), a)
	first := replenishmentRoute(t, a, sources[0], 10*time.Second)
	require.NoError(t, r.Publish(ctx, first, a.Lease))
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
	need, err := r.NeedsCandidate(ctx, a, nil)
	require.NoError(t, err)
	require.False(t, need)
	inv, err := r.Inventory(ctx, a.TargetID, a.Identity)
	require.NoError(t, err)
	require.Zero(t, inv.Count)
	require.Equal(t, 2, inv.StoredCount)
	_, err = r.Route(ctx, a.TargetID, a.Identity, first.ID)
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "mint/continue publication remains pending quality")
	// Already verified concurrent candidates retain their publication right.
	rotationExec(t, `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, first.ID)
	need, err = r.NeedsCandidate(ctx, a, nil)
	require.NoError(t, err)
	require.True(t, need)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, a, sources[0], 230*time.Second), a.Lease))
}

func TestBorrowRoutePoolUpgradeRemovesSettingsWithoutRevivingHistory(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA pool_upgrade_fixture; SET LOCAL search_path TO pool_upgrade_fixture;
 CREATE TABLE ticket_borrow_settings(id INTEGER PRIMARY KEY,ttl_seconds INT,prewarm_seconds INT,min_route_remaining_seconds INT,admission_window_seconds INT,replenishment JSONB,renew_before_seconds INT);
 INSERT INTO ticket_borrow_settings VALUES(1,230,0,0,80,'{"enabled":true}',15);
 CREATE TABLE ticket_borrow_targets(renew_before_seconds INT);
 CREATE TABLE ticket_borrow_routes(id TEXT,target_id BIGINT,cookie_hash TEXT,expires_at TIMESTAMPTZ,admission_until TIMESTAMPTZ,checked_at TIMESTAMPTZ,revoked_at TIMESTAMPTZ,inventory_eligible BOOL,validation_only BOOL);
 INSERT INTO ticket_borrow_routes VALUES('expired',1,'e',NOW()+INTERVAL '1 hour',NOW()-INTERVAL '1 second',NOW(),NULL,true,false),('normal',1,'n',NOW()+INTERVAL '1 hour',NOW()+INTERVAL '30 seconds',NOW(),NULL,true,false),('retired',1,'r',NOW()+INTERVAL '1 hour',NOW()+INTERVAL '30 seconds',NOW(),NULL,false,false);
 CREATE TABLE ticket_borrow_route_leases(id TEXT);
 CREATE VIEW ticket_borrow_active_replenishment AS SELECT 1;`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("303_ticket_borrow_available_route_pool.sql")
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	var removed int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema='pool_upgrade_fixture' AND table_name='ticket_borrow_settings' AND column_name IN ('ttl_seconds','prewarm_seconds','min_route_remaining_seconds','admission_window_seconds','replenishment','renew_before_seconds')`).Scan(&removed))
	require.Zero(t, removed)
	var size, cooldown int
	require.NoError(t, tx.QueryRow(`SELECT route_pool_size,route_cooldown_seconds FROM ticket_borrow_settings`).Scan(&size, &cooldown))
	require.Equal(t, 4, size)
	require.Equal(t, 120, cooldown)
	for id, want := range map[string]string{"expired": "expired", "normal": "normal", "retired": "invalid"} {
		var state string
		require.NoError(t, tx.QueryRow(`SELECT state FROM ticket_borrow_routes WHERE id=$1`, id).Scan(&state))
		require.Equal(t, want, state)
	}
	var short bool
	require.NoError(t, tx.QueryRow(`SELECT admission_until<expires_at FROM ticket_borrow_routes WHERE id='normal'`).Scan(&short))
	require.True(t, short, "historical cutoff must not be extended")
}

func TestBorrowRoutePoolPendingGateAndNaturalDrain(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, id)
	old, lease := poolReserve(t, r, id, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_routes SET state='pending_probe',generation=generation+1,health_checked_at=NULL WHERE id=$1`, old.ID)
	require.ErrorIs(t, r.CheckRouteReservation(t.Context(), old, lease), service.ErrBorrowUnavailable)
	require.NoError(t, r.RenewRoute(t.Context(), lease))
	require.NoError(t, r.ObserveRouteOutcome(t.Context(), service.BorrowRouteOutcome{Route: old, Lease: lease, Success: true}))
	_, err := r.ReserveRoute(t.Context(), id, "identity", old.ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
	job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, old.ID, job.Route.ID)
	statuses, err := r.RouteStatuses(t.Context(), id, "identity")
	require.NoError(t, err)
	for _, s := range statuses {
		if s.ID == old.ID {
			require.Equal(t, "pending_quality", s.State)
			require.False(t, s.Usable)
			require.True(t, s.Checking)
			require.Equal(t, 1, s.Inflight)
		}
	}
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, r, job)))
	require.NoError(t, r.RenewRoute(t.Context(), lease))
	require.NoError(t, r.ReleaseRoute(t.Context(), lease))
	current, newLease := poolReserve(t, r, id, old.ID)
	require.Greater(t, current.Generation, old.Generation)
	require.NoError(t, r.ReleaseRoute(t.Context(), newLease))
}

func TestBorrowRoutePoolRecoveryRequiresCompletedQuality(t *testing.T) {
	for _, kind := range []string{"ordinary_ok", "missing_quality", "failed_quality_request", "wrong_answer", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			r, id, routes := borrowRouteLoadFixture(t)
			rotationExec(t, `UPDATE ticket_borrow_routes SET state='cooling',cooldown_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, routes[0].ID)
			rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
			job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
			require.NoError(t, err)
			require.NotNil(t, job)
			o := service.BorrowRouteOutcome{Success: true}
			switch kind {
			case "missing_quality":
				o = completedPoolQuality(t, r, job)
				o.Quality = nil
			case "failed_quality_request":
				o = completedPoolQuality(t, r, job)
				rotationExec(t, `UPDATE ticket_borrow_requests SET status=429 WHERE batch_id=$1 AND phase='quality'`, job.Token)
			case "wrong_answer":
				o = completedPoolQuality(t, r, job)
				o.Quality.Answer = "29"
			case "cancelled":
				o.Success = false
			}
			require.NoError(t, r.FinishRouteHealth(t.Context(), job, o))
			var state string
			require.NoError(t, integrationDB.QueryRow(`SELECT state FROM ticket_borrow_routes WHERE id=$1`, job.Route.ID).Scan(&state))
			require.NotEqual(t, "normal", state)
			_, err = r.ReserveRoute(t.Context(), id, "identity", job.Route.ID, "", uuid.NewString())
			require.ErrorIs(t, err, service.ErrBorrowUnavailable)
			if kind == "cancelled" {
				require.Equal(t, "pending_quality", state)
				rotationExec(t, `UPDATE ticket_borrow_routes SET health_checked_at=clock_timestamp()-INTERVAL '31 seconds' WHERE id=$1`, job.Route.ID)
				rotationExec(t, `UPDATE ticket_borrow_route_health SET last_started_at=NULL`)
				retry, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
				require.NoError(t, err)
				require.NotNil(t, retry)
				require.NoError(t, r.FinishRouteHealth(t.Context(), retry, completedPoolQuality(t, r, retry)))
			}
		})
	}
}

func TestBorrowRoutePoolProbeBudgetAndLostOwner(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	cfg, err := r.Settings(t.Context())
	require.NoError(t, err)
	old := cfg
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		current, e := r.Settings(ctx)
		require.NoError(t, e)
		old.Revision = current.Revision
		require.NoError(t, r.SaveSettings(ctx, old))
	})
	rotationExec(t, `UPDATE ticket_borrow_routes SET state='pending_probe' WHERE id=$1`, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
	rotationExec(t, `UPDATE ticket_borrow_settings SET recovery=jsonb_set(recovery,'{probe_requests_per_minute}','1')`)
	job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job, "one request budget is sufficient for pooled-route quality recovery")
	q := service.BorrowRequest{ID: job.Token, Kind: "probe", Phase: "quality", TargetID: id, Identity: "identity", RouteID: job.Route.ID, BatchID: job.Token, RouteHealth: job}
	for _, phase := range []string{"mint", "continue"} {
		rejected := q
		rejected.Phase = phase
		require.ErrorIs(t, r.BeginTargetRequest(t.Context(), rejected), service.ErrBorrowUnavailable)
	}
	require.NoError(t, r.BeginTargetRequest(t.Context(), q))
	require.ErrorIs(t, r.BeginTargetRequest(t.Context(), q), service.ErrBorrowUnavailable, "a quality job sends at most one request")
	rotationExec(t, `UPDATE ticket_borrow_routes SET health_token='replacement-owner' WHERE id=$1`, job.Route.ID)
	require.ErrorIs(t, r.MarkTargetRequestSent(t.Context(), q), service.ErrBorrowUnavailable)
	require.NoError(t, r.EndTargetRequest(t.Context(), q, 0, "cancelled"))
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, service.BorrowRouteOutcome{}))
	var used int
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_requests WHERE target_id=$1`, id).Scan(&used))
	require.Equal(t, 1, used, "the quality request reuses the health reservation")
	rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
}

func TestBorrowRoutePoolReprobeMigrationPreservesHistoryAndLeases(t *testing.T) {
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE SCHEMA route_reprobe_fixture; SET LOCAL search_path TO route_reprobe_fixture;
 CREATE TABLE ticket_borrow_routes(id TEXT,state TEXT CONSTRAINT ticket_borrow_routes_state_check CHECK(state IN ('normal','cooling','expired','invalid')),generation BIGINT,dispatch_limit INT,health_token TEXT,health_until TIMESTAMPTZ,health_checked_at TIMESTAMPTZ,revoked_at TIMESTAMPTZ,inventory_eligible BOOL,validation_only BOOL,expires_at TIMESTAMPTZ,admission_until TIMESTAMPTZ);
 INSERT INTO ticket_borrow_routes VALUES('old','normal',1,10,'old-check',NOW()+INTERVAL '1 minute',NOW(),NULL,true,false,NOW()+INTERVAL '1 hour',NOW()+INTERVAL '1 hour'),('cooled','cooling',2,1,'',NULL,NULL,NULL,true,false,NOW()+INTERVAL '1 hour',NOW()+INTERVAL '1 hour'),('expired','expired',3,1,'',NULL,NULL,NULL,true,false,NOW()-INTERVAL '1 minute',NOW()-INTERVAL '1 minute');
 CREATE TABLE ticket_borrow_route_health(token TEXT,lease_until TIMESTAMPTZ); INSERT INTO ticket_borrow_route_health VALUES('old-check',NOW()+INTERVAL '1 minute');
 CREATE TABLE ticket_borrow_route_leases(id TEXT,generation BIGINT); INSERT INTO ticket_borrow_route_leases VALUES('live-business',1);
 CREATE TABLE ticket_borrow_settings(id INT,route_pool_size INT DEFAULT 4,revision INT); INSERT INTO ticket_borrow_settings VALUES(1,4,7),(2,16,8);`)
	require.NoError(t, err)
	raw, err := migrations.FS.ReadFile("304_ticket_borrow_route_reprobe.sql")
	require.NoError(t, err)
	_, err = tx.Exec(string(raw))
	require.NoError(t, err)
	for id, want := range map[string]string{"old": "pending_probe", "cooled": "cooling", "expired": "expired"} {
		var state string
		require.NoError(t, tx.QueryRow(`SELECT state FROM ticket_borrow_routes WHERE id=$1`, id).Scan(&state))
		require.Equal(t, want, state)
	}
	var generation, size, revision int
	require.NoError(t, tx.QueryRow(`SELECT generation FROM ticket_borrow_route_leases WHERE id='live-business'`).Scan(&generation))
	require.Equal(t, 1, generation)
	require.NoError(t, tx.QueryRow(`SELECT route_pool_size,revision FROM ticket_borrow_settings WHERE id=1`).Scan(&size, &revision))
	require.Equal(t, 128, size)
	require.Equal(t, 8, revision)
	require.NoError(t, tx.QueryRow(`SELECT route_pool_size FROM ticket_borrow_settings WHERE id=2`).Scan(&size))
	require.Equal(t, 16, size)
	_, err = tx.Exec(`INSERT INTO ticket_borrow_settings(id,revision) VALUES(3,1)`)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(`SELECT route_pool_size FROM ticket_borrow_settings WHERE id=3`).Scan(&size))
	require.Equal(t, 128, size)
}

func TestBorrowRoutePoolAbandonedProbeReclaimsWithoutRevival(t *testing.T) {
	r, _, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE ticket_borrow_routes SET state='pending_probe' WHERE id=$1`, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
	abandoned, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, abandoned)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET lease_until=clock_timestamp()-INTERVAL '1 second',last_started_at=NULL`)
	rotationExec(t, `UPDATE ticket_borrow_routes SET health_until=clock_timestamp()-INTERVAL '1 second',health_checked_at=clock_timestamp()-INTERVAL '61 seconds' WHERE id=$1`, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_requests SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, abandoned.Token)
	replacement, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, replacement)
	require.NoError(t, r.FinishRouteHealth(t.Context(), abandoned, service.BorrowRouteOutcome{Success: true}))
	require.NoError(t, r.CheckRouteHealth(t.Context(), replacement, "identity"))
	require.NoError(t, r.FinishRouteHealth(t.Context(), replacement, completedPoolQuality(t, r, replacement)))
}

func TestBorrowRoutePoolIdleReprobeRestoresSharedCapacity(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=10 WHERE id=$1`, id)
	rotationExec(t, `UPDATE ticket_borrow_routes SET quality_valid_until=clock_timestamp()-INTERVAL '1 second',checked_at=clock_timestamp()-INTERVAL '61 seconds',last_success_at=clock_timestamp()-INTERVAL '61 seconds' WHERE id=$1`, routes[0].ID)
	rotationExec(t, `UPDATE ticket_borrow_route_health SET token='',lease_until=NULL,last_started_at=NULL`)
	job, err := r.AcquireRouteHealth(t.Context(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, job)
	_, err = r.ReserveRoute(t.Context(), id, "identity", job.Route.ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.NoError(t, r.FinishRouteHealth(t.Context(), job, completedPoolQuality(t, r, job)))
	states, err := r.RouteStatuses(t.Context(), id, "identity")
	require.NoError(t, err)
	for _, state := range states {
		require.True(t, state.Usable)
		require.Equal(t, 10, state.AvailableSlots, "quality recovery permits all shared account slots immediately")
	}
}
