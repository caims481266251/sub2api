package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestHostingSecurityOAuthSessionAllocationBounded(t *testing.T) {
	s, repo, _ := hostingTestService()
	s.oauth = NewOpenAIOAuthService(nil, nil)
	t.Cleanup(s.oauth.sessionStore.Stop)
	var wg sync.WaitGroup
	var accepted, limited, unexpected atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.StartOAuth(context.Background(), 7, 0)
			if err == nil {
				accepted.Add(1)
			} else if infraerrors.Code(err) == 429 {
				limited.Add(1)
			} else {
				unexpected.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 3, accepted.Load())
	require.EqualValues(t, 17, limited.Load())
	require.Zero(t, unexpected.Load())
	// Expired sessions are reclaimed in both the tenant binding and PKCE store.
	s.sessions.Range(func(k, v any) bool {
		session := v.(hostingOAuthSession)
		session.expiresAt = time.Now().Add(-time.Minute)
		s.sessions.Store(k, session)
		return true
	})
	_, err := s.StartOAuth(context.Background(), 7, 0)
	require.NoError(t, err)
	owner := int64(7)
	repo.accounts = []Account{{ID: 1, OwnerUserID: &owner}}
	_, err = s.StartOAuth(context.Background(), owner, 0)
	require.ErrorIs(t, err, ErrHostingSeatsFull)
	_, err = s.StartOAuth(context.Background(), owner, 1)
	require.NoError(t, err, "reauthorization must remain possible with full seats")
}

func TestHostingSecurityOAuthRejectsForeignCompletionAndState(t *testing.T) {
	s, _, _ := hostingTestService()
	s.oauth = NewOpenAIOAuthService(nil, nil)
	t.Cleanup(s.oauth.sessionStore.Stop)
	ctx := context.Background()
	start, err := s.StartOAuth(ctx, 7, 0)
	require.NoError(t, err)
	_, err = s.CompleteOAuth(ctx, 8, start.SessionID, "code", "wrong-state", "name")
	require.Error(t, err)
	_, exists := s.sessions.Load(start.SessionID)
	require.True(t, exists, "another user cannot consume the owner's session")
	_, err = s.CompleteOAuth(ctx, 7, start.SessionID, "code", "wrong-state", "name")
	require.Equal(t, "HOSTING_AUTH_INVALID", infraerrors.Reason(err))
	_, exists = s.sessions.Load(start.SessionID)
	require.False(t, exists)
	_, exists = s.oauth.sessionStore.Get(start.SessionID)
	require.False(t, exists, "failed exchanges must release PKCE state")
}

func TestHostingSecurityHTTPContinuationIsolation(t *testing.T) {
	gateway := &OpenAIGatewayService{}
	key := &APIKey{ID: 11, UserID: 7, RouteMode: RouteModeHosted}
	ctx := context.WithValue(context.Background(), hostingRequestContextKey{}, key)
	require.NoError(t, gateway.BindOpenAIHTTPResponseOwner(ctx, 0, "resp_victim", 7, 11))
	ok, err := gateway.ValidateOpenAIHTTPResponseOwner(ctx, 0, "resp_victim", 7, 11)
	require.NoError(t, err)
	require.True(t, ok)
	pool := int64(9)
	for _, other := range []*APIKey{
		{ID: 12, UserID: 8, RouteMode: RouteModeHosted},
		{ID: 12, UserID: 7, RouteMode: RouteModeHosted},
		{ID: 11, UserID: 7, RouteMode: RouteModeHosted, HostingPoolID: &pool},
		{ID: 11, UserID: 7, RouteMode: RouteModeHosted, HostingFallback: true},
	} {
		otherCtx := context.WithValue(context.Background(), hostingRequestContextKey{}, other)
		ok, err = gateway.ValidateOpenAIHTTPResponseOwner(otherCtx, 0, "resp_victim", other.UserID, other.ID)
		require.NoError(t, err)
		require.False(t, ok)
	}
}
