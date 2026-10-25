//go:build integration

package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var _ service.BorrowSchedulingRepository = (*ticketBorrowRepository)(nil)

func TestBorrowSchedulingTargetsAndUsableInventory(t *testing.T) {
	r, _, sources, acquisition := replenishmentFixture(t, false)
	ctx := service.WithBorrowAcquisition(t.Context(), acquisition)
	ids, err := r.EnabledTargetIDs(ctx)
	require.NoError(t, err)
	require.Contains(t, ids, acquisition.TargetID)
	first := replenishmentRoute(t, acquisition, sources[0], 230*time.Second)
	second := replenishmentRoute(t, acquisition, sources[0], 235*time.Second)
	require.NoError(t, r.Publish(ctx, first, acquisition.Lease))
	passBorrowFixtureQuality(t)
	needed, err := r.NeedsCandidate(ctx, acquisition, nil)
	require.NoError(t, err)
	require.True(t, needed, "one usable route cannot stop filling a two-route inventory")
	require.NoError(t, r.CheckAcquisition(ctx, acquisition))
	require.NoError(t, r.Publish(ctx, second, acquisition.Lease))
	passBorrowFixtureQuality(t)
	needed, err = r.NeedsCandidate(ctx, acquisition, nil)
	require.NoError(t, err)
	require.False(t, needed, "stop speculative acquisition once inventory is full")
	// A shortened Cookie deadline removes this route from new-request inventory
	// even though the route row itself still has a future expiry.
	_, err = r.ObserveCookie(ctx, first.CookieHash, -time.Second)
	require.NoError(t, err)
	inv, err := r.Inventory(ctx, acquisition.TargetID, acquisition.Identity)
	require.NoError(t, err)
	require.Equal(t, 1, inv.Count)
	require.NotNil(t, inv.EarliestExpiry)
	require.WithinDuration(t, second.AdmissionUntil, *inv.EarliestExpiry, time.Millisecond)
	needed, err = r.NeedsCandidate(ctx, acquisition, nil)
	require.NoError(t, err)
	require.True(t, needed)
	require.NoError(t, r.Publish(ctx, replenishmentRoute(t, acquisition, sources[0], 240*time.Second), acquisition.Lease))
	passBorrowFixtureQuality(t)
	inv, err = r.Inventory(ctx, acquisition.TargetID, acquisition.Identity)
	require.NoError(t, err)
	require.Equal(t, 2, inv.Count, "a verified replacement becomes business-usable immediately")
	target, err := r.Target(ctx, acquisition.TargetID)
	require.NoError(t, err)
	require.NoError(t, r.SetTarget(ctx, acquisition.TargetID, false, target.Revision))
	ids, err = r.EnabledTargetIDs(ctx)
	require.NoError(t, err)
	require.NotContains(t, ids, acquisition.TargetID)
}
