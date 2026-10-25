package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// Borrowing must leave upstream rate limits to the normal gateway policy.
// Repeated HTTP or stream errors must not add a route exclusion policy.
func TestBorrowRepeatedRateLimitsPreserveResponseAndRoute(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s, repo := borrowFixture()
			payload := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n"
			var inflight context.Context
			good, err := s.ForwardHTTP(borrowRequest(t), "", 11, 2, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.NoError(t, BorrowBeforeTransport(req))
				inflight = req.Context()
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(borrowCompletionPayload))}, nil
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = good.Body.Close() })
			for range 3 {
				resp, err := s.ForwardHTTP(borrowRequest(t), "", 11, 2, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
					require.NoError(t, BorrowBeforeTransport(req))
					return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
				})
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.Equal(t, payload, string(body))
				require.Equal(t, status, resp.StatusCode)
				require.Equal(t, "60", resp.Header.Get("Retry-After"))
				require.NoError(t, resp.Body.Close())
				require.NoError(t, s.Check(t.Context(), 11, "gpt-6-sol", "http"))
			}
			require.Empty(t, repo.revoked)
			require.NoError(t, inflight.Err())
			require.Equal(t, 1, activeBorrowLeases(repo))
			body, err := io.ReadAll(good.Body)
			require.NoError(t, err)
			require.Equal(t, borrowCompletionPayload, string(body))
			require.NoError(t, good.Body.Close())
			require.Zero(t, activeBorrowLeases(repo))
		})
	}
}

func TestBorrowRepeatedWebSocketRateLimitsDoNotExcludeRoute(t *testing.T) {
	s, repo := borrowFixture()
	route := repo.route
	lease := "ws-rate-limit-passthrough"
	_, err := repo.ReserveRoute(t.Context(), 11, route.IdentityHash, "", "", lease)
	require.NoError(t, err)
	b := &borrowWSUse{service: s, route: route, use: newBorrowRouteUse(t.Context(), repo, lease, time.Hour), bindings: map[string]<-chan struct{}{}}
	defer b.finish()
	for range 3 {
		require.NoError(t, s.observeWSHandshake(t.Context(), route, http.Header{}, http.StatusTooManyRequests))
		require.NoError(t, b.check(t.Context(), []byte(`{"type":"response.create"}`)))
		b.observe([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
	}
	require.Empty(t, repo.revoked)
	require.NoError(t, b.use.ctx.Err())
	require.NoError(t, b.check(t.Context(), []byte(`{"type":"response.create"}`)))
}
