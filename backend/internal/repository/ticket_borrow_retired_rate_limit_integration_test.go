//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBorrowRetiredRateLimitColumnsDoNotAffectAdmission(t *testing.T) {
	r, id, routes := borrowRouteLoadFixture(t)
	ctx := t.Context()
	original, err := r.Settings(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		current, err := r.Settings(context.Background())
		require.NoError(t, err)
		original.Revision = current.Revision
		require.NoError(t, r.SaveSettings(context.Background(), original))
	})
	// Retain migration 300 and its historical values on upgraded databases.
	_, err = integrationDB.Exec(`UPDATE ticket_borrow_routes SET rate_limit_count=100 WHERE target_id=$1`, id)
	require.NoError(t, err)
	require.NoError(t, r.SaveSettings(ctx, original))
	require.ErrorIs(t, r.SaveSettings(ctx, original), service.ErrBorrowConflict)
	settings, err := r.Settings(ctx)
	require.NoError(t, err)
	encoded, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "route_429_threshold")
	lease := uuid.NewString()
	route, err := r.ReserveLatestRoute(ctx, id, "identity", "session", "fresh", lease)
	require.NoError(t, err)
	require.Equal(t, routes[0].ID, route.Route.ID)
	require.NoError(t, r.RenewRoute(ctx, lease))
	states, err := r.RouteStatuses(ctx, id, "identity")
	require.NoError(t, err)
	require.Len(t, states, 3)
	encoded, err = json.Marshal(states)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "rate_limit_count")
	var count, exclusions int
	require.NoError(t, integrationDB.QueryRow(`SELECT rate_limit_count FROM ticket_borrow_routes WHERE id=$1`, route.Route.ID).Scan(&count))
	require.Equal(t, 100, count, "rollback must preserve historical data")
	require.NoError(t, integrationDB.QueryRow(`SELECT count(*) FROM ticket_borrow_events WHERE target_id=$1 AND reason='route_429_excluded'`, id).Scan(&exclusions))
	require.Zero(t, exclusions)
	require.NoError(t, r.ReleaseRoute(ctx, lease))
}
