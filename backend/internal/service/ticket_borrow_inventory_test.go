package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBorrowInventoryTransitionsAndCadence(t *testing.T) {
	now := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	last := now.Add(-5 * time.Second)
	expiry := now.Add(230 * time.Second)
	p := &BorrowReplenishmentSettings{Enabled: true, Inventory: 2, IntervalSeconds: 10, HoldSeconds: 150}
	f := BorrowInventoryFacts{Now: now, LastStarted: &last, EarliestExpiry: &expiry, RenewBeforeSeconds: 15}
	for _, tc := range []struct {
		count int
		ttl   time.Duration
		state string
		due   time.Duration
	}{
		{0, 230 * time.Second, "search", 0}, {1, 230 * time.Second, "refill", 0},
		{2, 230 * time.Second, "healthy", 215 * time.Second}, {2, 17 * time.Second, "healthy", 2 * time.Second},
		{2, 10 * time.Second, "renew", 0},
	} {
		f.Count = tc.count
		e := now.Add(tc.ttl)
		f.EarliestExpiry = &e
		inv := EvaluateBorrowInventory(f, p)
		require.Equal(t, tc.state, inv.State)
		require.Equal(t, now.Add(tc.due), inv.NextAt)
	}
	f.Count = 1
	f.LastState = "healthy"
	require.Equal(t, now, EvaluateBorrowInventory(f, p).NextAt, "inventory loss bypasses healthy delay")
	f.LastState = "refill"
	f.RefreshGeneration = 2
	f.ProcessedGeneration = 1
	require.Equal(t, now, EvaluateBorrowInventory(f, p).NextAt, "unprocessed wakeup is immediate")
}

func TestTicketBorrowOuterAdmissionUsesRetiredSessionBinding(t *testing.T) {
	s, r := borrowFixture()
	route := r.route
	r.oldRoutes = map[string]*BorrowRoute{route.ID: route}
	r.route = nil
	header := http.Header{}
	header.Set("session_id", "old-conversation")
	r.sessions = map[string]string{borrowSessionKey(header): route.ID, borrowResponseKey("known-response"): route.ID}
	require.ErrorIs(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
	ctx := WithBorrowBusinessBinding(t.Context(), header, "")
	require.NoError(t, s.Check(ctx, 11, "gpt-6-astra", "http"))
	require.Empty(t, r.leases, "admission check is read-only")
	ctx = WithBorrowBusinessBinding(t.Context(), header, "unknown-response")
	require.ErrorIs(t, s.Check(ctx, 11, "gpt-6-astra", "http"), ErrBorrowUnavailable, "unknown response cannot fall back to known session")
	ctx = WithBorrowBusinessBinding(t.Context(), header, "known-response")
	require.NoError(t, s.Check(ctx, 11, "gpt-6-astra", "http"))
	r.sessions[borrowResponseKey("known-response")] = "another-route"
	require.ErrorIs(t, s.Check(ctx, 11, "gpt-6-astra", "http"), ErrBorrowUnavailable)
}
