package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestTicketBorrowMinimumRemainingStatusCheckAndForward(t *testing.T) {
	for _, remaining := range []time.Duration{121 * time.Second, 120 * time.Second, 119 * time.Second} {
		t.Run(remaining.String(), func(t *testing.T) {
			s, r := borrowFixture()
			r.minRemaining = 120 * time.Second
			r.route.ExpiresAt = r.now.Add(remaining)
			status, err := s.Status(t.Context(), 11)
			require.NoError(t, err)
			require.Equal(t, remaining > r.minRemaining, status.Ready)
			check := s.Check(t.Context(), 11, "gpt-6-astra", "http")
			calls := 0
			_, err = s.ForwardHTTP(borrowRequest(t), "", 11, 10, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
				require.NoError(t, BorrowBeforeSend(req))
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
			})
			if remaining > r.minRemaining {
				require.NoError(t, check)
				require.NoError(t, err)
				require.Equal(t, 1, calls)
				require.Equal(t, r.now.Add(time.Second), *status.UsableUntil)
			} else {
				require.ErrorIs(t, check, ErrBorrowUnavailable)
				require.ErrorIs(t, err, ErrBorrowUnavailable)
				require.Zero(t, calls)
			}
		})
	}
}

func TestTicketBorrowMinimumRemainingRecheckAndInFlightStream(t *testing.T) {
	s, r := borrowFixture()
	r.minRemaining = 120 * time.Second
	r.route.ExpiresAt = r.now.Add(121 * time.Second)
	_, err := s.ForwardHTTP(borrowRequest(t), "", 11, 10, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		// Simulate waiting for an account slot or connection across the boundary.
		r.now = r.now.Add(time.Second)
		return nil, BorrowBeforeSend(req)
	})
	require.ErrorIs(t, err, ErrBorrowUnavailable)
	r.route.ExpiresAt = r.now.Add(121 * time.Second)
	response, err := s.ForwardHTTP(borrowRequest(t), "", 11, 10, nil, func(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
		require.NoError(t, BorrowBeforeSend(req))
		r.now = r.now.Add(2 * time.Second)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("already-started-response"))}, nil
	})
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "already-started-response", string(body))
	require.Empty(t, r.revoked)
}

func TestTicketBorrowMinimumRemainingDoesNotRebindContinuation(t *testing.T) {
	s, r := borrowFixture()
	r.minRemaining = 120 * time.Second
	r.route.ExpiresAt = r.now.Add(200 * time.Second)
	old := *r.route
	old.ID, old.ExpiresAt = "old-bound-route", r.now.Add(119*time.Second)
	r.oldRoutes = map[string]*BorrowRoute{old.ID: &old}
	r.sessions = map[string]string{borrowResponseKey("resp_old"): old.ID}
	req, err := http.NewRequestWithContext(t.Context(), "POST", chatgptCodexURL, strings.NewReader(`{"model":"gpt-6-astra","previous_response_id":"resp_old"}`))
	require.NoError(t, err)
	require.NoError(t, s.Check(t.Context(), 11, "gpt-6-astra", "http"), "target has a fresh unbound route")
	_, err = s.ForwardHTTP(req, "", 11, 10, nil, func(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
		t.Fatal("a bound continuation must not be sent using the replacement")
		return nil, nil
	})
	require.ErrorIs(t, err, ErrBorrowUnavailable)
	require.Equal(t, old.ID, r.sessions[borrowResponseKey("resp_old")])
}
