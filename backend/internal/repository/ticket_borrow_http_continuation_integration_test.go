//go:build integration

package repository

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var _ service.BorrowHTTPContinuationRepository = (*ticketBorrowRepository)(nil)

func TestBorrowHTTPContinuationLifecycleAndLineage(t *testing.T) {
	for _, invalidation := range []string{"expiry", "cookie_expiry", "revocation", "identity", "revision"} {
		t.Run(invalidation, func(t *testing.T) {
			r, id, routes := borrowRouteLoadFixture(t)
			ctx := t.Context()
			_, err := r.BindSession(ctx, id, "original-response", routes[0].ID)
			require.NoError(t, err)
			_, err = r.BindSession(ctx, id, "original-client-session", routes[0].ID)
			require.NoError(t, err)
			choice, err := r.ReserveHTTPContinuation(ctx, id, "identity", "original-response", "original-client-session", "fresh-1", "first-lease")
			require.NoError(t, err)
			require.Equal(t, routes[0].ID, choice.Route.ID)
			require.False(t, choice.Restarted)
			require.Empty(t, choice.SessionID)
			require.NoError(t, r.ReleaseRoute(ctx, "first-lease"))
			// Eligibility is deliberately before invalidation: final reservation
			// must handle expiry/revocation while waiting for the account slot.
			require.NoError(t, r.CheckHTTPContinuation(ctx, id, "identity", "original-response", "original-client-session"))
			switch invalidation {
			case "expiry":
				_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET expires_at=clock_timestamp() WHERE id=$1`, routes[0].ID)
			case "cookie_expiry":
				_, err = integrationDB.Exec(`INSERT INTO ticket_borrow_cookie_deadlines(cookie_hash,expires_at) VALUES($1,clock_timestamp())`, routes[0].CookieHash)
			case "revocation":
				_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET revoked_at=clock_timestamp() WHERE id=$1`, routes[0].ID)
			case "identity":
				_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET identity_hash='obsolete' WHERE id=$1`, routes[0].ID)
			case "revision":
				_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET target_revision=0 WHERE id=$1`, routes[0].ID)
			}
			require.NoError(t, err)
			// Load the longer-lived replacement: latest publication wins even with more inflight.
			_, err = r.ReserveRoute(ctx, id, "identity", routes[1].ID, "other-session", "other-lease")
			require.NoError(t, err)
			choice, err = r.ReserveHTTPContinuation(ctx, id, "identity", "original-response", "original-client-session", "fresh-1", "migrated-lease")
			require.NoError(t, err)
			require.Equal(t, routes[1].ID, choice.Route.ID)
			require.True(t, choice.Restarted)
			require.Equal(t, "fresh-1", choice.SessionID)
			require.NoError(t, r.BindHTTPContinuationResponse(ctx, id, "new-response", choice.Route.ID, choice.SessionID))
			// Even another app instance can resume the effective session while
			// the client continues to send its original session header.
			fresh := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
			next, err := fresh.ReserveHTTPContinuation(ctx, id, "identity", "new-response", "original-client-session", "unused-fresh-2", "next-lease")
			require.NoError(t, err)
			require.Equal(t, routes[1].ID, next.Route.ID)
			require.False(t, next.Restarted)
			require.Equal(t, "fresh-1", next.SessionID)
			bound, found, err := r.LookupSession(ctx, id, "original-client-session")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, routes[0].ID, bound)
			require.Error(t, r.BindHTTPContinuationResponse(ctx, id, "new-response", routes[2].ID, "cannot-overwrite"))
		})
	}
}

func TestBorrowHTTPContinuationNoGenericFallback(t *testing.T) {
	for _, block := range []string{"unknown", "disabled", "unhealthy", "no_inventory", "validation_only"} {
		t.Run(block, func(t *testing.T) {
			r, id, routes := borrowRouteLoadFixture(t)
			ctx := t.Context()
			_, err := r.BindSession(ctx, id, "response", routes[0].ID)
			require.NoError(t, err)
			response := "response"
			switch block {
			case "unknown":
				response = "missing"
			case "disabled":
				_, err = integrationDB.Exec(`UPDATE ticket_borrow_targets SET enabled=false WHERE account_id=$1`, id)
			case "unhealthy":
				_, err = integrationDB.Exec(`UPDATE accounts SET schedulable=false WHERE id=$1`, id)
			case "no_inventory":
				_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET expires_at=clock_timestamp() WHERE target_id=$1`, id)
			case "validation_only":
				_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET validation_only=true WHERE id=$1`, routes[0].ID)
			}
			require.NoError(t, err)
			choice, err := r.ReserveHTTPContinuation(ctx, id, "identity", response, "session", "fresh", uuid.NewString())
			require.Error(t, err)
			require.Nil(t, choice)
			var count int
			require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_route_leases WHERE target_id=$1`, id).Scan(&count))
			require.Zero(t, count)
		})
	}
}

func TestBorrowHTTPContinuationPrefersLatestWithOrdinaryReservations(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	ctx := t.Context()
	_, err := r.BindSession(ctx, id, "response", routes[0].ID)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET expires_at=clock_timestamp() WHERE id=$1`, routes[0].ID)
	require.NoError(t, err)
	fresh := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	var wg sync.WaitGroup
	results := make(chan error, 30)
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = fresh.ReserveHTTPContinuation(ctx, id, "identity", "response", "", uuid.NewString(), uuid.NewString())
			} else {
				_, err = r.ReserveLatestRoute(ctx, id, "identity", fmt.Sprintf("ordinary-%d", i), uuid.NewString(), uuid.NewString())
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	states, err := r.RouteStatuses(ctx, id, "identity")
	require.NoError(t, err)
	require.Len(t, states, 2)
	for _, state := range states {
		if state.ID == routes[1].ID {
			require.Equal(t, 30, state.Inflight)
		} else {
			require.Zero(t, state.Inflight)
		}
	}
}
