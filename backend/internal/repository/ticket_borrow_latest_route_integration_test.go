//go:build integration

package repository

import (
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var _ service.BorrowLatestRouteRepository = (*ticketBorrowRepository)(nil)

func TestBorrowLatestRouteReplacesStillValidRoute(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=64 WHERE id=$1`, id)
	ctx := t.Context()
	first, err := r.ReserveLatestRoute(ctx, id, "identity", "client-session", "unused", "old-stream")
	require.NoError(t, err)
	require.Equal(t, routes[0].ID, first.Route.ID)
	require.False(t, first.Restarted)
	require.NoError(t, r.BindHTTPContinuationResponse(ctx, id, "old-response", first.Route.ID, ""))
	require.NoError(t, r.CheckHTTPContinuation(ctx, id, "identity", "old-response", "client-session"))
	// A newer publication appears during the account queue. It has less time
	// left and more active requests: neither expiry nor load can outrank recency.
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET checked_at=clock_timestamp() WHERE id=$1`, routes[1].ID)
	require.NoError(t, err)
	for range 3 {
		_, err = r.ReserveRoute(ctx, id, "identity", routes[1].ID, "", uuid.NewString())
		require.NoError(t, err)
	}
	newRequest, err := r.ReserveLatestRoute(ctx, id, "identity", "new-client", "unused", uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, routes[2].ID, newRequest.Route.ID, "new sessions prefer the unloaded route")
	require.Empty(t, newRequest.SessionID)
	continued, err := r.ReserveHTTPContinuation(ctx, id, "identity", "old-response", "client-session", "new-effective", uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, routes[1].ID, continued.Route.ID)
	require.True(t, continued.Restarted, "the original route is still usable")
	require.Equal(t, "new-effective", continued.SessionID)
	require.NoError(t, r.BindHTTPContinuationResponse(ctx, id, "new-response", continued.Route.ID, continued.SessionID))
	next, err := r.ReserveHTTPContinuation(ctx, id, "identity", "new-response", "client-session", "must-not-restart", uuid.NewString())
	require.NoError(t, err)
	require.False(t, next.Restarted)
	require.Equal(t, "new-effective", next.SessionID)
	bound, found, err := r.LookupSession(ctx, id, "old-response")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, routes[0].ID, bound, "historical responses remain immutable")
	require.NoError(t, r.RenewRoute(ctx, "old-stream"), "publication must not interrupt an in-flight request")
	var oldLeaseRoute string
	require.NoError(t, integrationDB.QueryRow(`SELECT route_id FROM ticket_borrow_route_leases WHERE id='old-stream'`).Scan(&oldLeaseRoute))
	require.Equal(t, routes[0].ID, oldLeaseRoute)
	statuses, err := r.RouteStatuses(ctx, id, "identity")
	require.NoError(t, err)
	require.Equal(t, routes[1].ID, statuses[0].ID, "UI uses the same newest-first ordering")
}

func TestBorrowLatestRouteSessionChangesPreserveResponseLineageAcrossInstances(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	rotationExec(t, `UPDATE accounts SET concurrency=64 WHERE id=$1`, id)
	ctx := t.Context()
	_, err := r.ReserveLatestRoute(ctx, id, "identity", "session", "first", uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, r.BindHTTPContinuationResponse(ctx, id, "original-response", routes[0].ID, ""))
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET checked_at=clock_timestamp() WHERE id=$1`, routes[2].ID)
	require.NoError(t, err)
	fresh := NewTicketBorrowRepository(integrationDB, r.encryptor).(*ticketBorrowRepository)
	var wg sync.WaitGroup
	choices := make(chan *service.BorrowContinuationRoute, 12)
	errors := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo := r
			if i%2 == 0 {
				repo = fresh
			}
			choice, err := repo.ReserveLatestRoute(ctx, id, "identity", "session", uuid.NewString(), uuid.NewString())
			errors <- err
			if err == nil {
				choices <- choice
			}
		}()
	}
	wg.Wait()
	close(errors)
	close(choices)
	for err := range errors {
		require.NoError(t, err)
	}
	used := map[string]int{}
	for choice := range choices {
		used[choice.Route.ID]++
		if choice.Restarted {
			require.NotEmpty(t, choice.SessionID)
		}
	}
	require.Len(t, used, 3, "session-only requests share capacity across normal routes")
	// The same client header now points to a new route, but an old response is
	// still a valid owner-checked request, not a false session-binding conflict.
	old, err := fresh.ReserveHTTPContinuation(ctx, id, "identity", "original-response", "session", "response-effective", uuid.NewString())
	require.NoError(t, err)
	require.Equal(t, routes[2].ID, old.Route.ID)
	require.True(t, old.Restarted)
}

func TestBorrowLatestRouteSkipsUnusableNewest(t *testing.T) {
	for _, state := range []string{"expired", "cookie_expired", "revoked", "identity", "revision", "validation_only", "retired"} {
		t.Run(state, func(t *testing.T) {
			r, id, routes := borrowRouteLoadFixture(t)
			rotationExec(t, `UPDATE accounts SET concurrency=64 WHERE id=$1`, id)
			ctx := t.Context()
			query := map[string]string{
				"expired":         `UPDATE ticket_borrow_routes SET expires_at=clock_timestamp() WHERE id=$1`,
				"revoked":         `UPDATE ticket_borrow_routes SET revoked_at=clock_timestamp() WHERE id=$1`,
				"identity":        `UPDATE ticket_borrow_routes SET identity_hash='obsolete' WHERE id=$1`,
				"revision":        `UPDATE ticket_borrow_routes SET target_revision=0 WHERE id=$1`,
				"validation_only": `UPDATE ticket_borrow_routes SET validation_only=true WHERE id=$1`,
				"retired":         `UPDATE ticket_borrow_routes SET inventory_eligible=false WHERE id=$1`,
			}[state]
			var err error
			if state == "cookie_expired" {
				_, err = integrationDB.Exec(`INSERT INTO ticket_borrow_cookie_deadlines(cookie_hash,expires_at) VALUES($1,clock_timestamp())`, routes[0].CookieHash)
			} else {
				_, err = integrationDB.Exec(query, routes[0].ID)
			}
			require.NoError(t, err)
			choice, err := r.ReserveLatestRoute(ctx, id, "identity", "", uuid.NewString(), uuid.NewString())
			require.NoError(t, err)
			require.Equal(t, routes[1].ID, choice.Route.ID)
		})
	}
}
