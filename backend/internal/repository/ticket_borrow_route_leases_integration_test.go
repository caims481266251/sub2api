//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var _ service.BorrowRouteLeaseRepository = (*ticketBorrowRepository)(nil)

func borrowRouteLoadFixture(t *testing.T) (*ticketBorrowRepository, int64, []*service.BorrowRoute) {
	t.Helper()
	repo, id := borrowIntegrationFixture(t)
	r := repo.(*ticketBorrowRepository)
	target, err := r.Target(t.Context(), id)
	require.NoError(t, err)
	secret, err := r.seal(service.BorrowRouteSecret{})
	require.NoError(t, err)
	routes := make([]*service.BorrowRoute, 0, 3)
	for i := range 3 {
		route := &service.BorrowRoute{ID: uuid.NewString(), TargetID: id, TargetRevision: target.Revision, SourceID: 1, IdentityHash: "identity", CookieHash: uuid.NewString(), Gateway: fmt.Sprintf("gateway-%d.example", i), ExpiresAt: time.Now().Add(time.Duration(220-i*10) * time.Second)}
		_, err = integrationDB.Exec(`INSERT INTO ticket_borrow_routes(id,target_id,target_revision,source_id,identity_hash,cookie_hash,gateway,secret,expires_at,checked_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp()-$10*INTERVAL '1 second')`, route.ID, id, route.TargetRevision, route.SourceID, route.IdentityHash, route.CookieHash, route.Gateway, secret, route.ExpiresAt, i)
		require.NoError(t, err)
		routes = append(routes, route)
	}
	return r, id, routes
}

func TestTicketBorrowRoutesPreferLatestAcrossInstances(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	fresh := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	var wg sync.WaitGroup
	errors := make(chan error, 30)
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo := r
			if i%2 == 0 {
				repo = fresh
			}
			_, err := repo.ReserveRoute(t.Context(), id, "identity", "", fmt.Sprintf("new-session-%d", i), uuid.NewString())
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	states, err := r.RouteStatuses(t.Context(), id, "identity")
	require.NoError(t, err)
	require.Len(t, states, 3)
	for _, state := range states {
		if state.ID == routes[0].ID {
			require.Equal(t, 30, state.Inflight)
		} else {
			require.Zero(t, state.Inflight)
		}
		require.True(t, state.Usable && state.InventoryEligible)
	}
	// Read-only eligibility must not change occupancy; occupancy does not override publication recency.
	for range 3 {
		route, err := r.Route(t.Context(), id, "identity", "")
		require.NoError(t, err)
		require.Equal(t, routes[0].ID, route.ID)
	}
	lease := uuid.NewString()
	chosen, err := fresh.ReserveRoute(t.Context(), id, "identity", "", "", lease)
	require.NoError(t, err)
	require.Equal(t, routes[0].ID, chosen.ID)
	require.NoError(t, fresh.ReleaseRoute(t.Context(), lease))
	require.NoError(t, fresh.ReleaseRoute(t.Context(), lease), "release is idempotent")
}

func TestTicketBorrowRoutesExactReservationRetainsBindingFence(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	var wg sync.WaitGroup
	results := make(chan string, 12)
	errors := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			route, err := r.ReserveRoute(t.Context(), id, "identity", "", "same-conversation", uuid.NewString())
			errors <- err
			if err == nil {
				results <- route.ID
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	for route := range results {
		require.Equal(t, routes[0].ID, route)
	}
	// Unbound reads prefer newest even under load. Exact-route callers retain
	// their binding fence; business requests use ReserveLatestRoute instead.
	other, err := r.ReserveRoute(t.Context(), id, "identity", "", "other", uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, routes[0].ID, other.ID)
	_, err = r.ReserveRoute(t.Context(), id, "identity", routes[1].ID, "same-conversation", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable, "conflicting response/session bindings cannot migrate")
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET inventory_eligible=false WHERE id=$1`, routes[0].ID)
	require.NoError(t, err)
	bound, err := r.ReserveRoute(t.Context(), id, "identity", "", "same-conversation", uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, routes[0].ID, bound.ID)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET expires_at=clock_timestamp() WHERE id=$1`, routes[0].ID)
	require.NoError(t, err)
	_, err = r.ReserveRoute(t.Context(), id, "identity", "", "same-conversation", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
}

func TestTicketBorrowRoutesEligibilityAndOrphanLeases(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	ctx := t.Context()
	lease := uuid.NewString()
	_, err := r.ReserveRoute(ctx, id, "identity", "", "", lease)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_route_leases SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE id=$1`, lease)
	require.NoError(t, err)
	require.ErrorIs(t, r.RenewRoute(ctx, lease), service.ErrBorrowUnavailable, "a lost lease must not resurrect")
	fresh := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	chosen, err := fresh.ReserveRoute(ctx, id, "identity", "", "", uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, routes[0].ID, chosen.ID, "an orphan expires without waiting for cleanup")
	// Idle but shadow/revoked/short-lived routes are never preferred over a valid one.
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET validation_only=true WHERE id=$1`, routes[1].ID)
	require.NoError(t, err)
	_, err = r.ReserveRoute(ctx, id, "identity", routes[1].ID, "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	_, err = r.ObserveCookie(ctx, routes[2].CookieHash, time.Millisecond)
	require.NoError(t, err)
	chosen, err = r.ReserveRoute(ctx, id, "identity", "", "", uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, routes[0].ID, chosen.ID)
	_, err = r.ReserveRoute(ctx, id, "different-identity", "", "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
	require.NoError(t, r.Revoke(ctx, id, routes[0].ID, "changed"))
	_, err = r.ReserveRoute(ctx, id, "identity", "", "", uuid.NewString())
	require.ErrorIs(t, err, service.ErrBorrowUnavailable)
}

func TestTicketBorrowRouteLeaseCanOutliveAdmissionDeadline(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	ctx := t.Context()
	lease := uuid.NewString()
	_, err := r.ReserveRoute(ctx, id, "identity", routes[0].ID, "", lease)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET expires_at=clock_timestamp()-INTERVAL '1 second',inventory_eligible=false WHERE id=$1`, routes[0].ID)
	require.NoError(t, err)
	require.NoError(t, r.RenewRoute(ctx, lease), "an admitted stream is not cancelled by expiry")
	states, err := r.RouteStatuses(ctx, id, "identity")
	require.NoError(t, err)
	var found bool
	for _, state := range states {
		if state.ID == routes[0].ID {
			found = true
			require.False(t, state.Usable)
			require.Equal(t, 1, state.Inflight)
		}
	}
	require.True(t, found, "expired but active streams remain visible")
	require.NoError(t, r.ReleaseRoute(context.Background(), lease))
}
