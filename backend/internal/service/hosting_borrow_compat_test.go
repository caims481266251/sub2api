package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHostingBorrowHTTPContinuationUsesScopedResponseBinding(t *testing.T) {
	borrow, _ := borrowFixture()
	svc := &OpenAIGatewayService{accountRepo: borrow.owner.accountRepo, borrowRoutes: borrow}
	pool := int64(31)
	key := &APIKey{ID: 2, UserID: 1, RouteMode: RouteModeHosted, HostingPoolID: &pool, HostingFallback: true}
	ctx := context.WithValue(t.Context(), hostingRequestContextKey{}, key)
	account, err := svc.accountRepo.GetByID(ctx, 11)
	require.NoError(t, err)
	const response = "resp_hosting_fallback"
	require.NoError(t, svc.getOpenAIWSStateStoreForContext(ctx).BindResponseAccount(ctx, 0, response, 11, time.Hour))
	// A public binding with the same response ID must not override this key's account.
	require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, 0, response, 99, time.Hour))
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":"first"}]}`)
	ctx = svc.PrepareBorrowHTTPReplay(ctx, nil, key.UserID, key.ID, "", body)
	svc.observeBorrowHTTPReplayResponse(ctx, account, []byte(`{"id":"resp_hosting_fallback","status":"completed","output":[{"role":"assistant","content":"OK"}]}`), "")
	body = []byte(`{"model":"gpt-6-astra","previous_response_id":"resp_hosting_fallback","input":[{"role":"user","content":"next"}]}`)
	ctx = svc.PrepareBorrowHTTPReplay(ctx, nil, key.UserID, key.ID, response, body)
	nextCtx, effective, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, response, "gpt-6-astra", http.Header{}, body)
	require.NoError(t, err)
	require.True(t, converted)
	require.EqualValues(t, 11, borrowHTTPContinuationFromContext(nextCtx).TargetID)
	require.False(t, gjson.GetBytes(effective, "previous_response_id").Exists())
	require.Len(t, gjson.GetBytes(effective, "input").Array(), 3)

	for _, other := range []*APIKey{
		{ID: 2, UserID: 9, RouteMode: RouteModeHosted, HostingPoolID: &pool, HostingFallback: true},
		{ID: 3, UserID: 1, RouteMode: RouteModeHosted, HostingPoolID: &pool, HostingFallback: true},
		{ID: 2, UserID: 1, RouteMode: RouteModeHosted, HostingFallback: true},
		{ID: 2, UserID: 1, RouteMode: RouteModeHosted, HostingPoolID: &pool},
	} {
		otherCtx := context.WithValue(t.Context(), hostingRequestContextKey{}, other)
		otherCtx = svc.PrepareBorrowHTTPReplay(otherCtx, nil, other.UserID, other.ID, response, body)
		_, unchanged, converted, err := svc.PrepareBorrowHTTPContinuation(otherCtx, nil, response, "gpt-6-astra", http.Header{}, body)
		require.NoError(t, err)
		require.False(t, converted, "foreign user, key, pool or primary route cannot reuse fallback bindings")
		require.Equal(t, body, unchanged)
	}
}

func TestHostingBorrowHTTPContinuationRechecksAccountOwnership(t *testing.T) {
	borrow, _ := borrowFixture()
	owner := int64(9)
	account, err := borrow.owner.accountRepo.GetByID(t.Context(), 11)
	require.NoError(t, err)
	account.OwnerUserID = &owner
	svc := &OpenAIGatewayService{accountRepo: borrow.owner.accountRepo, borrowRoutes: borrow}
	key := &APIKey{ID: 2, UserID: 1, RouteMode: RouteModeHosted, HostingFallback: true}
	ctx := context.WithValue(t.Context(), hostingRequestContextKey{}, key)
	require.NoError(t, svc.getOpenAIWSStateStoreForContext(ctx).BindResponseAccount(ctx, 0, "resp_stale", account.ID, time.Hour))
	body := []byte(`{"model":"gpt-6-astra","previous_response_id":"resp_stale","input":"next"}`)
	_, unchanged, converted, err := svc.PrepareBorrowHTTPContinuation(ctx, nil, "resp_stale", "gpt-6-astra", http.Header{}, body)
	require.NoError(t, err)
	require.False(t, converted, "fallback must not prepare a private account even if a stale binding exists")
	require.Equal(t, body, unchanged)
}
